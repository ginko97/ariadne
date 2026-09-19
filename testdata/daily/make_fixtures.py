"""Writes the fixtures for testdata/daily/tasks.json.

Every answer a task checks is planted here, in made-up text, so the expected
answers are known for certain. Deterministic: running it again produces the
same bytes. Run from the repo root:

    python testdata/daily/make_fixtures.py
"""
import os
import zipfile

HERE = os.path.dirname(os.path.abspath(__file__))
FILES = os.path.join(HERE, 'files')


def put(rel, data):
    path = os.path.join(FILES, *rel.split('/'))
    os.makedirs(os.path.dirname(path), exist_ok=True)
    mode = 'wb' if isinstance(data, bytes) else 'w'
    kw = {} if isinstance(data, bytes) else {'encoding': 'utf-8', 'newline': '\n'}
    with open(path, mode, **kw) as f:
        f.write(data)


def zipped(rel, parts):
    path = os.path.join(FILES, *rel.split('/'))
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with zipfile.ZipFile(path, 'w', zipfile.ZIP_DEFLATED) as z:
        for name, text in parts:
            info = zipfile.ZipInfo(name, date_time=(2026, 1, 1, 0, 0, 0))
            info.compress_type = zipfile.ZIP_DEFLATED
            z.writestr(info, text)


# ---- find: a report whose file name the prompt does not give -------------
put('find/notes.txt', 'Groceries: rice, eggs, chillies.\nCall the landlord on Friday.\n')
put('find/old/draft.txt', 'Draft, do not use. Revenue guess: 3,900,000.\n')
put('find/reports/q3-summary.txt',
    'Quarterly summary, Q3 2026\n\nRevenue for Q3: 4,210,000\nCosts for Q3: 3,305,000\n')

# ---- long: a code word only in part 2 (parts are 256 KB) -----------------
filler = ('The warehouse ledger lists crates, pallets and the dates they moved. '
          'Nothing in this paragraph is the code word. ') * 3 + '\n\n'
body = ''
while len(body.encode('utf-8')) < 290_000:
    body += filler
body += 'For the record, the code word is PELICAN.\n\n'
while len(body.encode('utf-8')) < 330_000:
    body += filler
put('long/long.txt', body)

# ---- story: how it ends is only in part 2 --------------------------------
chapter = ('{title}\n\n' + ('The seasons turned slowly in the valley, and the people who lived there '
                             'kept to their work and their small arguments. ') * 40 + '\n\n')
story = 'THE VALLEY\n\n'
n = 1
while len(story.encode('utf-8')) < 280_000:
    story += chapter.format(title=f'Chapter {n}: the orchard' if n == 1 else f'Chapter {n}')
    n += 1
story += ('Final chapter: the lighthouse\n\nIn the end the whole family left the valley and '
          'became lighthouse keepers on the northern coast, and never went back.\n')
put('story/story.txt', story)

# ---- office: one planted fact per format ---------------------------------
W = 'xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"'
zipped('office/contract.docx', [
    ('[Content_Types].xml', '<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>'),
    ('word/document.xml', f'<?xml version="1.0" encoding="UTF-8"?><w:document {W}><w:body>'
     '<w:p><w:r><w:t>Service agreement between Lumen Studio and Harbor Foods.</w:t></w:r></w:p>'
     '<w:tbl>'
     '<w:tr><w:tc><w:p><w:r><w:t>Term</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>Value</w:t></w:r></w:p></w:tc></w:tr>'
     '<w:tr><w:tc><w:p><w:r><w:t>Monthly fee</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>1,200 EUR</w:t></w:r></w:p></w:tc></w:tr>'
     '<w:tr><w:tc><w:p><w:r><w:t>Late penalty</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>2.5% per week</w:t></w:r></w:p></w:tc></w:tr>'
     '</w:tbl></w:body></w:document>'),
])
S = 'xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"'
zipped('office/budget.xlsx', [
    ('[Content_Types].xml', '<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>'),
    ('xl/workbook.xml', f'<workbook {S} xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">'
     '<sheets><sheet name="Budget" sheetId="1" r:id="rId1"/></sheets></workbook>'),
    ('xl/_rels/workbook.xml.rels', '<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">'
     '<Relationship Id="rId1" Target="worksheets/sheet1.xml"/></Relationships>'),
    ('xl/sharedStrings.xml', f'<sst {S}><si><t>Item</t></si><si><t>Amount</t></si><si><t>Due</t></si>'
     '<si><t>Rent</t></si><si><t>Internet</t></si></sst>'),
    ('xl/styles.xml', f'<styleSheet {S}><cellXfs><xf numFmtId="0"/><xf numFmtId="14"/></cellXfs></styleSheet>'),
    ('xl/worksheets/sheet1.xml', f'<worksheet {S}><sheetData>'
     '<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c><c r="C1" t="s"><v>2</v></c></row>'
     '<row r="2"><c r="A2" t="s"><v>3</v></c><c r="B2"><v>1200</v></c><c r="C2" s="1"><v>46082</v></c></row>'
     '<row r="3"><c r="A3" t="s"><v>4</v></c><c r="B3"><v>45</v></c><c r="C3" s="1"><v>46086</v></c></row>'
     '</sheetData></worksheet>'),
])
P = ('xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" '
     'xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"')


def slide(*lines):
    runs = ''.join(f'<a:p><a:r><a:t>{l}</a:t></a:r></a:p>' for l in lines)
    return f'<p:sld {P}><p:cSld><p:spTree><p:sp><p:txBody>{runs}</p:txBody></p:sp></p:spTree></p:cSld></p:sld>'


zipped('office/deck.pptx', [
    ('[Content_Types].xml', '<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>'),
    ('ppt/slides/slide1.xml', slide('Harbor Foods app', 'Plan for 2026')),
    ('ppt/slides/slide2.xml', slide('Beta', 'Invite 200 customers')),
    ('ppt/slides/slide3.xml', slide('Launch', 'Launch date: 2026-10-14')),
])
print('fixtures written to', FILES)
