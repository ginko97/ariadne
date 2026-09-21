package tool

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Office documents are zip archives of XML, and a model cannot read either
// layer: fetched raw, a .docx is binary noise. The text is in a handful of
// known parts, so fetch unpacks those — Word, Excel and PowerPoint, and their
// OpenDocument (LibreOffice) counterparts — with archive/zip and encoding/xml.
//
// A document is still somebody else's text: the result is fenced as untrusted
// like any other fetch, and bounded like any other fetch. The bounds are three,
// because a zip has three sizes: the file on disk (maxDocumentBytes), each part
// once decompressed (maxPartBytes, which is what stops a zip bomb), and the text
// (maxDocumentText). What reaches the conversation is one maxFetchBytes part of
// that text at a time, as for a plain file, with a note saying which part it is
// and how to ask for the next — see textPart.

const (
	// maxDocumentText bounds the text fetch holds to serve a document in
	// parts. Far past a long book (a 500-page one is about 1 MB of text), and
	// short of what a machine notices.
	maxDocumentText = 16 << 20
	// maxDocumentBytes bounds the file itself. Compressed XML plus embedded
	// images; a document past this is mostly pictures, which yield no text.
	maxDocumentBytes = 50 << 20
	// maxPartBytes bounds one XML part once decompressed. A real 300-page
	// document.xml is a few megabytes; a zip bomb is gigabytes.
	maxPartBytes = 64 << 20
)

// documentKind names what a path's extension says it is, or "" for anything
// fetch reads as plain text.
func documentKind(p string) string {
	switch ext := strings.ToLower(path.Ext(strings.ReplaceAll(p, `\`, "/"))); ext {
	case ".docx", ".xlsx", ".pptx", ".odt", ".ods", ".odp", ".pdf":
		return ext
	case ".doc", ".xls", ".ppt":
		return "legacy"
	}
	return ""
}

// legacyMessage is the answer for the Office 97–2003 binary formats. They are
// not zip and XML, and reading them means a parser this project does not carry.
func legacyMessage(p string) string {
	return fmt.Sprintf("fetch: %q is an old binary Office format (.doc, .xls or .ppt), which "+
		"this tool cannot read. Saved as .docx, .xlsx or .pptx (File > Save As), it can be", p)
}

// errTextFull stops a walk once the text has reached its cap.
var errTextFull = errors.New("text cap reached")

// textBuf collects extracted text up to max bytes, then refuses more.
type textBuf struct {
	b         strings.Builder
	max       int
	truncated bool
}

func (t *textBuf) add(s string) error {
	if t.truncated {
		return errTextFull
	}
	if room := t.max - t.b.Len(); len(s) > room {
		// Cut on a rune boundary: half a UTF-8 sequence is not text.
		cut := room
		for cut > 0 && cut < len(s) && s[cut]&0xC0 == 0x80 {
			cut--
		}
		t.b.WriteString(s[:cut])
		t.truncated = true
		return errTextFull
	}
	t.b.WriteString(s)
	return nil
}

func (t *textBuf) String() string {
	s := strings.TrimRight(t.b.String(), " \t\n")
	if t.truncated {
		s += fmt.Sprintf("\n\n[truncated: the document's text is longer than %d KB; this is the first %d KB]",
			t.max>>10, t.max>>10)
	}
	return s
}

// textPart returns part n (1-based) of text, maxFetchBytes at most, and how
// many parts there are. Parts are cut on rune boundaries and are the same on
// every call, so part 2 always starts where part 1 ended.
//
// Parts exist because one message cannot hold a book: a single result is
// resent with every later request, and one 20 MB read once made a checkpoint
// no provider would accept. Before parts, the text past the first 256 KB was
// simply not available; a model asked to summarise a book read the first 112
// pages twice, got the same 112 pages both times, and summarised the rest
// from the table of contents without saying so.
func textPart(text string, n int) (string, int) {
	var starts []int
	for start := 0; start < len(text); {
		starts = append(starts, start)
		end := start + maxFetchBytes
		if end >= len(text) {
			break
		}
		for end > start && !utf8.RuneStart(text[end]) {
			end--
		}
		start = end
	}
	if len(starts) == 0 {
		return "", 1
	}
	if n < 1 || n > len(starts) {
		return "", len(starts)
	}
	end := len(text)
	if n < len(starts) {
		end = starts[n]
	}
	return text[starts[n-1]:end], len(starts)
}

// withPartNote serves part n of text, saying which part it is when there is
// more than one.
func withPartNote(text string, n int) (string, error) {
	body, parts := textPart(text, n)
	switch {
	case n < 1 || n > parts:
		return "", fmt.Errorf("it has %d part(s) of text; ask for part 1 to %d", parts, parts)
	case parts == 1:
		return body, nil
	case n < parts:
		return fmt.Sprintf("%s\n\n[part %d of %d of this document's text. Fetch it again with part=%d to read on. "+
			"Every part read stays in the conversation, so read only the parts you need.]", body, n, parts, n+1), nil
	default:
		return fmt.Sprintf("%s\n\n[part %d of %d: the end of this document's text.]", body, n, parts), nil
	}
}

// readDocument returns the text of an Office or OpenDocument file.
func readDocument(f io.ReaderAt, size int64, kind string) (string, error) {
	zr, err := zip.NewReader(f, size)
	if err != nil {
		return "", fmt.Errorf("not a valid %s file: %v", kind, err)
	}
	out := &textBuf{max: maxDocumentText}
	switch kind {
	case ".docx":
		err = docxText(zr, out)
	case ".pptx":
		err = pptxText(zr, out)
	case ".xlsx":
		err = xlsxText(zr, out)
	case ".odt", ".ods", ".odp":
		err = odfText(zr, out)
	default:
		return "", fmt.Errorf("unknown document kind %q", kind)
	}
	if err != nil && !errors.Is(err, errTextFull) {
		return "", err
	}
	return out.String(), nil
}

// part opens one member of the archive, bounded once decompressed.
func part(zr *zip.Reader, name string) (*xml.Decoder, func(), error) {
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		if f.UncompressedSize64 > maxPartBytes {
			return nil, nil, fmt.Errorf("%s is %d bytes uncompressed, over the %d-byte limit", name, f.UncompressedSize64, maxPartBytes)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %v", name, err)
		}
		// The header's size is the archive's claim; the limit is what holds.
		d := xml.NewDecoder(io.LimitReader(rc, maxPartBytes))
		d.Strict = false
		return d, func() { rc.Close() }, nil
	}
	return nil, nil, fmt.Errorf("%s is missing; the file is not a complete document", name)
}

// hasPart says whether the archive holds name.
func hasPart(zr *zip.Reader, name string) bool {
	for _, f := range zr.File {
		if f.Name == name {
			return true
		}
	}
	return false
}

func attr(se xml.StartElement, local string) string {
	for _, a := range se.Attr {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

// ---------------------------------------------------------------- Word

func docxText(zr *zip.Reader, out *textBuf) error {
	d, done, err := part(zr, "word/document.xml")
	if err != nil {
		return err
	}
	defer done()

	// Inside a table a paragraph ends a line of the cell, not of the document;
	// cells are separated by tabs and rows by newlines, so a table reads as
	// rows.
	// Tabs and breaks count only inside a run: w:tab is also the name of a
	// tab-stop definition in paragraph properties, which is layout, not text.
	inText, inRun, cellDepth, skip := false, 0, 0, 0
	var (
		cell strings.Builder
		row  []string
	)
	// write sends text to the current cell inside a table, and to the
	// document otherwise. A table nested in a cell joins that cell's text.
	write := func(s string) error {
		if cellDepth > 0 {
			cell.WriteString(s)
			return nil
		}
		return out.add(s)
	}
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("word/document.xml: %v", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "delText", "instrText":
				// Deleted tracked changes and field codes are not what the
				// document says.
				skip++
			case "r":
				inRun++
			case "t":
				inText = true
			case "tab":
				if inRun > 0 {
					err = write("\t")
				}
			case "br", "cr":
				if inRun > 0 {
					err = write("\n")
				}
			case "tc":
				if cellDepth == 0 {
					cell.Reset()
				}
				cellDepth++
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "delText", "instrText":
				skip--
			case "r":
				inRun--
			case "t":
				inText = false
			case "p":
				if cellDepth > 0 {
					cell.WriteString(" ")
				} else {
					err = out.add("\n")
				}
			case "tc":
				cellDepth--
				if cellDepth == 0 {
					row = append(row, cellText(strings.TrimSpace(cell.String())))
				} else {
					cell.WriteString(" ")
				}
			case "tr":
				if cellDepth == 0 {
					err = out.add(strings.Join(row, "\t") + "\n")
					row = row[:0]
				}
			}
		case xml.CharData:
			if inText && skip == 0 {
				err = write(string(t))
			}
		}
		if err != nil {
			return err
		}
	}
}

// ---------------------------------------------------------------- PowerPoint

func pptxText(zr *zip.Reader, out *textBuf) error {
	// Slides are slide1.xml, slide2.xml, ...; sorted by number, since slide10
	// sorts before slide2 as a string. (The presentation's own order lives in
	// presentation.xml; the file numbers match it unless slides were
	// reordered, and then the text is still all there.)
	type slide struct {
		n    int
		name string
	}
	var slides []slide
	for _, f := range zr.File {
		rest, ok := strings.CutPrefix(f.Name, "ppt/slides/slide")
		if !ok || !strings.HasSuffix(rest, ".xml") {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSuffix(rest, ".xml")); err == nil {
			slides = append(slides, slide{n, f.Name})
		}
	}
	if len(slides) == 0 {
		return errors.New("no slides found; the file is not a complete presentation")
	}
	sort.Slice(slides, func(i, j int) bool { return slides[i].n < slides[j].n })

	for i, s := range slides {
		if err := out.add(fmt.Sprintf("--- slide %d ---\n", i+1)); err != nil {
			return err
		}
		if err := drawingMLText(zr, s.name, out); err != nil {
			return err
		}
		if err := out.add("\n"); err != nil {
			return err
		}
	}
	return nil
}

// drawingMLText writes the a:t runs of one slide, a line per paragraph.
func drawingMLText(zr *zip.Reader, name string, out *textBuf) error {
	d, done, err := part(zr, name)
	if err != nil {
		return err
	}
	defer done()
	inText := false
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %v", name, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "br":
				err = out.add("\n")
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				err = out.add("\n")
			}
		case xml.CharData:
			if inText {
				err = out.add(string(t))
			}
		}
		if err != nil {
			return err
		}
	}
}

// ---------------------------------------------------------------- Excel

// xlsxText writes each sheet as a heading and tab-separated rows.
//
// A cell's text is not always in the cell: most strings are an index into
// sharedStrings.xml, and a date is a plain number whose meaning is in the
// cell's style. Both are resolved, so the model reads "2026-03-01" rather than
// "46082" — a number it would otherwise report as a number, confidently.
func xlsxText(zr *zip.Reader, out *textBuf) error {
	sheets, date1904, err := xlsxSheets(zr)
	if err != nil {
		return err
	}
	shared, err := xlsxSharedStrings(zr)
	if err != nil {
		return err
	}
	dateStyles, err := xlsxDateStyles(zr)
	if err != nil {
		return err
	}
	for _, sh := range sheets {
		if err := out.add("## " + sh.name + "\n"); err != nil {
			return err
		}
		if err := xlsxSheet(zr, sh.path, shared, dateStyles, date1904, out); err != nil {
			return err
		}
		if err := out.add("\n"); err != nil {
			return err
		}
	}
	return nil
}

type xlsxSheetRef struct{ name, path string }

// xlsxSheets reads the sheet names, in workbook order, and where each lives.
func xlsxSheets(zr *zip.Reader) ([]xlsxSheetRef, bool, error) {
	targets := map[string]string{}
	if hasPart(zr, "xl/_rels/workbook.xml.rels") {
		d, done, err := part(zr, "xl/_rels/workbook.xml.rels")
		if err != nil {
			return nil, false, err
		}
		for {
			tok, err := d.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				done()
				return nil, false, fmt.Errorf("workbook.xml.rels: %v", err)
			}
			if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "Relationship" {
				t := attr(se, "Target")
				// Targets are relative to xl/, or absolute from the package root.
				if strings.HasPrefix(t, "/") {
					t = strings.TrimPrefix(t, "/")
				} else if !strings.HasPrefix(t, "xl/") {
					t = path.Join("xl", t)
				}
				targets[attr(se, "Id")] = t
			}
		}
		done()
	}

	d, done, err := part(zr, "xl/workbook.xml")
	if err != nil {
		return nil, false, err
	}
	defer done()
	var sheets []xlsxSheetRef
	date1904 := false
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false, fmt.Errorf("workbook.xml: %v", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "workbookPr":
			v := attr(se, "date1904")
			date1904 = v == "1" || v == "true"
		case "sheet":
			p := targets[attr(se, "id")]
			if p == "" {
				// No relationships part: the conventional name.
				p = fmt.Sprintf("xl/worksheets/sheet%d.xml", len(sheets)+1)
			}
			sheets = append(sheets, xlsxSheetRef{name: attr(se, "name"), path: p})
		}
	}
	if len(sheets) == 0 {
		return nil, false, errors.New("no sheets found; the file is not a complete workbook")
	}
	return sheets, date1904, nil
}

// xlsxSharedStrings returns the workbook's string table. Rich text is several
// runs per string; phonetic guides (rPh) are not part of the text.
func xlsxSharedStrings(zr *zip.Reader) ([]string, error) {
	if !hasPart(zr, "xl/sharedStrings.xml") {
		return nil, nil
	}
	d, done, err := part(zr, "xl/sharedStrings.xml")
	if err != nil {
		return nil, err
	}
	defer done()
	var (
		strs     []string
		cur      strings.Builder
		inT      bool
		phonetic int
		total    int
	)
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return strs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("sharedStrings.xml: %v", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				cur.Reset()
			case "t":
				inT = true
			case "rPh":
				phonetic++
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "si":
				strs = append(strs, cur.String())
			case "t":
				inT = false
			case "rPh":
				phonetic--
			}
		case xml.CharData:
			if inT && phonetic == 0 {
				// The table is held in memory; bound it like the text it feeds.
				if total += len(t); total > maxPartBytes {
					return nil, errors.New("sharedStrings.xml holds more text than this tool will read")
				}
				cur.Write(t)
			}
		}
	}
}

// xlsxDateStyles returns which cell style indexes format a number as a date or
// time.
func xlsxDateStyles(zr *zip.Reader) (map[int]bool, error) {
	styles := map[int]bool{}
	if !hasPart(zr, "xl/styles.xml") {
		return styles, nil
	}
	d, done, err := part(zr, "xl/styles.xml")
	if err != nil {
		return nil, err
	}
	defer done()
	custom := map[int]bool{}
	inCellXfs, xf := false, 0
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return styles, nil
		}
		if err != nil {
			return nil, fmt.Errorf("styles.xml: %v", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "numFmt":
				if id, err := strconv.Atoi(attr(t, "numFmtId")); err == nil {
					custom[id] = isDateFormat(attr(t, "formatCode"))
				}
			case "cellXfs":
				inCellXfs = true
			case "xf":
				if inCellXfs {
					id, _ := strconv.Atoi(attr(t, "numFmtId"))
					date, isCustom := custom[id]
					if !isCustom {
						date = builtinDateFormat(id)
					}
					if date {
						styles[xf] = true
					}
					xf++
				}
			}
		case xml.EndElement:
			if t.Name.Local == "cellXfs" {
				inCellXfs = false
			}
		}
	}
}

// builtinDateFormat reports the built-in number formats that are dates or
// times (ECMA-376 §18.8.30).
func builtinDateFormat(id int) bool {
	return (id >= 14 && id <= 22) || (id >= 45 && id <= 47)
}

// isDateFormat reports whether a custom number format shows a date or time:
// it uses y, m, d, h or s outside quoted text, escapes and [brackets]. A
// bracket holds a colour ([Magenta]), a locale ([$-409]) or a condition — or
// elapsed time ([h], [mm], [ss]), which is still a time.
func isDateFormat(code string) bool {
	code = strings.ToLower(code)
	inQuote := false
	for i := 0; i < len(code); i++ {
		c := code[i]
		switch {
		case inQuote:
			inQuote = c != '"'
		case c == '"':
			inQuote = true
		case c == '[':
			end := strings.IndexByte(code[i:], ']')
			if end < 0 {
				return false
			}
			inner := code[i+1 : i+end]
			if inner != "" && (strings.Trim(inner, "h") == "" || strings.Trim(inner, "m") == "" || strings.Trim(inner, "s") == "") {
				return true
			}
			i += end
		case c == '\\' || c == '_' || c == '*':
			i++ // the next character is literal, or padding
		case c == 'y' || c == 'm' || c == 'd' || c == 'h' || c == 's':
			return true
		}
	}
	return false
}

// excelDate turns a date serial into text. Excel's 1900 system counts from
// 1899-12-30 (the day 1900-02-29, which never happened, shifts it by one), the
// 1904 system from 1904-01-01.
func excelDate(serial float64, date1904 bool) string {
	base := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)
	if date1904 {
		base = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	days := math.Floor(serial)
	secs := math.Round((serial - days) * 86400)
	if secs >= 86400 {
		days++
		secs = 0
	}
	t := base.AddDate(0, 0, int(days)).Add(time.Duration(secs) * time.Second)
	switch {
	case serial < 1 && !date1904:
		return t.Format("15:04:05")
	case secs == 0:
		return t.Format("2006-01-02")
	default:
		return t.Format("2006-01-02 15:04:05")
	}
}

// columnIndex turns the letters of a cell reference ("C7") into a 0-based
// column, or -1. A valid reference must have 1-3 ASCII letters followed by
// 1 or more ASCII digits and nothing else.
func columnIndex(ref string) int {
	col := 0
	n := 0
	for _, c := range ref {
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		if c < 'A' || c > 'Z' {
			break
		}
		col = col*26 + int(c-'A'+1)
		n++
	}
	if n == 0 || n > 3 || n == len(ref) {
		return -1
	}
	for _, c := range ref[n:] {
		if c < '0' || c > '9' {
			return -1
		}
	}
	return col - 1
}

func xlsxSheet(zr *zip.Reader, name string, shared []string, dateStyles map[int]bool, date1904 bool, out *textBuf) error {
	d, done, err := part(zr, name)
	if err != nil {
		return err
	}
	defer done()

	var (
		row                []string
		cellCol, cellStyle int
		cellType           string
		value              strings.Builder
		inValue, inInline  bool
		blankRows          int
	)
	flushRow := func() error {
		for len(row) > 0 && row[len(row)-1] == "" {
			row = row[:len(row)-1]
		}
		if len(row) == 0 {
			// Blank rows are held back, so trailing ones are never written.
			blankRows++
			return nil
		}
		for ; blankRows > 0; blankRows-- {
			if err := out.add("\n"); err != nil {
				return err
			}
		}
		line := strings.Join(row, "\t") + "\n"
		row = row[:0]
		return out.add(line)
	}
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %v", name, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				row = row[:0]
			case "c":
				cellCol = columnIndex(attr(t, "r"))
				if cellCol < 0 {
					cellCol = len(row)
				}
				cellType = attr(t, "t")
				cellStyle, _ = strconv.Atoi(attr(t, "s"))
				value.Reset()
			case "v":
				inValue = true
			case "is":
				inInline = true
			case "t":
				if inInline {
					inValue = true
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v":
				inValue = false
			case "t":
				if inInline {
					inValue = false
				}
			case "is":
				inInline = false
			case "c":
				text := xlsxCell(value.String(), cellType, cellStyle, shared, dateStyles, date1904)
				for len(row) < cellCol {
					row = append(row, "")
				}
				if cellCol < len(row) {
					row[cellCol] = text
				} else {
					row = append(row, text)
				}
			case "row":
				if err := flushRow(); err != nil {
					return err
				}
			}
		case xml.CharData:
			if inValue {
				value.Write(t)
			}
		}
	}
}

func xlsxCell(v, typ string, style int, shared []string, dateStyles map[int]bool, date1904 bool) string {
	switch typ {
	case "s":
		i, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || i < 0 || i >= len(shared) {
			return ""
		}
		return cellText(shared[i])
	case "b":
		val := strings.TrimSpace(v)
		if val == "1" || strings.EqualFold(val, "true") {
			return "TRUE"
		}
		return "FALSE"
	case "str", "inlineStr", "e":
		return cellText(v)
	}
	if dateStyles[style] {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f >= 0 {
			return excelDate(f, date1904)
		}
	}
	return strings.TrimSpace(v)
}

// cellText keeps a cell on its line: a tab or newline inside a cell would
// read as a new column or row.
func cellText(s string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ").Replace(s)
}

// ---------------------------------------------------------------- OpenDocument

// maxRepeat bounds ODF's run-length encoding. A spreadsheet row commonly ends
// with one empty cell repeated to column 16384, and a sheet with one empty
// row repeated a million times; expanded, those are the zip bomb.
const maxRepeat = 256

// odfText reads content.xml, which holds the text of all three OpenDocument
// kinds: paragraphs and headings, tables (every spreadsheet is one per
// sheet), and presentation pages.
func odfText(zr *zip.Reader, out *textBuf) error {
	d, done, err := part(zr, "content.xml")
	if err != nil {
		return err
	}
	defer done()

	var (
		spreadsheet bool
		pages       int
		// Table state: the current row's cells, the current cell's text, and
		// how many times each repeats.
		cellDepth             int
		row                   []string
		cell                  strings.Builder
		cellRepeat, rowRepeat int
		cellDate              string
		blankRows             int
		skip                  int
		// Text lives in paragraphs and headings; whitespace between other
		// elements is the file's indentation.
		textDepth int
	)
	write := func(s string) error {
		if skip > 0 || textDepth == 0 {
			return nil
		}
		if cellDepth > 0 {
			cell.WriteString(s)
			return nil
		}
		return out.add(s)
	}
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("content.xml: %v", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "spreadsheet":
				spreadsheet = true
			case "p", "h":
				textDepth++
			case "page":
				pages++
				err = out.add(fmt.Sprintf("--- slide %d ---\n", pages))
			case "table":
				if spreadsheet && cellDepth == 0 {
					err = out.add("## " + attr(t, "name") + "\n")
				}
				blankRows = 0
			case "table-row":
				row = row[:0]
				rowRepeat = repeat(attr(t, "number-rows-repeated"))
			case "table-cell", "covered-table-cell":
				cellDepth++
				cell.Reset()
				cellRepeat = repeat(attr(t, "number-columns-repeated"))
				// A date cell shows its value in the locale it was saved in
				// (3/1/2026: March or January?); the attribute is ISO 8601.
				cellDate = ""
				if attr(t, "value-type") == "date" {
					cellDate = strings.Replace(strings.TrimSuffix(attr(t, "date-value"), "T00:00:00"), "T", " ", 1)
				}
			case "s":
				n := repeat(attr(t, "c"))
				err = write(strings.Repeat(" ", n))
			case "tab":
				err = write("\t")
			case "line-break":
				err = write("\n")
			case "annotation", "tracked-changes":
				// Comments and the record of deleted text are not the document.
				skip++
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "p", "h":
				textDepth--
				if skip > 0 {
					break
				}
				if cellDepth > 0 {
					cell.WriteString(" ")
				} else {
					err = out.add("\n")
				}
			case "table-cell", "covered-table-cell":
				cellDepth--
				text := cellText(strings.TrimSpace(cell.String()))
				if cellDate != "" {
					text = cellDate
				}
				for i := 0; i < cellRepeat; i++ {
					row = append(row, text)
				}
			case "table-row":
				for len(row) > 0 && row[len(row)-1] == "" {
					row = row[:len(row)-1]
				}
				if len(row) == 0 {
					blankRows += rowRepeat
					break
				}
				for ; blankRows > 0; blankRows-- {
					if err = out.add("\n"); err != nil {
						return err
					}
				}
				line := strings.Join(row, "\t") + "\n"
				for i := 0; i < rowRepeat && err == nil; i++ {
					err = out.add(line)
				}
			case "table":
				err = out.add("\n")
			case "annotation", "tracked-changes":
				skip--
			}
		case xml.CharData:
			err = write(string(t))
		}
		if err != nil {
			return err
		}
	}
}

// repeat reads an ODF repeat count, 1 when absent, bounded by maxRepeat.
func repeat(v string) int {
	n, err := strconv.Atoi(v)
	switch {
	case err != nil || n < 1:
		return 1
	case n > maxRepeat:
		return maxRepeat
	}
	return n
}

// ---------------------------------------------------------------- PDF

// maxPDFBytes bounds a PDF handed to pdftotext. Larger than the Office cap:
// a PDF carries its fonts and images, so 20 MB of PDF is often a short text.
const maxPDFBytes = 100 << 20

// pdfTimeout bounds one conversion. A variable for the test that needs to
// tell stopping early from waiting it out.
var pdfTimeout = time.Minute

// pdfCommand builds the pdftotext invocation. A variable so tests can stand in
// for a program that may not be installed.
var pdfCommand = func(ctx context.Context, file string) *exec.Cmd {
	return exec.CommandContext(ctx, "pdftotext", "-enc", "UTF-8", "-layout", file, "-")
}

// errNoPdftotext is the answer when pdftotext is not installed.
var errNoPdftotext = errors.New("reading a PDF needs pdftotext, which is not installed. " +
	"It is part of Poppler: `winget install poppler` or `choco install poppler` on Windows, " +
	"`brew install poppler` on macOS, `apt install poppler-utils` on Debian and Ubuntu")

// readPDF runs pdftotext on a copy of the file.
//
// A copy, because pdftotext opens a path and os.Root cannot confine another
// program: handed the workspace path, it would follow a symlink or junction
// out of the tree that fetch itself refused to follow. The copy is read
// through the root, so what pdftotext sees is exactly what fetch was allowed
// to read.
func readPDF(ctx context.Context, f io.Reader, size int64) (string, error) {
	if size > maxPDFBytes {
		return "", fmt.Errorf("the PDF is %d bytes, over the %d-byte limit", size, maxPDFBytes)
	}
	tmp, err := os.CreateTemp("", "ariadne-*.pdf")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, io.LimitReader(f, maxPDFBytes)); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}

	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, pdfTimeout)
	defer cancel()
	cmd := pdfCommand(ctx, tmp.Name())
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, n: 4 << 10}
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", errNoPdftotext
		}
		return "", fmt.Errorf("pdftotext: %v", err)
	}
	// A character can straddle two reads, and nothing here holds the tail
	// back until the rest arrives: converting bytes to a string keeps the
	// bytes as they are, so the halves meet again in the buffer and the text
	// is the same as if it had arrived in one piece. Only the final cut has
	// to know about characters, and textBuf.add does that.
	// TestFetchPDFUtf8RuneSplit guards it: a rune split across reads comes
	// back whole, with no replacement character.
	out := &textBuf{max: maxDocumentText}
	buf := make([]byte, 32<<10)
	for {
		n, rerr := stdout.Read(buf)
		if n > 0 && out.add(string(buf[:n])) != nil {
			// Enough text: stop the conversion rather than wait for the rest.
			cancel()
			_, _ = io.Copy(io.Discard, stdout)
			break
		}
		if rerr != nil {
			break
		}
	}
	werr := cmd.Wait()
	switch {
	case parent.Err() != nil:
		// The turn was stopped: say that, not that pdftotext was slow.
		return "", parent.Err()
	case out.truncated:
	case ctx.Err() != nil:
		return "", fmt.Errorf("pdftotext: gave up after %s", pdfTimeout)
	case werr != nil:
		return "", fmt.Errorf("pdftotext: %v: %s", werr, strings.TrimSpace(stderr.String()))
	}
	text := pdfPages(out.String())
	if strings.TrimSpace(text) == "" {
		return "", errors.New("the PDF has no text layer — it is probably scanned images, which need OCR")
	}
	return text, nil
}

// pdfPages turns pdftotext's output into the same shape as the other
// documents: \n line endings (it writes \r\n on Windows), and a heading per
// page where it writes a form feed, so a model can say which page it read.
func pdfPages(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	pages := strings.Split(s, "\f")
	for len(pages) > 1 && strings.TrimSpace(pages[len(pages)-1]) == "" {
		pages = pages[:len(pages)-1]
	}
	if len(pages) == 1 {
		return strings.TrimSpace(pages[0])
	}
	var b strings.Builder
	for i, p := range pages {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "--- page %d ---\n%s", i+1, strings.Trim(p, "\n"))
	}
	return b.String()
}

// limitedWriter keeps the first n bytes and drops the rest without failing,
// so a noisy program is not killed by a full pipe.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		_, _ = l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}
