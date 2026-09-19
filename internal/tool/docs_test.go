package tool

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// writeZip writes a zip archive of parts to dir/name, in the given order.
func writeZip(t *testing.T, dir, name string, parts ...[2]string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, p := range parts {
		w, err := zw.Create(p[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(p[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fetchDoc fetches name from dir and fails the test on an error result.
func fetchDoc(t *testing.T, dir, name string) string {
	t.Helper()
	res := fetchResult(t, dir, name)
	if res.IsError {
		t.Fatalf("fetch %s: %s", name, res.Content)
	}
	if !res.Untrusted {
		t.Errorf("fetch %s: a document's text must be marked untrusted", name)
	}
	return res.Content
}

func fetchResult(t *testing.T, dir, name string) struct {
	Content            string
	IsError, Untrusted bool
} {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"path": name})
	res, err := NewFetch(dir).Call(context.Background(), "c1", args)
	if err != nil {
		t.Fatal(err)
	}
	return struct {
		Content            string
		IsError, Untrusted bool
	}{res.Content, res.IsError, res.Untrusted}
}

const wordNS = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"`

func TestFetchReadsDocx(t *testing.T) {
	dir := t.TempDir()
	doc := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document ` + wordNS + `>
  <w:body>
    <w:p>
      <w:pPr><w:tabs><w:tab w:val="left" w:pos="720"/></w:tabs></w:pPr>
      <w:r><w:t>Invoice</w:t></w:r><w:r><w:tab/><w:t xml:space="preserve">No. 42</w:t></w:r>
    </w:p>
    <w:p>
      <w:r><w:t xml:space="preserve">Total due: </w:t></w:r>
      <w:del><w:r><w:delText>$10</w:delText></w:r></w:del>
      <w:r><w:fldChar w:fldCharType="begin"/></w:r><w:r><w:instrText>PAGE</w:instrText></w:r>
      <w:r><w:t>$12</w:t></w:r>
    </w:p>
    <w:tbl>
      <w:tr><w:tc><w:p><w:r><w:t>Item</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>Price</w:t></w:r></w:p></w:tc></w:tr>
      <w:tr><w:tc><w:p><w:r><w:t>Tea</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>3</w:t></w:r></w:p></w:tc></w:tr>
    </w:tbl>
    <w:p><w:r><w:t>Thanks</w:t></w:r><w:r><w:br/><w:t>Ginko</w:t></w:r></w:p>
  </w:body>
</w:document>`
	writeZip(t, dir, "invoice.DOCX", [2]string{"word/document.xml", doc})

	got := fetchDoc(t, dir, "invoice.DOCX")
	want := "Invoice\tNo. 42\nTotal due: $12\nItem\tPrice\nTea\t3\nThanks\nGinko"
	if got != want {
		t.Errorf("docx text:\n got %q\nwant %q", got, want)
	}
}

func TestFetchReadsPptxInSlideOrder(t *testing.T) {
	dir := t.TempDir()
	slide := func(lines ...string) string {
		var b strings.Builder
		b.WriteString(`<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><p:cSld><p:spTree><p:sp><p:txBody>`)
		for _, l := range lines {
			fmt.Fprintf(&b, `<a:p><a:r><a:t>%s</a:t></a:r></a:p>`, l)
		}
		b.WriteString(`</p:txBody></p:sp></p:spTree></p:cSld></p:sld>`)
		return b.String()
	}
	// Stored out of order, and slide10 sorts before slide2 as a string.
	writeZip(t, dir, "deck.pptx",
		[2]string{"ppt/slides/slide10.xml", slide("Ten")},
		[2]string{"ppt/slides/slide2.xml", slide("Two", "second line")},
		[2]string{"ppt/slides/slide1.xml", slide("One")},
		[2]string{"ppt/slides/_rels/slide1.xml.rels", `<Relationships/>`},
	)
	got := fetchDoc(t, dir, "deck.pptx")
	want := "--- slide 1 ---\nOne\n\n--- slide 2 ---\nTwo\nsecond line\n\n--- slide 3 ---\nTen"
	if got != want {
		t.Errorf("pptx text:\n got %q\nwant %q", got, want)
	}
}

// A workbook: sheets named and ordered by workbook.xml through the
// relationships, strings from the shared table, and dates as dates.
func TestFetchReadsXlsx(t *testing.T) {
	dir := t.TempDir()
	workbook := `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
  <workbookPr/>
  <sheets><sheet name="Budget" sheetId="1" r:id="rId2"/><sheet name="Notes" sheetId="2" r:id="rId1"/></sheets>
</workbook>`
	rels := `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Target="worksheets/sheet1.xml"/>
  <Relationship Id="rId2" Target="/xl/worksheets/sheet2.xml"/>
</Relationships>`
	shared := `<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
  <si><t>Item</t></si>
  <si><r><t>Due</t></r><r><t xml:space="preserve"> date</t></r></si>
  <si><t>東京</t><rPh sb="0" eb="2"><t>トウキョウ</t></rPh></si>
  <si><t>two	parts</t></si>
</sst>`
	// Style 0 General, 1 built-in date (14), 2 custom date-time, 3 a colour
	// code that contains an m, 4 custom elapsed time.
	styles := `<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
  <numFmts>
    <numFmt numFmtId="164" formatCode="yyyy-mm-dd hh:mm"/>
    <numFmt numFmtId="165" formatCode="[Magenta]0.00"/>
    <numFmt numFmtId="166" formatCode="[h]:mm"/>
  </numFmts>
  <cellXfs>
    <xf numFmtId="0"/><xf numFmtId="14"/><xf numFmtId="164"/><xf numFmtId="165"/><xf numFmtId="166"/>
  </cellXfs>
</styleSheet>`
	budget := `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>
  <row r="1"><c r="A1" t="s"><v>0</v></c><c r="C1" t="s"><v>1</v></c></row>
  <row r="2"><c r="A2" t="s"><v>2</v></c><c r="B2" s="3"><v>12.5</v></c><c r="C2" s="1"><v>46082</v></c></row>
  <row r="3"><c r="A3" t="inlineStr"><is><t>Paid</t></is></c><c r="B3" t="b"><v>1</v></c><c r="C3" s="2"><v>46082.25</v></c></row>
  <row r="4"><c r="A4" t="s"><v>3</v></c><c r="B4" t="str"><f>A1</f><v>Item</v></c><c r="C4" s="4"><v>0.5</v></c></row>
  <row r="5"><c r="A5" s="0"/></row>
  <row r="6"/>
</sheetData></worksheet>`
	notes := `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>
  <row r="1"><c r="A1" t="inlineStr"><is><t>hello</t></is></c></row>
</sheetData></worksheet>`
	writeZip(t, dir, "budget.xlsx",
		[2]string{"xl/workbook.xml", workbook},
		[2]string{"xl/_rels/workbook.xml.rels", rels},
		[2]string{"xl/sharedStrings.xml", shared},
		[2]string{"xl/styles.xml", styles},
		[2]string{"xl/worksheets/sheet1.xml", notes},
		[2]string{"xl/worksheets/sheet2.xml", budget},
	)

	got := fetchDoc(t, dir, "budget.xlsx")
	want := "## Budget\n" +
		"Item\t\tDue date\n" +
		"東京\t12.5\t2026-03-01\n" +
		"Paid\tTRUE\t2026-03-01 06:00:00\n" +
		"two parts\tItem\t12:00:00\n" +
		"\n## Notes\nhello"
	if got != want {
		t.Errorf("xlsx text:\n got %q\nwant %q", got, want)
	}
}

func TestIsDateFormat(t *testing.T) {
	for code, want := range map[string]bool{
		"General":             false,
		"0.00":                false,
		"#,##0.00 ;[Red]-0":   false,
		"[Magenta]0.00":       false,
		"0.00E+00":            false,
		`"Days: "0`:           false,
		`0\d`:                 false,
		"yyyy-mm-dd":          true,
		"d/m/yy":              true,
		"[$-409]mmmm d, yyyy": true,
		"h:mm AM/PM":          true,
		"[h]:mm":              true,
		"[ss]":                true,
		"mm:ss.0":             true,
	} {
		if got := isDateFormat(code); got != want {
			t.Errorf("isDateFormat(%q) = %v, want %v", code, got, want)
		}
	}
}

func TestExcelDate(t *testing.T) {
	for _, c := range []struct {
		serial   float64
		date1904 bool
		want     string
	}{
		{46082, false, "2026-03-01"},
		{46082.25, false, "2026-03-01 06:00:00"},
		{46082.0 + 86399.6/86400.0, false, "2026-03-02"},
		{61, false, "1900-03-01"},
		{0.5, false, "12:00:00"},
		{0, true, "1904-01-01"},
		{44620, true, "2026-03-01"},
	} {
		if got := excelDate(c.serial, c.date1904); got != c.want {
			t.Errorf("excelDate(%v, 1904=%v) = %q, want %q", c.serial, c.date1904, got, c.want)
		}
	}
}

func TestColumnIndex(t *testing.T) {
	for ref, want := range map[string]int{
		"A1": 0, "a1": 0, "B5": 1, "b5": 1, "Z9": 25, "z9": 25,
		"AA1": 26, "aa1": 26, "AB10": 27, "ab10": 27,
		"123": -1, "": -1, "AAAA1": -1,
	} {
		if got := columnIndex(ref); got != want {
			t.Errorf("columnIndex(%q) = %d, want %d", ref, got, want)
		}
	}
}

func TestXlsxCellBoolean(t *testing.T) {
	dateStyles := map[int]bool{}
	for v, want := range map[string]string{
		"1": "TRUE", "true": "TRUE", "True": "TRUE", "TRUE": "TRUE",
		"0": "FALSE", "false": "FALSE", "False": "FALSE", "": "FALSE",
	} {
		if got := xlsxCell(v, "b", 0, nil, dateStyles, false); got != want {
			t.Errorf("xlsxCell(%q, b) = %q, want %q", v, got, want)
		}
	}
}

const odfNS = `xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" ` +
	`xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" ` +
	`xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0" ` +
	`xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0"`

func TestFetchReadsOdt(t *testing.T) {
	dir := t.TempDir()
	content := `<office:document-content ` + odfNS + `>
  <office:body>
    <office:text>
      <text:tracked-changes><text:changed-region><text:deletion><text:p>removed words</text:p></text:deletion></text:changed-region></text:tracked-changes>
      <text:h text:outline-level="1">Minutes</text:h>
      <text:p>Present:<text:s text:c="3"/>Ana<text:tab/>Budi<text:line-break/>Absent: none</text:p>
      <text:p>Agreed<office:annotation><text:p>ask Ana</text:p></office:annotation>, <text:span>unanimously</text:span>.</text:p>
      <table:table table:name="T1">
        <table:table-row><table:table-cell><text:p>a</text:p></table:table-cell><table:table-cell><text:p>b</text:p></table:table-cell></table:table-row>
      </table:table>
    </office:text>
  </office:body>
</office:document-content>`
	writeZip(t, dir, "minutes.odt", [2]string{"mimetype", "application/vnd.oasis.opendocument.text"}, [2]string{"content.xml", content})
	got := fetchDoc(t, dir, "minutes.odt")
	want := "Minutes\nPresent:   Ana\tBudi\nAbsent: none\nAgreed, unanimously.\na\tb"
	if got != want {
		t.Errorf("odt text:\n got %q\nwant %q", got, want)
	}
}

// A spreadsheet row ends in an empty cell repeated to the last column, and a
// sheet in an empty row repeated a million times. Neither is text, and
// expanded they are a decompression bomb made of blanks.
func TestFetchReadsOdsWithoutExpandingRepeats(t *testing.T) {
	dir := t.TempDir()
	content := `<office:document-content ` + odfNS + `>
  <office:body><office:spreadsheet>
    <table:table table:name="Sales">
      <table:table-row>
        <table:table-cell office:value-type="string"><text:p>Month</text:p></table:table-cell>
        <table:table-cell office:value-type="string"><text:p>Total</text:p></table:table-cell>
        <table:table-cell table:number-columns-repeated="16382"/>
      </table:table-row>
      <table:table-row>
        <table:table-cell office:value-type="date" office:date-value="2026-03-01"><text:p>01/03/26</text:p></table:table-cell>
        <table:table-cell office:value-type="date" office:date-value="2026-03-01T06:30:00"><text:p>01/03/26 06:30</text:p></table:table-cell>
        <table:table-cell table:number-columns-repeated="2" office:value-type="float" office:value="7"><text:p>7</text:p></table:table-cell>
      </table:table-row>
      <table:table-row table:number-rows-repeated="2"><table:table-cell table:number-columns-repeated="1024"/></table:table-row>
      <table:table-row><table:table-cell><text:p>end</text:p></table:table-cell></table:table-row>
      <table:table-row table:number-rows-repeated="1048570"><table:table-cell table:number-columns-repeated="16384"/></table:table-row>
    </table:table>
    <table:table table:name="Empty"/>
  </office:spreadsheet></office:body>
</office:document-content>`
	writeZip(t, dir, "sales.ods", [2]string{"content.xml", content})
	got := fetchDoc(t, dir, "sales.ods")
	want := "## Sales\nMonth\tTotal\n2026-03-01\t2026-03-01 06:30:00\t7\t7\n\n\nend\n\n## Empty"
	if got != want {
		t.Errorf("ods text:\n got %q\nwant %q", got, want)
	}
}

func TestFetchReadsOdp(t *testing.T) {
	dir := t.TempDir()
	content := `<office:document-content ` + odfNS + `>
  <office:body><office:presentation>
    <draw:page draw:name="page1"><draw:frame><draw:text-box><text:p>Roadmap</text:p></draw:text-box></draw:frame></draw:page>
    <draw:page draw:name="page2"><draw:frame><draw:text-box><text:p>Q1</text:p><text:p>Q2</text:p></draw:text-box></draw:frame></draw:page>
  </office:presentation></office:body>
</office:document-content>`
	writeZip(t, dir, "plan.odp", [2]string{"content.xml", content})
	got := fetchDoc(t, dir, "plan.odp")
	want := "--- slide 1 ---\nRoadmap\n--- slide 2 ---\nQ1\nQ2"
	if got != want {
		t.Errorf("odp text:\n got %q\nwant %q", got, want)
	}
}

// fetchPart fetches one part of name and fails the test on an error result.
func fetchPart(t *testing.T, dir, name string, part int) (string, bool) {
	t.Helper()
	args, _ := json.Marshal(map[string]any{"path": name, "part": part})
	res, err := NewFetch(dir).Call(context.Background(), "c1", args)
	if err != nil {
		t.Fatal(err)
	}
	return res.Content, res.IsError
}

// A document longer than one message is served in parts: each part within the
// cap, cut on a rune boundary, saying which part it is and how to get the
// next, and together they are the whole text — nothing skipped, nothing twice.
func TestFetchServesALongDocumentInParts(t *testing.T) {
	dir := t.TempDir()
	para := `<w:p><w:r><w:t>` + strings.Repeat("é", 500) + `</w:t></w:r></w:p>`
	doc := `<w:document ` + wordNS + `><w:body>` + strings.Repeat(para, 400) + `</w:body></w:document>`
	writeZip(t, dir, "long.docx", [2]string{"word/document.xml", doc})
	whole := strings.TrimRight(strings.Repeat(strings.Repeat("é", 500)+"\n", 400), "\n")

	one, errored := fetchPart(t, dir, "long.docx", 1)
	body1, note1, ok := strings.Cut(one, "\n\n[part 1 of 2")
	if errored || !ok || !strings.Contains(note1, "part=2") {
		t.Fatalf("part 1 has no note pointing at part 2: error=%v, ends %q", errored, one[max(0, len(one)-160):])
	}
	two, errored := fetchPart(t, dir, "long.docx", 2)
	body2, note2, ok := strings.Cut(two, "\n\n[part 2 of 2")
	if errored || !ok || !strings.Contains(note2, "the end") {
		t.Fatalf("part 2 is not marked as the end: error=%v, ends %q", errored, two[max(0, len(two)-160):])
	}
	for i, b := range []string{body1, body2} {
		if len(b) > maxFetchBytes || !utf8.ValidString(b) {
			t.Errorf("part %d: %d bytes, valid UTF-8 %v", i+1, len(b), utf8.ValidString(b))
		}
	}
	if body1+body2 != whole {
		t.Errorf("the parts are not the whole text: %d + %d bytes, want %d", len(body1), len(body2), len(whole))
	}
	if msg, errored := fetchPart(t, dir, "long.docx", 3); !errored || !strings.Contains(msg, "2 part") {
		t.Errorf("part 3 of 2: error=%v %q", errored, msg)
	}
	// Without part, fetch starts at the beginning, as it always did.
	if first := fetchDoc(t, dir, "long.docx"); first != one {
		t.Error("fetch with no part is not part 1")
	}
}

// A part that inflates past maxPartBytes is refused from its header, and one
// whose header lies is still cut off at the limit, not read to the end.
func TestFetchRefusesADecompressionBomb(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte("a"), 1<<20)
	for i := 0; i < maxPartBytes>>20+1; i++ {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bomb.docx"), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	res := fetchResult(t, dir, "bomb.docx")
	if !res.IsError || !strings.Contains(res.Content, "over the") {
		t.Errorf("a part of %d MB uncompressed was not refused: %q", maxPartBytes>>20+1, res.Content)
	}
}

func TestFetchExplainsWhatItCannotRead(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "old.xls"), []byte{0xD0, 0xCF, 0x11, 0xE0, 0, 0}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fake.docx"), []byte("not a zip at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeZip(t, dir, "empty.docx", [2]string{"docProps/app.xml", "<Properties/>"})

	for name, want := range map[string]string{
		"old.xls":    "Save As",
		"fake.docx":  "not a valid .docx",
		"empty.docx": "word/document.xml is missing",
	} {
		res := fetchResult(t, dir, name)
		if !res.IsError || !strings.Contains(res.Content, want) {
			t.Errorf("%s: got %q, want an error containing %q", name, res.Content, want)
		}
	}
}

// ---------------------------------------------------------------- PDF

// TestPdftotextHelper is not a test: it stands in for pdftotext when a test
// sets ARIADNE_FAKE_PDFTOTEXT, so the PDF path is tested without depending on
// Poppler being installed.
func TestPdftotextHelper(t *testing.T) {
	mode := os.Getenv("ARIADNE_FAKE_PDFTOTEXT")
	if mode == "" {
		return
	}
	file := os.Args[len(os.Args)-2] // ... <file> -
	switch mode {
	case "echo":
		data, _ := os.ReadFile(file)
		fmt.Printf("read %s\npath %s\n", data, file)
	case "empty":
		fmt.Print("\f\n")
	case "utf8split":
		prefix := bytes.Repeat([]byte("a"), (32<<10)-1)
		os.Stdout.Write(prefix)
		os.Stdout.Write([]byte{0xC3})
		time.Sleep(10 * time.Millisecond)
		os.Stdout.Write([]byte{0xA9, 'b', '\n'})
	case "pages":
		// What pdftotext writes on Windows: CRLF, a form feed after each page.
		fmt.Print("Roadmap\r\n\r\n  one binary\r\n\fQ1\r\n• ship\r\n\f")
	case "huge":
		for i := 0; i < 400; i++ {
			fmt.Println(strings.Repeat("x", 1023))
		}
	case "endless":
		line := strings.Repeat("y", 1023) + "\n"
		for {
			if _, err := os.Stdout.WriteString(line); err != nil {
				os.Exit(1)
			}
		}
	case "fail":
		fmt.Fprint(os.Stderr, "Syntax Error: Couldn't read xref table")
		os.Exit(1)
	}
	os.Exit(0)
}

func fakePdftotext(t *testing.T, mode string) {
	t.Helper()
	saved := pdfCommand
	pdfCommand = func(ctx context.Context, file string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPdftotextHelper$", "--", file, "-")
		cmd.Env = append(os.Environ(), "ARIADNE_FAKE_PDFTOTEXT="+mode)
		return cmd
	}
	t.Cleanup(func() { pdfCommand = saved })
}

func TestFetchReadsPDFThroughACopy(t *testing.T) {
	fakePdftotext(t, "echo")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.pdf"), []byte("%PDF-1.7 body"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := fetchDoc(t, dir, "report.pdf")
	if !strings.Contains(got, "read %PDF-1.7 body") {
		t.Errorf("pdftotext's output did not come back: %q", got)
	}
	// pdftotext opens a path, and os.Root cannot confine another program: it
	// must be handed a copy made through the root, never the workspace path.
	_, path, _ := strings.Cut(got, "path ")
	if abs, _ := filepath.Abs(dir); strings.HasPrefix(filepath.Clean(path), abs) {
		t.Errorf("pdftotext was given the workspace file %q, not a copy", path)
	}
	if _, err := os.Stat(strings.TrimSpace(path)); !os.IsNotExist(err) {
		t.Errorf("the copy %q was left behind (%v)", path, err)
	}
}

func TestFetchPDFFailures(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.pdf"), []byte("%PDF-1.7"), 0o600); err != nil {
		t.Fatal(err)
	}
	for mode, want := range map[string]string{
		"empty": "no text layer",
		"fail":  "Couldn't read xref table",
		"huge":  "[part 1 of 2",
	} {
		t.Run(mode, func(t *testing.T) {
			fakePdftotext(t, mode)
			res := fetchResult(t, dir, "a.pdf")
			if !strings.Contains(res.Content, want) {
				t.Errorf("got %q, want it to contain %q", clipped(res.Content), want)
			}
			if mode == "huge" && (res.IsError || len(res.Content) > maxFetchBytes+300) {
				t.Errorf("huge output: error=%v, %d bytes", res.IsError, len(res.Content))
			}
		})
	}

	t.Run("not installed", func(t *testing.T) {
		saved := pdfCommand
		pdfCommand = func(ctx context.Context, file string) *exec.Cmd {
			return exec.CommandContext(ctx, "ariadne-no-such-pdftotext", file, "-")
		}
		t.Cleanup(func() { pdfCommand = saved })
		res := fetchResult(t, dir, "a.pdf")
		if !res.IsError || !strings.Contains(res.Content, "install poppler") {
			t.Errorf("got %q, want how to install pdftotext", res.Content)
		}
	})
}

func clipped(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// The real pdftotext, when it is installed, on a PDF built here byte by byte.
func TestFetchReadsARealPDF(t *testing.T) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext not installed")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.pdf"), minimalPDF("Quarterly total 42"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := fetchDoc(t, dir, "hello.pdf"); !strings.Contains(got, "Quarterly total 42") {
		t.Errorf("got %q", got)
	}
}

// minimalPDF returns a one-page PDF showing text, with a correct xref table.
func minimalPDF(text string) []byte {
	stream := fmt.Sprintf("BT /F1 18 Tf 72 720 Td (%s) Tj ET", text)
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

// The output cannot show the repeat cap — trailing blanks are dropped either
// way — but memory can: one cell repeated 16384 times across a million rows
// is what the cap stops being built.
func TestODFRepeatIsBounded(t *testing.T) {
	for v, want := range map[string]int{"": 1, "0": 1, "-3": 1, "x": 1, "7": 7, "16384": maxRepeat, "1048576": maxRepeat} {
		if got := repeat(v); got != want {
			t.Errorf("repeat(%q) = %d, want %d", v, got, want)
		}
	}
}

// Once the text is full, pdftotext is stopped, not waited out: a PDF that
// yields endless text would otherwise hold the turn until the timeout. The
// cap is maxDocumentText, served as 64 parts of 256 KB.
func TestFetchPDFStopsAtTheCap(t *testing.T) {
	fakePdftotext(t, "endless")
	saved := pdfTimeout
	pdfTimeout = 20 * time.Second
	t.Cleanup(func() { pdfTimeout = saved })

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "big.pdf"), []byte("%PDF-1.7"), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res := fetchResult(t, dir, "big.pdf")
	if _, n, _ := strings.Cut(res.Content, "[part 1 of "); res.IsError || len(n) < 2 || n[:2] < "64" {
		t.Errorf("got error=%v %q", res.IsError, clipped(res.Content))
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("took %s: pdftotext ran until the timeout instead of being stopped at the cap", d.Round(time.Second))
	}
}

// pdftotext's page breaks become headings and its CRLFs newlines, the same
// shape as slides.
func TestFetchPDFMarksPages(t *testing.T) {
	fakePdftotext(t, "pages")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "deck.pdf"), []byte("%PDF-1.7"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := fetchDoc(t, dir, "deck.pdf")
	want := "--- page 1 ---\nRoadmap\n\n  one binary\n\n--- page 2 ---\nQ1\n• ship"
	if got != want {
		t.Errorf("pdf text:\n got %q\nwant %q", got, want)
	}
}

func TestFetchPDFUtf8RuneSplit(t *testing.T) {
	fakePdftotext(t, "utf8split")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "split.pdf"), []byte("%PDF-1.7"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := fetchDoc(t, dir, "split.pdf")
	if !strings.Contains(got, "éb") {
		t.Errorf("multi-byte rune was corrupted across buffer boundary: %q", got[len(got)-20:])
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Errorf("result contained RuneError: %q", got[len(got)-20:])
	}
}

func TestFetchReadsXlsxWithXlPrefixRels(t *testing.T) {
	dir := t.TempDir()
	workbook := `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" ` +
		`xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` +
		`<sheets><sheet name="Sheet1" sheetId="1" r:id="rId1"/></sheets></workbook>`
	rels := `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" ` +
		`Target="xl/worksheets/sheet1.xml"/></Relationships>`
	sheet := `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>` +
		`<row r="1"><c r="a1" t="b"><v>true</v></c></row>` +
		`</sheetData></worksheet>`
	writeZip(t, dir, "prefix.xlsx",
		[2]string{"xl/workbook.xml", workbook},
		[2]string{"xl/_rels/workbook.xml.rels", rels},
		[2]string{"xl/worksheets/sheet1.xml", sheet},
	)
	got := fetchDoc(t, dir, "prefix.xlsx")
	want := "## Sheet1\nTRUE"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
