package verso

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func span(text string, x, y, w float64) Span {
	return Span{Box: Box{x, y, w, 12}, Text: text, Font: "Arial", Color: "000000", Size: 11, Baseline: y + 10}
}

func tablePage() Page {
	p := Page{Width: 612, Height: 792, Spans: []Span{span("Report & review", 50, 30, 100), span("Merged heading", 55, 105, 160), span("123.45", 55, 135, 50), span("Note", 155, 135, 40), span("Due", 155, 165, 40)}}
	for _, y := range []float64{100, 130, 190} {
		p.Segments = append(p.Segments, Segment{50, y, 250, y})
	}
	p.Segments = append(p.Segments, Segment{150, 160, 250, 160}, Segment{50, 100, 50, 190}, Segment{250, 100, 250, 190}, Segment{150, 130, 150, 190})
	return p
}

func docParts(t *testing.T, data []byte) map[string]string {
	t.Helper()
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	parts := map[string]string{}
	for _, f := range z.File {
		r, e := f.Open()
		if e != nil {
			t.Fatal(e)
		}
		b, e := io.ReadAll(r)
		r.Close()
		if e != nil {
			t.Fatal(e)
		}
		parts[f.Name] = string(b)
		if strings.HasSuffix(f.Name, ".xml") || strings.HasSuffix(f.Name, ".rels") {
			d := xml.NewDecoder(bytes.NewReader(b))
			for {
				_, e = d.Token()
				if e == io.EOF {
					break
				}
				if e != nil {
					t.Fatalf("%s: %v", f.Name, e)
				}
			}
		}
	}
	return parts
}

func TestEditableTableAndLinks(t *testing.T) {
	p := tablePage()
	p.Spans = append(p.Spans, span("continued value", 55, 165, 80))
	p.Spans[0].Link = "https://example.org/?a=1&b=2"
	var b bytes.Buffer
	r, err := Write(&b, []Page{p})
	if err != nil {
		t.Fatal(err)
	}
	parts := docParts(t, b.Bytes())
	body := parts["word/document.xml"]
	// The physical third row has no horizontal border in its first column:
	// both lines must remain in one editable Word cell, in source order.
	doc := struct {
		Tables []struct {
			Rows []struct {
				Cells []struct {
					Paragraphs []struct {
						Runs []struct {
							Text string `xml:"t"`
						} `xml:"r"`
					} `xml:"p"`
				} `xml:"tc"`
			} `xml:"tr"`
		} `xml:"body>tbl"`
	}{}
	if err := xml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Tables) != 1 || len(doc.Tables[0].Rows) != 3 {
		t.Fatal("lost source grid")
	}
	var cellText strings.Builder
	for _, p := range doc.Tables[0].Rows[1].Cells[0].Paragraphs {
		for _, r := range p.Runs {
			cellText.WriteString(r.Text)
		}
	}
	if cellText.String() != "123.45continued value" {
		t.Fatalf("split merged cell text: %q", cellText.String())
	}
	if r.Tables != 1 {
		t.Fatalf("tables=%d; wanted one connected grid", r.Tables)
	}
	for _, s := range []string{`w:gridSpan w:val="2"`, `w:vMerge w:val="restart"`, `<w:vMerge/>`, "123.45", "Report &amp; review", "Merged heading", "w:hyperlink"} {
		if !strings.Contains(body, s) {
			t.Fatalf("lost table or text feature %s", s)
		}
	}
	if strings.Contains(body, "<w:tab/>") || strings.Contains(body, "#000000") {
		t.Fatal("PDF runs must not become Word tabs or invalid Word colors")
	}
	d := xml.NewDecoder(strings.NewReader(body))
	runs := 0
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch x := token.(type) {
		case xml.StartElement:
			if x.Name.Local == "r" {
				runs++
			}
			if x.Name.Local == "hyperlink" && runs > 0 {
				t.Fatal("hyperlink nested inside a run")
			}
		case xml.EndElement:
			if x.Name.Local == "r" {
				runs--
			}
		}
	}
}

func TestTableBoundaryCellOwnership(t *testing.T) {
	for _, tc := range []struct {
		name     string
		x, y     float64
		row, col int
	}{
		{"left of column divider", 199.75, 120, 0, 0},
		{"on column divider", 200, 120, 0, 1},
		{"right of column divider", 200.25, 120, 0, 1},
		{"above row divider", 100, 129.75, 0, 0},
		{"on row divider", 100, 130, 1, 0},
		{"below row divider", 100, 130.25, 1, 0},
		{"divider intersection", 200.25, 130.25, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Page{Width: 612, Height: 792, Spans: []Span{span("R", tc.x-3, tc.y-6, 6)}}
			for _, x := range []float64{50, 200, 350} {
				p.Segments = append(p.Segments, Segment{x, 100, x, 160})
			}
			for _, y := range []float64{100, 130, 160} {
				p.Segments = append(p.Segments, Segment{50, y, 350, y})
			}
			var b bytes.Buffer
			if _, err := Write(&b, []Page{p}); err != nil {
				t.Fatal(err)
			}
			assertTableCell(t, b.Bytes(), "R", tc.row, tc.col)
		})
	}
}

func assertTableCell(t *testing.T, data []byte, text string, row, col int) {
	t.Helper()
	doc := struct {
		Tables []struct {
			Rows []struct {
				Cells []struct {
					Text []string `xml:"p>r>t"`
				} `xml:"tc"`
			} `xml:"tr"`
		} `xml:"body>tbl"`
	}{}
	if err := xml.Unmarshal([]byte(docParts(t, data)["word/document.xml"]), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Tables) != 1 || len(doc.Tables[0].Rows) != 2 {
		t.Fatal("lost source grid")
	}
	for r, cells := range doc.Tables[0].Rows {
		if len(cells.Cells) != 2 {
			t.Fatal("lost source columns")
		}
		for c, cell := range cells.Cells {
			want := ""
			if r == row && c == col {
				want = text
			}
			if got := strings.Join(cell.Text, ""); got != want {
				t.Errorf("cell [%d,%d] = %q, want %q", r, c, got, want)
			}
		}
	}
}

func TestPDFTableBoundaryCellOwnership(t *testing.T) {
	opts := integrationOptions(t)
	dir := t.TempDir()
	input, output := filepath.Join(dir, "boundary.pdf"), filepath.Join(dir, "boundary.docx")
	fixturePDF(t, input, 1, false, func(string) string {
		// Helvetica's 10pt R has a 7.22pt advance, placing its center
		// 0.25pt to the right of the payment table's column divider.
		return "BT /F1 10 Tf 196.64 635 Td (R) Tj ET\n" +
			"0.5 w 50 680 m 350 680 l S 50 650 m 350 650 l S 50 620 m 350 620 l S 50 680 m 50 620 l S 200 680 m 200 620 l S 350 680 m 350 620 l S\n"
	})
	pages, err := Extract(context.Background(), input, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 || len(pages[0].Spans) != 1 {
		t.Fatal("expected one decoded source glyph")
	}
	s := pages[0].Spans[0]
	if cx := s.X + s.Width/2; cx <= 200 || cx >= 200.5 {
		t.Fatalf("fixture must exercise right-side divider ownership, got center %g", cx)
	}
	if _, err := Convert(context.Background(), input, output, opts); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	assertTableCell(t, data, "R", 1, 1)
}

func TestSeparateColumnsAndAdjacentRuns(t *testing.T) {
	p := Page{Width: 612, Height: 792, Spans: []Span{span("share", 50, 50, 25), span("holder", 75, 50, 30), span("right column", 350, 50, 100)}}
	l := lines(p.Spans)
	if len(l) != 2 {
		t.Fatalf("column gutter must create two editable blocks, got %d", len(l))
	}
	var b bytes.Buffer
	if _, err := Write(&b, []Page{p}); err != nil {
		t.Fatal(err)
	}
	body := docParts(t, b.Bytes())["word/document.xml"]
	if strings.Contains(body, `xml:space="preserve"> </w:t>`) {
		t.Fatal("adjacent glyph runs acquired a word-breaking space")
	}
}

func TestRejectInvalidGeometryAndContent(t *testing.T) {
	for name, mutate := range map[string]func(*Page){"bad color": func(p *Page) { p.Spans[0].Color = "#000000" }, "XML control": func(p *Page) { p.Spans[0].Text = "a\x00b" }, "active link": func(p *Page) { p.Spans[0].Link = "javascript:alert(1)" }, "page size": func(p *Page) { p.Width = 1 }, "bad bounds": func(p *Page) { p.Spans[0].X = 10000 }} {
		t.Run(name, func(t *testing.T) {
			p := tablePage()
			mutate(&p)
			var b bytes.Buffer
			if _, err := Write(&b, []Page{p}); err == nil {
				t.Fatal("invalid model accepted")
			}
		})
	}
}

// A structurally valid PDF written with standard PDF objects, not a mocked
// decoder response. The payment grid, URI, pages and glyphs cross Recto too.
func fixturePDF(t *testing.T, path string, pages int, rotated bool, transforms ...func(string) string) {
	t.Helper()
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>", "<< /Type /Font /Subtype /Type1 /BaseFont /Courier-Bold >>", "<< /Type /XObject /Subtype /Image /Width 2 /Height 2 /ColorSpace /DeviceRGB /BitsPerComponent 8 /Length 12 >>\nstream\n\xff\x00\x00\x00\xff\x00\x00\x00\xff\xff\xff\x00\nendstream"}
	var kids []string
	for i := 0; i < pages; i++ {
		id := len(objects) + 1
		kids = append(kids, fmt.Sprintf("%d 0 R", id))
		rotate := ""
		if rotated {
			rotate = " /Rotate 90"
		}
		objects = append(objects, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792]%s /Resources << /Font << /F1 3 0 R /F2 4 0 R >> /XObject << /Im1 5 0 R >> >> /Contents %d 0 R /Annots [%d 0 R] >>", rotate, id+1, id+2))
		content := fmt.Sprintf("BT /F1 12 Tf 50 740 Td (Example report %d) Tj ET\nBT /F1 10 Tf 50 710 Td (Review the shareholder name.) Tj ET\nBT /F1 10 Tf 55 665 Td (Payment) Tj ET\nBT /F1 10 Tf 205 665 Td (Amount) Tj ET\nBT /F1 10 Tf 55 635 Td (Total) Tj ET\nBT /F1 10 Tf 205 635 Td (123.45) Tj ET\n0.5 w 50 680 m 350 680 l S 50 650 m 350 650 l S 50 620 m 350 620 l S 50 680 m 50 620 l S 200 680 m 200 620 l S 350 680 m 350 620 l S\n", i+1)
		content += "BT /F1 10 Tf 50 580 Td 0.25 Tc (Source tracking remains editable.) Tj ET\n"
		content += "BT /F1 10 Tf 140 635 Td (678.90) Tj ET\n"
		content += "BT /F1 10 Tf 50 540 Td (The shareholder agrees to transfer the) Tj ET\nBT /F1 10 Tf 50 528 Td (registered shares on the closing date.) Tj ET\n"
		for _, transform := range transforms {
			content = transform(content)
		}
		objects = append(objects, fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content), "<< /Type /Annot /Subtype /Link /Rect [50 735 180 755] /Border [0 0 0] /A << /S /URI /URI (https://example.org/review) >> >>")
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Count %d /Kids [%s] >>", pages, strings.Join(kids, " "))
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := []int{0}
	for i, obj := range objects {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&b, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&b, "trailer << /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	if err := os.WriteFile(path, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
}

func integrationOptions(t *testing.T) Options {
	t.Helper()
	dll := os.Getenv("VERSO_VALIDATOR_DLL")
	if dll == "" {
		dll, _ = filepath.Abs(defaultValidatorDLL)
	}
	// Tests trust the repository's own node_modules; the library never looks
	// at the working directory.
	if os.Getenv("VERSO_RECTO_MODULE") == "" {
		root, _ := filepath.Abs(repoRoot)
		t.Setenv("VERSO_RECTO_MODULE", filepath.Join(root, "node_modules", "@2018kguo", "recto", "dist", "index.js"))
	}
	module, err := FindRecto()
	if err == nil {
		_, err = os.Stat(dll)
	}
	if err != nil {
		if os.Getenv("VERSO_REQUIRE_INTEGRATION") == "1" {
			t.Fatal(err)
		}
		t.Skip("install Recto and build the validator for integration tests")
	}
	return Options{RectoModule: module, Validate: Validator("dotnet", dll), AllowWarnings: true, Timeout: 30 * time.Second}
}

// A sentence the PDF wrapped over two lines is one Word paragraph.
func TestWrappedSentenceIsOneParagraph(t *testing.T) {
	opts := integrationOptions(t)
	dir := t.TempDir()
	input, output := filepath.Join(dir, "input.pdf"), filepath.Join(dir, "output.docx")
	fixturePDF(t, input, 1, false)
	if _, err := Convert(context.Background(), input, output, opts); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(output)
	body := docParts(t, data)["word/document.xml"]
	for _, p := range regexp.MustCompile(`(?s)<w:p>.*?</w:p>`).FindAllString(body, -1) {
		var text strings.Builder
		for _, m := range regexp.MustCompile(`<w:t[^>]*>([^<]*)</w:t>`).FindAllStringSubmatch(p, -1) {
			text.WriteString(m[1])
		}
		if text.String() == "The shareholder agrees to transfer the registered shares on the closing date." {
			return
		}
	}
	t.Fatal("wrapped sentence is not one paragraph")
}

// A /Rotate page is laid out as displayed: a landscape Word page, with text
// that reads vertically there kept as images rather than refusing the file.
func TestRotatedPageConverts(t *testing.T) {
	opts := integrationOptions(t)
	dir := t.TempDir()
	input, output := filepath.Join(dir, "rotated.pdf"), filepath.Join(dir, "rotated.docx")
	fixturePDF(t, input, 1, true)
	report, err := Convert(context.Background(), input, output, opts)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(output)
	body := docParts(t, data)["word/document.xml"]
	if !strings.Contains(body, `<w:pgSz w:w="15840" w:h="12240"/>`) || report.Images == 0 || !strings.Contains(strings.Join(report.Warnings, " "), "rotated or unmapped characters kept as images") {
		t.Fatalf("rotated page not kept as displayed: %+v", report)
	}
}

func TestPublicationAndRefusal(t *testing.T) {
	base := integrationOptions(t)
	dir := t.TempDir()
	input := filepath.Join(dir, "input.pdf")
	fixturePDF(t, input, 2, false)
	for _, name := range []string{"validator failure", "validator changes bytes", "warnings", "page limit", "byte limit", "deadline", "malformed"} {
		t.Run(name, func(t *testing.T) {
			opts := base
			source := input
			output := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".docx")
			switch name {
			case "validator failure":
				opts.Validate = func(context.Context, string) error { return errors.New("rejected by validator") }
			case "validator changes bytes":
				opts.Validate = func(_ context.Context, p string) error { return os.WriteFile(p, []byte("changed"), 0600) }
			case "warnings":
				opts.AllowWarnings = false
				source = filepath.Join(dir, "rotated.pdf")
				fixturePDF(t, source, 1, false, func(c string) string { return c + "BT /F1 12 Tf 0 1 -1 0 300 300 Tm (Rotated) Tj ET\n" })
			case "page limit":
				opts.MaxPages = 1
			case "byte limit":
				opts.MaxInputBytes = 20
			case "deadline":
				opts.Timeout = time.Nanosecond
			case "malformed":
				source = filepath.Join(dir, "malformed.pdf")
				os.WriteFile(source, []byte("%PDF-1.7\ngarbage"), 0600)
			}
			if _, err := Convert(context.Background(), source, output, opts); err == nil {
				t.Fatal("unsafe conversion was published")
			}
			if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("destination exists after refusal: %v", err)
			}
			files, _ := filepath.Glob(filepath.Join(dir, ".verso-*"))
			if len(files) != 0 {
				t.Fatal("temporary output leaked")
			}
		})
	}
	output := filepath.Join(dir, "concurrent.docx")
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := Convert(context.Background(), input, output, base); results <- err }()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("concurrent publishers: %d succeeded", success)
	}
	if err := base.Validate(context.Background(), output); err != nil {
		t.Fatal(err)
	}
}

func TestHundredPageDocument(t *testing.T) {
	if testing.Short() {
		t.Skip("large-document stress")
	}
	opts := integrationOptions(t)
	dir := t.TempDir()
	input, output := filepath.Join(dir, "large.pdf"), filepath.Join(dir, "large.docx")
	fixturePDF(t, input, 100, false)
	r, err := Convert(context.Background(), input, output, opts)
	if err != nil {
		t.Fatal(err)
	}
	if r.Pages != 100 || r.Tables != 100 || !r.Validated {
		t.Fatalf("truncated large document: %+v", r)
	}
	b, _ := os.ReadFile(output)
	if strings.Count(docParts(t, b)["word/document.xml"], "123.45") != 100 {
		t.Fatal("large document lost table values")
	}
	t.Logf("100 pages: %d ms, %d bytes", r.ElapsedMS, len(b))
}

func TestRenderedPaymentGeometry(t *testing.T) {
	opts := integrationOptions(t)
	for _, tool := range []string{"soffice", "pdftotext"} {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("VERSO_REQUIRE_INTEGRATION") == "1" {
				t.Fatal(err)
			}
			t.Skip("LibreOffice and Poppler required for render comparisons")
		}
	}
	dir := t.TempDir()
	input, output := filepath.Join(dir, "source.pdf"), filepath.Join(dir, "converted.docx")
	// A running head and a counting foot move to header and footer parts,
	// which must not push the body.
	fixturePDF(t, input, 3, false, func(c string) string {
		n := regexp.MustCompile(`Example report (\d+)`).FindStringSubmatch(c)[1]
		return c + "BT /F1 9 Tf 260 770 Td (CONFIDENTIAL) Tj ET\nBT /F1 9 Tf 290 30 Td (Page " + n + ") Tj ET\n"
	})
	if _, err := Convert(context.Background(), input, output, opts); err != nil {
		t.Fatal(err)
	}
	if parts := docParts(t, mustRead(t, output)); strings.Contains(parts["word/document.xml"], "CONFIDENTIAL") {
		t.Fatal("running head left in the body")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "soffice", "-env:UserInstallation=file://"+filepath.Join(dir, "profile"), "--headless", "--convert-to", "pdf", "--outdir", dir, output)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("LibreOffice: %v: %s", err, b)
	}
	type point struct {
		page int
		x, y float64
	}
	positions := func(path string) (pages int, amounts []point) {
		t.Helper()
		b, err := exec.CommandContext(ctx, "pdftotext", "-bbox", path, "-").Output()
		if err != nil {
			t.Fatal(err)
		}
		d := xml.NewDecoder(bytes.NewReader(b))
		for {
			token, err := d.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if s, ok := token.(xml.StartElement); ok {
				if s.Name.Local == "page" {
					pages++
				}
				if s.Name.Local == "word" {
					var value string
					if err := d.DecodeElement(&value, &s); err != nil {
						t.Fatal(err)
					}
					if value == "123.45" || value == "678.90" || value == "editable." {
						p := point{page: pages}
						for _, a := range s.Attr {
							if a.Name.Local == "xMin" {
								p.x, _ = strconv.ParseFloat(a.Value, 64)
							}
							if a.Name.Local == "yMin" {
								p.y, _ = strconv.ParseFloat(a.Value, 64)
							}
						}
						amounts = append(amounts, p)
					}
				}
			}
		}
		return
	}
	sourcePages, source := positions(input)
	renderPages, render := positions(filepath.Join(dir, "converted.pdf"))
	if sourcePages != renderPages || len(source) != 9 || len(render) != 9 {
		t.Fatalf("render lost pages/amounts: source=%d/%d output=%d/%d", sourcePages, len(source), renderPages, len(render))
	}
	for i, a := range source {
		b := render[i]
		if a.page != b.page || math.Abs(a.x-b.x) > 3 || math.Abs(a.y-b.y) > 3 {
			t.Fatalf("payment amount moved: source=%+v Word/LibreOffice=%+v", a, b)
		}
	}
}

// Horizontal scaling (Tz) is Word's character scale: published without
// review, and restored with the graphics state.
func TestHorizontalScaleIsCharacterScale(t *testing.T) {
	opts := integrationOptions(t)
	opts.AllowWarnings = false
	for _, scale := range []int{50, 100, 150} {
		t.Run(strconv.Itoa(scale), func(t *testing.T) {
			dir := t.TempDir()
			input, output := filepath.Join(dir, "scaled.pdf"), filepath.Join(dir, "scaled.docx")
			fixturePDF(t, input, 1, false, func(_ string) string {
				return fmt.Sprintf("q BT /F1 12 Tf 50 740 Td %d Tz (Horizontal scaling) Tj ET Q\nBT /F1 12 Tf 50 700 Td (Restored width) Tj ET\n", scale)
			})
			report, err := Convert(context.Background(), input, output, opts)
			if err != nil || len(report.Warnings) > 0 {
				t.Fatalf("%+v %v", report, err)
			}
			doc := docParts(t, mustRead(t, output))["word/document.xml"]
			scaled, restored, _ := strings.Cut(doc, "Restored width")
			restored = scaled[strings.LastIndex(scaled, "<w:rPr>"):]
			if strings.Contains(restored, "<w:w ") || (scale == 100) == strings.Contains(scaled, fmt.Sprintf(`<w:w w:val="%d"/>`, scale)) {
				t.Fatalf("scale %d%%: %s", scale, doc)
			}
		})
	}
}
func TestWideLettersAreNotScaling(t *testing.T) {
	opts := integrationOptions(t)
	opts.AllowWarnings = false
	dir := t.TempDir()
	input, output := filepath.Join(dir, "wide.pdf"), filepath.Join(dir, "wide.docx")
	fixturePDF(t, input, 1, false, func(_ string) string {
		return "BT /F2 12 Tf 50 740 Td (Announcing nine new running banners: nnnnnnnn) Tj ET\n"
	})
	if report, err := Convert(context.Background(), input, output, opts); err != nil {
		t.Fatalf("%v %+v", err, report)
	}
}

func FuzzPageModel(f *testing.F) {
	seed, _ := json.Marshal(tablePage())
	f.Add(seed)
	// Faces with and without known widths, word gaps, leaders, a scale.
	var spans []Span
	for i, font := range []string{"Verdana", "ArialMT", "DejaVuSansMono", "Courier", "Symbol"} {
		s := span("Entry .......", 72+90*float64(i), 100, 80)
		s.Font, s.Scale = font, 90+5*i
		spans = append(spans, s, span("7", 160+90*float64(i), 100, 6))
	}
	seed, _ = json.Marshal(Page{Width: 612, Height: 792, Spans: spans})
	f.Add(seed)
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 65536 {
			return
		}
		var p Page
		if json.Unmarshal(b, &p) != nil {
			return
		}
		var output bytes.Buffer
		if _, err := Write(&output, []Page{p}); err == nil {
			docParts(t, output.Bytes())
		}
	})
}

func TestFontNames(t *testing.T) {
	for in, want := range map[string]string{"ABCDEF+TimesNewRomanPS-BoldMT": "Times New Roman", "ArialMT": "Arial", "Arial-Black": "Arial Black", "Arial,Bold": "Arial", "Helvetica-Oblique": "Arial", "SegoeUI-Semibold": "Segoe UI Semibold", "Calibri": "Calibri", "CourierNewPSMT": "Courier New", "Times-Roman": "Times New Roman", "BookAntiqua-Bold": "Book Antiqua", "STSong-Light-UniGB-UCS2-H": "STSong Light", "MSungStd-Light-Acro": "MSung Std Light", "ABCDEF+SomeVeryLongFontFamilyNameThatGoesOnAndOn": "Some Very Long Font Family Name", "DejaVuSans-Bold": "Deja Vu Sans", "Courier-Bold": "Courier New", "LiberationSerif": "Liberation Serif"} {
		if got := fontName(in); got != want {
			t.Errorf("fontName(%q) = %q, want %q", in, got, want)
		}
	}
}

// Kerned glyph runs split a word into spans; the word is still one word.
func TestFirstWordCrossesKernedSpans(t *testing.T) {
	l := Line{Spans: []Span{span("M", 50, 50, 10), span("auris", 60, 50, 30), span("et orci.", 93, 50, 40)}}
	if got := l.text(); got != "Mauris et orci." {
		t.Fatalf("text %q", got)
	}
	if w := l.firstWord(); w < 40 || w > 44 {
		t.Fatalf("first word width %.1f, want the 40pt of Mauris plus a space", w)
	}
	// A previous line with 30pt free cannot have taken "Mauris": one paragraph.
	prev := Line{Spans: []Span{span("Proin pharetra nonummy pede.", 50, 38, 200)}, X: 50, Width: 200, Baseline: 48, Height: 12}
	l.X, l.Width, l.Baseline, l.Height = 50, 83, 60, 12
	if ps := paragraphs([]Line{prev, l}, 50, 280); len(ps) != 1 {
		t.Fatalf("wrapped word split the paragraph into %d", len(ps))
	}
}

// Library callers may skip validation; the report says it was not checked.
func TestUnvalidatedConversionSaysSo(t *testing.T) {
	opts := integrationOptions(t)
	opts.Validate = nil
	dir := t.TempDir()
	input, output := filepath.Join(dir, "input.pdf"), filepath.Join(dir, "output.docx")
	fixturePDF(t, input, 1, false)
	report, err := Convert(context.Background(), input, output, opts)
	if err != nil || report.Validated {
		t.Fatalf("report %+v, err %v", report, err)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatal(err)
	}
}

// A wrapped list item stays one paragraph even when the list wraps short of
// the body's right edge, so the free space alone would call it a new one.
func TestWrappedListItemContinues(t *testing.T) {
	body := Line{Spans: []Span{span("Body text reaches the right margin of the page here.", 61, 120, 433)}, X: 61, Width: 433, Baseline: 130, Height: 12}
	item := Line{Spans: []Span{span("•", 79, 134, 5), span("Documentation and implementation of functionally", 97, 134, 350)}, X: 79, Width: 368, Baseline: 144, Height: 12}
	next := Line{Spans: []Span{span("aligned processes.", 97, 148, 100)}, X: 97, Width: 100, Baseline: 158, Height: 12}
	ps := paragraphs([]Line{body, item, next}, 61, 494)
	if len(ps) != 2 || len(ps[1].Lines) != 2 {
		t.Fatalf("list item split: %d paragraphs", len(ps))
	}
	if ps[1].Indent >= 0 {
		t.Fatalf("list item lost its hanging indent: %+v", ps[1])
	}
}

// A rule just under text is its underline only if it ends with the text; a
// border under a heading runs on across the column and stays art.
func TestUnderlineVersusHeadingRule(t *testing.T) {
	heading := []Span{span("PREPARING THE OFFICE", 70, 100, 150)}
	if underlines(heading, Segment{60, 111, 550, 111}) || heading[0].Underline {
		t.Fatal("heading border taken as an underline")
	}
	if !underlines(heading, Segment{70, 111, 220, 111}) || !heading[0].Underline {
		t.Fatal("underline missed")
	}
}

func TestSymbolListMarkers(t *testing.T) {
	for _, m := range []string{"☐", "☒", "➢", "", "•", "3.", "(b)", "1.2.1"} {
		l := Line{Spans: []Span{span(m, 70, 100, 8), span("Pause routine deliveries.", 90, 100, 120)}}
		if !l.marked() {
			t.Errorf("%q is a list marker", m)
		}
	}
	if (Line{Spans: []Span{span("Total", 70, 100, 30), span("12.00", 110, 100, 30)}}).marked() {
		t.Error("a word is not a list marker")
	}
}

// One column, one font size, two pitches: a paragraph at 10.5pt spacing and
// crammed small print at 6.8pt. Neither pitch may split the other's lines.
func TestLocalPitchKeepsParagraphs(t *testing.T) {
	line := func(text string, base float64, width float64) Line {
		s := Span{Box: Box{45, base - 8, width, 10}, Text: text, Font: "Arial", Color: "000000", Size: 8.5, Baseline: base}
		return Line{Spans: []Span{s}, X: 45, Y: base - 8, Width: width, Height: 10, Baseline: base}
	}
	ls := []Line{line("In exercise of the rights and powers conferred upon", 190, 530), line("Assignment both dated the 23rd day of February", 200.5, 530), line("Assignee Bank it is hereby proclaimed that", 211, 500),
		line("The strata title for the subject property has been", 240, 530)}
	for i := 1; i < 6; i++ { // the small print outnumbers the paragraph above
		ls = append(ls, line("issued and the title is held under the name of the", 240+6.8*float64(i), 530))
	}
	ls[len(ls)-1].Width, ls[len(ls)-1].Spans[0].Width = 400, 400
	ps := paragraphs(ls, 45, 575)
	if len(ps) != 2 || len(ps[0].Lines) != 3 || len(ps[1].Lines) != 6 {
		var n []int
		for _, p := range ps {
			n = append(n, len(p.Lines))
		}
		t.Fatalf("paragraph line counts %v, want [3 6]", n)
	}
}

// A producer that nudges every glyph (TJ adjustments) still yields a run per
// stretch of text, not a run per letter.
func TestNudgedGlyphsShareRuns(t *testing.T) {
	opts := integrationOptions(t)
	dir := t.TempDir()
	input, output := filepath.Join(dir, "nudged.pdf"), filepath.Join(dir, "nudged.docx")
	const text = "Each glyph moves by a hair"
	fixturePDF(t, input, 1, false, func(_ string) string {
		var tj strings.Builder
		for i, r := range text {
			fmt.Fprintf(&tj, "(%c) %d ", r, 5-10*(i%2))
		}
		return "BT /F1 10 Tf 50 700 Td [" + tj.String() + "] TJ ET\n"
	})
	if _, err := Convert(context.Background(), input, output, opts); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(output)
	runs := strings.Count(docParts(t, data)["word/document.xml"], "<w:r>")
	if runs > 6 {
		t.Fatalf("%d runs for one nudged line", runs)
	}
}

// A white panel drawn past the page edge behind all text is a backdrop, not
// vector art that needs review.
func TestBackdropIsNotArtwork(t *testing.T) {
	opts := integrationOptions(t)
	opts.AllowWarnings = false
	dir := t.TempDir()
	input, output := filepath.Join(dir, "backdrop.pdf"), filepath.Join(dir, "backdrop.docx")
	fixturePDF(t, input, 1, false, func(content string) string {
		return "1 g 28 -700 556 3000 re f 0 g\n" + content
	})
	if report, err := Convert(context.Background(), input, output, opts); err != nil {
		t.Fatalf("%v %+v", err, report)
	}
}

// A numbered item whose text starts at a half-inch tab is one line, not a
// column of numbers beside a column of text.
func TestWideMarkerTabStaysOneLine(t *testing.T) {
	ls := lines([]Span{span("1.", 72, 100, 8), span("The Borrower shall repay the loan.", 108, 100, 180)})
	if len(ls) != 1 {
		t.Fatalf("marker split from its text: %d pieces", len(ls))
	}
	ls = lines([]Span{span("Name", 72, 100, 30), span("Amount", 300, 100, 40)})
	if len(ls) != 2 {
		t.Fatalf("separate columns merged: %d pieces", len(ls))
	}
}

// A Recto module planted in the working directory (an extracted upload) is
// never loaded.
func TestFindRectoIgnoresWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	planted := filepath.Join(dir, "node_modules", "@2018kguo", "recto")
	if err := os.MkdirAll(filepath.Join(planted, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(planted, "package.json"), []byte(`{"name":"@2018kguo/recto","version":"0.8.2"}`), 0o600)
	os.WriteFile(filepath.Join(planted, "dist", "index.js"), []byte(`throw new Error("planted")`), 0o600)
	t.Chdir(dir)
	t.Setenv("VERSO_RECTO_MODULE", "")
	if m, _ := FindRecto(); strings.HasPrefix(m, dir) {
		t.Fatalf("loaded the working directory's module: %s", m)
	}
}

// Each placed image region is rendered with everything drawn there, so a
// stack of the same picture is one image, not one per draw.
func TestStackedImagesRenderOnce(t *testing.T) {
	opts := integrationOptions(t)
	dir := t.TempDir()
	input, output := filepath.Join(dir, "stack.pdf"), filepath.Join(dir, "stack.docx")
	fixturePDF(t, input, 1, false, func(_ string) string {
		return "BT /F1 12 Tf 50 740 Td (Gallery) Tj ET\n" + strings.Repeat("q 300 0 0 300 100 300 cm /Im1 Do Q\n", 19)
	})
	report, err := Convert(context.Background(), input, output, opts)
	if err != nil || report.Images != 1 {
		t.Fatalf("%d images for one stacked picture: %v", report.Images, err)
	}
}

// Placed images and drawn art together stay within the page image limit.
func TestImagesAndArtShareTheImageLimit(t *testing.T) {
	opts := integrationOptions(t)
	dir := t.TempDir()
	input, output := filepath.Join(dir, "many.pdf"), filepath.Join(dir, "many.docx")
	fixturePDF(t, input, 1, false, func(_ string) string {
		var b strings.Builder
		b.WriteString("BT /F1 10 Tf 40 770 Td (Catalogue) Tj ET\n")
		for i := 0; i < 60; i++ {
			fmt.Fprintf(&b, "q 30 0 0 30 %d %d cm /Im1 Do Q\n", 20+(i%10)*58, 600-(i/10)*40)
		}
		for i := 0; i < 45; i++ {
			fmt.Fprintf(&b, "%d %d 20 20 re f\n", 20+(i%10)*58, 300-(i/10)*40)
		}
		return b.String()
	})
	if report, err := Convert(context.Background(), input, output, opts); err != nil || report.Images > 100 {
		t.Fatalf("%v %+v", err, report)
	}
}

// Thousands of overlapping fills cluster in bounded time.
func TestOverlappingPathsClusterQuickly(t *testing.T) {
	opts := integrationOptions(t)
	opts.Timeout = 20 * time.Second
	dir := t.TempDir()
	input, output := filepath.Join(dir, "paths.pdf"), filepath.Join(dir, "paths.docx")
	fixturePDF(t, input, 1, false, func(_ string) string {
		var b strings.Builder
		b.WriteString("BT /F1 10 Tf 40 770 Td (Hello) Tj ET\n")
		for i := 0; i < 8000; i++ {
			fmt.Fprintf(&b, "%d %d 300 300 re f\n", 50+i%7, 100+i%5)
		}
		return b.String()
	})
	if _, err := Convert(context.Background(), input, output, opts); err != nil {
		t.Fatal(err)
	}
}

// A page scaled down to Word's limit lands on it exactly.
func TestOversizePageScalesToTheLimit(t *testing.T) {
	p := Page{Width: 2592, Height: 1735, Spans: []Span{span("Plan", 100, 100, 30)}}
	p.sanitize()
	if err := p.validate(); err != nil {
		t.Fatal(err)
	}
}

// A per-cell bottom border under text that fills the cell is a table rule,
// not an underline.
func TestCellBordersAreNotUnderlines(t *testing.T) {
	xs, ys := []float64{50, 110, 170, 230}, []float64{100, 115, 130, 145}
	var segs []Segment
	var spans []Span
	for r := 0; r < 3; r++ {
		for c := 0; c < 3; c++ {
			segs = append(segs, Segment{xs[c], ys[r], xs[c+1], ys[r]}, Segment{xs[c], ys[r+1], xs[c+1], ys[r+1]}, Segment{xs[c], ys[r], xs[c], ys[r+1]}, Segment{xs[c+1], ys[r], xs[c+1], ys[r+1]})
			spans = append(spans, Span{Box: Box{xs[c] + 3, ys[r+1] - 11, 54, 10}, Text: fmt.Sprintf("1,234,%d%d", r, c), Font: "Arial", Color: "000000", Size: 10, Baseline: ys[r+1] - 3})
		}
	}
	l, err := layout(Page{Width: 612, Height: 792, Spans: spans, Segments: segs})
	if err != nil || len(l.Tables) != 1 {
		t.Fatalf("tables=%d %v", len(l.Tables), err)
	}
	for r, row := range l.Tables[0].Rows {
		for c, cell := range row {
			if len(cell.Spans) != 1 || cell.Spans[0].Text != fmt.Sprintf("1,234,%d%d", r, c) || cell.Spans[0].Underline {
				t.Fatalf("cell %d,%d = %+v", r, c, cell)
			}
		}
	}
}

// Each image is its own region of the page render, encoded as a PNG Word
// can decode: one image renders alone, several share one page render.
func TestImagePixelsMatchTheirRegions(t *testing.T) {
	opts := integrationOptions(t)
	for _, count := range []int{1, 2} {
		dir := t.TempDir()
		input, output := filepath.Join(dir, "pics.pdf"), filepath.Join(dir, "pics.docx")
		fixturePDF(t, input, 1, false, func(_ string) string {
			b := "BT /F1 12 Tf 50 740 Td (Pictures) Tj ET\n"
			for i := 0; i < count; i++ {
				b += fmt.Sprintf("q 200 0 0 200 %d 300 cm /Im1 Do Q\n", 60+i*260)
			}
			return b
		})
		if _, err := Convert(context.Background(), input, output, opts); err != nil {
			t.Fatal(err)
		}
		archive, err := zip.OpenReader(output)
		if err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, f := range archive.File {
			if !strings.HasSuffix(f.Name, ".png") {
				continue
			}
			found++
			r, _ := f.Open()
			img, err := png.Decode(r)
			r.Close()
			if err != nil {
				t.Fatalf("%s: %v", f.Name, err)
			}
			// Im1's rows, top first: red, green / blue, yellow.
			bounds := img.Bounds()
			want := map[[2]int][3]uint32{{1, 1}: {1, 0, 0}, {3, 1}: {0, 1, 0}, {1, 3}: {0, 0, 1}, {3, 3}: {1, 1, 0}}
			for at, rgb := range want {
				r, g, b, _ := img.At(bounds.Dx()*at[0]/4, bounds.Dy()*at[1]/4).RGBA()
				if got := [3]uint32{r >> 15, g >> 15, b >> 15}; got != rgb {
					t.Errorf("%d images, %s at %v: %v, want %v", count, f.Name, at, got, rgb)
				}
			}
		}
		archive.Close()
		if found != count {
			t.Fatalf("%d PNGs for %d images", found, count)
		}
	}
}

// A footer that counts pages becomes one footer part with a PAGE field, and a
// constant head one header part; the body keeps only the page's own text.
func TestRunningHeadsAndFeet(t *testing.T) {
	page := func(i int, foot string) Page {
		p := Page{Width: 612, Height: 792, Spans: []Span{span("CONFIDENTIAL", 250, 20, 90), span(fmt.Sprintf("Body text of section %c.", 'A'+i), 72, 300, 160)}}
		if foot != "" {
			p.Spans = append(p.Spans, span(foot, 280, 760, 50))
		}
		return p
	}
	for _, c := range []struct {
		name  string
		pages []Page
		start string
	}{
		{"numbered from one", []Page{page(0, "Page 1"), page(1, "Page 2"), page(2, "Page 3")}, `<w:pgNumType w:start="1"/>`},
		{"unnumbered cover", []Page{page(0, ""), page(1, "Page 1"), page(2, "Page 2"), page(3, "Page 3")}, `<w:pgNumType w:start="0"/>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			if _, err := Write(&out, c.pages); err != nil {
				t.Fatal(err)
			}
			parts := docParts(t, out.Bytes())
			doc := parts["word/document.xml"]
			last := fmt.Sprintf("section %c.", 'A'+len(c.pages)-1)
			if strings.Contains(doc, "CONFIDENTIAL") || strings.Contains(doc, "Page ") || !strings.Contains(doc, last) {
				t.Fatalf("running text left in the body: %s", doc)
			}
			if strings.Count(doc, "<w:headerReference") != len(c.pages) || strings.Count(doc, "<w:footerReference") != len(c.pages) || !strings.Contains(doc, c.start) {
				t.Fatalf("sections: %s", doc)
			}
			var heads, feet int
			for name, xml := range parts {
				heads += strings.Count(xml, "CONFIDENTIAL")
				if strings.HasPrefix(name, "word/footer") && strings.Contains(xml, `<w:fldSimple w:instr=" PAGE ">`) && strings.Contains(xml, ">Page </w:t>") {
					feet++
				}
			}
			if heads != 1 || feet != 1 {
				t.Fatalf("%d header parts with the head, %d footers with a PAGE field", heads, feet)
			}
			if !strings.Contains(parts["[Content_Types].xml"], "wordprocessingml.footer+xml") {
				t.Fatal("footer content type missing")
			}
			if opts := integrationOptions(t); opts.Validate != nil {
				path := filepath.Join(t.TempDir(), "running.docx")
				if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := opts.Validate(context.Background(), path); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Each case broke running-line detection once: the head or foot must leave
// the body only when every page agrees, and nothing may be lost or doubled.
func TestRunningLineEdgeCases(t *testing.T) {
	doc := func(t *testing.T, pages []Page) map[string]string {
		t.Helper()
		var out bytes.Buffer
		if _, err := Write(&out, pages); err != nil {
			t.Fatal(err)
		}
		return docParts(t, out.Bytes())
	}
	pageWith := func(extra ...Span) Page {
		return Page{Width: 612, Height: 792, Spans: append([]Span{span("Body text.", 72, 300, 60)}, extra...)}
	}
	everywhere := func(parts map[string]string, text string) (body, margins int) {
		for name, xml := range parts {
			n := strings.Count(xml, text)
			if name == "word/document.xml" {
				body += n
			} else if strings.HasPrefix(name, "word/header") || strings.HasPrefix(name, "word/footer") {
				margins += n
			}
		}
		return
	}
	t.Run("literal # beside numbers stays in the body", func(t *testing.T) {
		for _, feet := range [][]string{{"Item 5", "Item #", "Item 5"}, {"Item #", "Item 5", "Item 7"}} {
			var pages []Page
			for _, f := range feet {
				pages = append(pages, pageWith(span(f, 280, 760, 40)))
			}
			parts := doc(t, pages)
			for _, f := range feet {
				if !strings.Contains(parts["word/document.xml"], ">"+f+"<") {
					t.Fatalf("%v: %q left the document", feet, f)
				}
			}
		}
	})
	t.Run("a header table keeps its text", func(t *testing.T) {
		var pages []Page
		for range 3 {
			p := pageWith(span("SOP-001 Rev 2", 80, 20, 100))
			p.Segments = []Segment{{70, 15, 70, 40}, {300, 15, 300, 40}, {70, 15, 300, 15}, {70, 40, 300, 40}}
			pages = append(pages, p)
		}
		if body, _ := everywhere(doc(t, pages), "SOP-001"); body != 3 {
			t.Fatalf("header table text in the body %d times, want 3", body)
		}
	})
	t.Run("a raised mark moves with its head", func(t *testing.T) {
		var pages []Page
		for range 3 {
			mark := Span{Box: Box{171, 18, 10, 7}, Text: "TM", Font: "Arial", Color: "000000", Size: 6, Baseline: 24}
			pages = append(pages, pageWith(span("Acme Widgets", 100, 20, 70), mark))
		}
		if body, margins := everywhere(doc(t, pages), ">TM<"); body != 0 || margins != 1 {
			t.Fatalf("TM in body %d, in margins %d", body, margins)
		}
	})
	t.Run("an underlined head stays underlined", func(t *testing.T) {
		var pages []Page
		for range 3 {
			p := pageWith(span("CONFIDENTIAL", 250, 20, 90))
			p.Segments = []Segment{{250, 31.5, 340, 31.5}}
			pages = append(pages, p)
		}
		for name, xml := range doc(t, pages) {
			if strings.HasPrefix(name, "word/header") && strings.Contains(xml, "CONFIDENTIAL") && !strings.Contains(xml, `<w:u w:val="single"/>`) {
				t.Fatal("underline lost")
			}
		}
	})
	t.Run("a mirrored head keeps its side", func(t *testing.T) {
		var pages []Page
		for i := range 4 {
			pages = append(pages, pageWith(span("Chapter One", []float64{72, 450}[i%2], 20, 80)))
		}
		lefts := 0
		for name, xml := range doc(t, pages) {
			if strings.HasPrefix(name, "word/header") && (strings.Contains(xml, `w:left="1440"`) || strings.Contains(xml, `w:left="9000"`)) {
				lefts++
			}
		}
		if lefts != 2 {
			t.Fatalf("%d header positions, want 2", lefts)
		}
	})
	t.Run("a number split across spans is not a page field", func(t *testing.T) {
		var pages []Page
		for i := range 3 {
			n := fmt.Sprint(10 + i)
			pages = append(pages, pageWith(span("Page "+n[:1], 280, 760, 30), span(n[1:], 310, 760, 5)))
		}
		if strings.Contains(strings.Join(slices.Collect(maps.Values(doc(t, pages))), ""), "fldSimple") {
			t.Fatal("PAGE field from a split number")
		}
	})
}

// Stored images carry their sizes in the local header (no data descriptor),
// which streaming ZIP readers require, and every entry states ZIP 2.0, which
// Word requires.
func TestStoredImagesHaveLocalSizes(t *testing.T) {
	p := Page{Width: 612, Height: 792, Spans: []Span{span("Logo", 72, 72, 40)}, Images: []Image{{Box: Box{72, 100, 10, 10}, PNG: pngPixel(t)}}}
	var out bytes.Buffer
	if _, err := Write(&out, []Page{p}); err != nil {
		t.Fatal(err)
	}
	z, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range z.File {
		if strings.HasSuffix(f.Name, ".png") && (f.Method != zip.Store || f.Flags&0x8 != 0) {
			t.Fatalf("%s: method %d flags %#x", f.Name, f.Method, f.Flags)
		}
		// Word reports "unreadable content" for an entry needing version 0.
		if f.ReaderVersion < 20 || f.CreatorVersion&0xff < 20 {
			t.Fatalf("%s: versions %d/%d, Word needs 2.0", f.Name, f.CreatorVersion, f.ReaderVersion)
		}
	}
}

func pngPixel(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// A numbered heading or contents entry keeps its tab: the text after the
// number starts at its source x, not one space along.
func TestLeadingNumberTab(t *testing.T) {
	for _, c := range []struct {
		number, text string
		gap          float64
		tab          bool
	}{{"3", "Beginning of the paragraph", 16, true}, {"1", "Do not shorten", 13, true}, {"The", "quick brown fox", 4, false}} {
		number := span(c.number, 72, 100, 8)
		text := span(c.text, 80+c.gap, 100, 150)
		var out bytes.Buffer
		if _, err := Write(&out, []Page{{Width: 612, Height: 792, Spans: []Span{number, text}}}); err != nil {
			t.Fatal(err)
		}
		doc := docParts(t, out.Bytes())["word/document.xml"]
		if got := strings.Contains(doc, "<w:tab/>") && strings.Contains(doc, fmt.Sprintf(`w:pos="%d"`, twips(80+c.gap))); got != c.tab {
			t.Fatalf("%q + %.0fpt gap: tab %v, want %v: %s", c.number, c.gap, got, c.tab, doc)
		}
	}
}

// Justification stretches every word gap alike: the first one, however
// wide, stays a space when the line's other gaps are as wide.
func TestJustifiedFirstGapIsASpace(t *testing.T) {
	var spans []Span
	x := 72.0
	for _, word := range []string{"$532.16", "per", "month", "toward", "the", "premium"} {
		spans = append(spans, span(word, x, 100, 6*float64(len(word))))
		x += 6*float64(len(word)) + 15.8
	}
	var out bytes.Buffer
	if _, err := Write(&out, []Page{{Width: 612, Height: 792, Spans: spans}}); err != nil {
		t.Fatal(err)
	}
	if doc := docParts(t, out.Bytes())["word/document.xml"]; strings.Contains(doc, "<w:tab/>") {
		t.Fatalf("justified word gap written as a tab: %s", doc)
	}
}

// A one-line numbered heading between two paragraph gaps stands alone, even
// when the next paragraph starts at the heading text's hanging indent.
func TestHeadingBetweenParagraphGaps(t *testing.T) {
	at := func(text string, x, baseline, w float64) Span {
		s := span(text, x, baseline-10, w)
		s.Size = 11
		return s
	}
	var spans []Span
	spans = append(spans, at("ending of the previous paragraph that runs full", 113, 100, 425), at("width to the right margin and stops here.", 113, 115, 300))
	spans = append(spans, at("2.6", 85, 145, 14), at("Extension of Termination Effective Date", 113, 145, 190))
	for i, text := range []string{"Customer may extend the effective date of termination or the end of the", "Initial Term or the Optional Period one or more times as it elects, at its", "sole discretion."} {
		spans = append(spans, at(text, 113, 170+15*float64(i), []float64{425, 425, 70}[i]))
	}
	var out bytes.Buffer
	if _, err := Write(&out, []Page{{Width: 612, Height: 792, Spans: spans}}); err != nil {
		t.Fatal(err)
	}
	doc := docParts(t, out.Bytes())["word/document.xml"]
	heading := doc[strings.Index(doc, "Extension of"):]
	if end := strings.Index(heading, "</w:p>"); strings.Contains(heading[:end], "Customer") {
		t.Fatal("heading merged into the next paragraph")
	}
}

// In a justified column every line but a paragraph's last reaches the edge,
// so a short line ends its paragraph even when the next line's first word
// ("Article") would not have fit after it.
func TestJustifiedShortLineEndsParagraph(t *testing.T) {
	at := func(text string, baseline, w float64) Span {
		s := span(text, 57, baseline-10, w)
		s.Size = 12
		return s
	}
	var spans []Span
	for i, text := range []string{"All are equal before the law and are entitled without any discrimination", "to equal protection of the law. All are entitled to equal protection", "against any incitement to such discrimination.", "Article 8", "Everyone has the right to an effective remedy by the competent national", "tribunals for acts violating the fundamental rights granted him by law."} {
		spans = append(spans, at(text, 100+16*float64(i), []float64{482, 482, 442, 49, 482, 300}[i]))
	}
	pages := []Page{{Width: 612, Height: 792, Spans: spans}}
	var out bytes.Buffer
	if _, err := Write(&out, pages); err != nil {
		t.Fatal(err)
	}
	doc := docParts(t, out.Bytes())["word/document.xml"]
	heading := doc[:strings.Index(doc, "Article 8")]
	if !strings.Contains(heading[strings.LastIndex(heading, "<w:p>"):], "</w:pPr>") || strings.Contains(heading[strings.LastIndex(heading, "<w:p>"):], "discrimination.") {
		t.Fatal("heading merged into the justified paragraph above it")
	}
	// A short line mid-sentence is a manual break, not a paragraph end.
	spans[2].Text = "against any incitement to such discrimination up to"
	spans[3].Text, spans[3].Width = "notwithstanding the rest", 130
	out.Reset()
	if _, err := Write(&out, pages); err != nil {
		t.Fatal(err)
	}
	doc = docParts(t, out.Bytes())["word/document.xml"]
	if para := doc[strings.LastIndex(doc[:strings.Index(doc, "notwithstanding")], "<w:p>"):]; !strings.Contains(para[:strings.Index(para, "notwithstanding")], "All are equal") {
		t.Fatal("a manual line break mid-sentence ended the paragraph")
	}
}

// Display lines set tighter than 1.2 times their size keep their source
// pitch: a guessed taller line pushes every line after it down the page.
func TestTightSingleLinesKeepTheirPitch(t *testing.T) {
	var spans []Span
	for i, size := range []float64{14, 7.5, 14, 7.5} {
		s := span(fmt.Sprintf("Display line %d", i), 100, 100+11*float64(i)-size, 8*size)
		s.Size, s.Height, s.Baseline = size, size, 100+11*float64(i)
		spans = append(spans, s)
	}
	var out bytes.Buffer
	if _, err := Write(&out, []Page{{Width: 612, Height: 792, Spans: spans}}); err != nil {
		t.Fatal(err)
	}
	doc := docParts(t, out.Bytes())["word/document.xml"]
	// Word puts an exact line's baseline 80% down its box (baselineDepth).
	top, k := 0, 0
	for _, m := range regexp.MustCompile(`w:before="(\d+)" w:after="0" w:line="(\d+)"`).FindAllStringSubmatch(doc, -1) {
		before, _ := strconv.Atoi(m[1])
		line, _ := strconv.Atoi(m[2])
		top += before
		if line > 1 {
			if got, want := float64(top)/20+0.8*float64(line)/20, spans[k].Baseline; math.Abs(got-want) > 0.3 {
				t.Fatalf("line %d baseline at %.2fpt in Word, %.2fpt in the source: %s", k, got, want, doc)
			}
			k++
		}
		top += line
	}
}

// Drawn leader dots before a closing page number become a right tab with a
// dot leader at the number's edge: drawn dots in Word's metrics run long and
// wrap the number onto a line of its own.
func TestLeaderDotsBecomeATab(t *testing.T) {
	for _, spans := range [][]Span{
		{span("1.2", 84, 100, 19), span("Organizational Background ", 120, 100, 148), span("........................................", 267, 100, 280), span("5", 544, 100, 7)},
		{span("1", 72, 100, 9), span("Background .............................", 104, 100, 443), span("4", 544, 100, 7)},
	} {
		var out bytes.Buffer
		if _, err := Write(&out, []Page{{Width: 612, Height: 792, Spans: spans}}); err != nil {
			t.Fatal(err)
		}
		doc := docParts(t, out.Bytes())["word/document.xml"]
		if strings.Contains(doc, "....") || !strings.Contains(doc, `<w:tab w:val="right" w:leader="dot"`) {
			t.Fatalf("leader not a dot-leader tab at the number: %s", doc)
		}
	}
	// A contents page's entries stay one paragraph each: joined, Word
	// wrapped each entry's title onto the previous entry's line.
	var spans []Span
	for i, title := range []string{"Transitional services ", "Transition Period", "Termination", "Description of Services", "General Limitations."} {
		y := 100 + 22*float64(i)
		spans = append(spans, span(fmt.Sprint(i+1)+" ", 72, y, 8), span(title, 94, y, 7*float64(len(title))), span(strings.Repeat(".", 60), 94+7*float64(len(title)), y, 440-7*float64(len(title))), span(fmt.Sprint(i+2), 534, y, 6))
	}
	var out bytes.Buffer
	if _, err := Write(&out, []Page{{Width: 612, Height: 792, Spans: spans}}); err != nil {
		t.Fatal(err)
	}
	// and each title keeps its tab after the number.
	doc := docParts(t, out.Bytes())["word/document.xml"]
	if n, tabs := strings.Count(doc, `w:leader="dot"`), strings.Count(doc, `<w:tab/>`); n != 5 || tabs != 10 {
		t.Fatalf("%d contents paragraphs with a leader tab and %d tabs, want 5 and 10", n, tabs)
	}
}

// A ragged paragraph's column ends short of where any line would take the
// next line's first word: a line shorter than the widest has more room, and
// Word pulled "tenure," up into it.
func TestRaggedColumnPullsNoWordUp(t *testing.T) {
	var ls []Line
	for i, c := range []struct {
		text  string
		width float64
	}{{"General. The population is varied and heavily staffed by XXX. Nearly", 490}, {"have many employees who are sophisticated in computing, pressured by their short", 452}, {"tenure, and must learn our environment quickly in order to be productive. We", 472}, {"tools. Although the ultimate goal is to provide a standardized desktop computing", 498}} {
		ls = append(ls, Line{Spans: []Span{span(c.text, 54, 100+14*float64(i), c.width)}, X: 54, Baseline: 110 + 14*float64(i), Width: c.width, Height: 12})
	}
	for _, p := range paragraphs(ls, 54, 552) {
		for i := 0; i+1 < len(p.Lines); i++ {
			if room := p.Right - p.Lines[i].right(); room >= p.Lines[i+1].firstWord() {
				t.Fatalf("line %d has %.1fpt of room for the next line's %.1fpt first word", i, room, p.Lines[i+1].firstWord())
			}
		}
	}
}

// A typesetter shrinks word gaps to fit a line; the space Word gets between
// two word spans is the source gap wide, not Word's own space, or the line
// runs long and rewraps.
func TestSpaceBetweenSpansKeepsTheGap(t *testing.T) {
	a, b := span("arbitrarily", 72, 100, 52), span("deprived", 126.5, 100, 40) // a 2.5pt gap
	a.Font, b.Font = "Helvetica", "Helvetica"
	var out bytes.Buffer
	if _, err := Write(&out, []Page{{Width: 612, Height: 792, Spans: []Span{a, b}}}); err != nil {
		t.Fatal(err)
	}
	doc := docParts(t, out.Bytes())["word/document.xml"]
	m := regexp.MustCompile(`<w:spacing w:val="(-?\d+)"/>(?:<w:[^>]*>)*</w:rPr><w:t xml:space="preserve"> </w:t>`).FindStringSubmatch(doc)
	if m == nil {
		t.Fatalf("no tracked space between the spans: %s", doc)
	}
	v, _ := strconv.Atoi(m[1])
	if got := float64(advance(faceAdvances["Arial"], ' '))/1000*a.Size + float64(v)/20; math.Abs(got-2.5) > 0.05 {
		t.Fatalf("space %.2fpt wide in Word, source gap 2.5pt", got)
	}
}

// A face Word has but whose widths are unknown (Verdana) still writes its
// spaces and leaders: their widths fall back, they never index a table.
func TestUnknownFaceMetricsFallBack(t *testing.T) {
	a, b, n := span("Name", 72, 100, 30), span(".........", 110, 100, 300), span("12", 420, 100, 12)
	a.Font, b.Font, n.Font = "Verdana", "Verdana", "Verdana"
	var out bytes.Buffer
	if _, err := Write(&out, []Page{{Width: 612, Height: 792, Spans: []Span{a, span("word", 107, 100, 1), b, n}}}); err != nil {
		t.Fatal(err)
	}
	docParts(t, out.Bytes())
}

// A justified line drawn as one span shows its stretch in the span's
// spaces: wider than Word draws them. A two-line paragraph stays justified.
func TestInSpanStretchIsJustified(t *testing.T) {
	text := "Lorem ipsum dolor sit amet, consectetuer adipiscing elit. Maecenas porttitor"
	first := span(text, 54, 100, emsOf(text, "Times New Roman")*12+9*1.6) // nine spaces, 1.6pt each wider
	first.Font, first.Size = "TimesNewRomanPSMT", 12
	second := span("ultricies, purus lectus malesuada libero.", 54, 114, 200)
	second.Font, second.Size = "TimesNewRomanPSMT", 12
	var out bytes.Buffer
	if _, err := Write(&out, []Page{{Width: 612, Height: 792, Spans: []Span{first, second}}}); err != nil {
		t.Fatal(err)
	}
	if doc := docParts(t, out.Bytes())["word/document.xml"]; !strings.Contains(doc, `<w:jc w:val="both"/>`) {
		t.Fatalf("stretched paragraph not justified: %s", doc)
	}
}

// Word stretches a justified line to the paragraph's right edge, so every
// justified paragraph ends at its column edge: a two-line one too.
func TestJustifiedParagraphsEndAtTheEdge(t *testing.T) {
	// Lines drawn a little narrower than Word's natural width (no stretch to
	// read from the spaces), so only the column shows the justification.
	text := "Everyone has the right to form and to join trade unions for the protection"
	full := emsOf(text, "Arial") * 12 * 0.995
	var ls []Line
	for i, w := range []float64{full, full, full, 300, full, 25, full, full, 200} {
		s := span(text, 56, 60+16*float64(i), w)
		s.Size = 12
		ls = append(ls, Line{Spans: []Span{s}, X: 56, Baseline: 70 + 16*float64(i), Width: w, Height: 12})
	}
	edge := 56 + full
	for _, p := range paragraphs(ls, 56, edge) {
		if p.Align == "both" && p.Right > edge+0.5 {
			t.Fatalf("justified %d-line paragraph ends at %.1f, the column at %.1f", len(p.Lines), p.Right, edge)
		}
	}
}

// A link Word cannot carry (a bare % in the URL) costs the link, not the
// document.
func TestMalformedLinkKeepsItsText(t *testing.T) {
	bad := span("new arrivals", 72, 100, 60)
	bad.Link = "http://example.org/100%new/item.html"
	good := span("home", 72, 120, 30)
	good.Link = "https://example.org/"
	p := Page{Width: 612, Height: 792, Spans: []Span{bad, good}}
	notes := p.sanitize()
	if err := p.validate(); err != nil || p.Spans[0].Link != "" || p.Spans[0].Text != "new arrivals" || p.Spans[1].Link != good.Link || len(notes) != 1 {
		t.Fatalf("validate %v, spans %+v, notes %q", err, p.Spans, notes)
	}
}

// A tab drawn in the PDF becomes a tab to a stop at the next text's x, not a
// space, even across a gap narrower than a space-or-tab guess would allow.
func TestDrawnTabReachesItsStop(t *testing.T) {
	population := span("Population", 268, 100, 90)
	population.Tab = true
	words := span("then words", 362, 100, 60)
	var out bytes.Buffer
	if _, err := Write(&out, []Page{{Width: 612, Height: 792, Spans: []Span{span("Human", 200, 100, 62), population, words}}}); err != nil {
		t.Fatal(err)
	}
	doc := docParts(t, out.Bytes())["word/document.xml"]
	if strings.Count(doc, "<w:tab/>") != 1 || !strings.Contains(doc, fmt.Sprintf(`w:pos="%d"`, twips(268))) {
		t.Fatalf("tab not reproduced as a stop at 268pt: %s", doc)
	}
}

// Word lays a font it lacks out in a substitute; the writer names one whose
// widths it knows and scales the span to its source width, so lines break
// where the PDF's did. Fonts Word has stay as they are.
func TestSubstituteFontsKeepSourceWidths(t *testing.T) {
	text := "Proclamation of the Loan"
	for _, c := range []struct {
		font  string
		width float64 // source width as a share of Arial Bold's
	}{{"DejaVuSans-Bold", 1.12}, {"ABCDEF+Frutiger-Bold", 0.95}, {"Arial-BoldMT", 1.005}} {
		s := span(text, 72, 100, c.width*emsOf(text, "Arial|b")*11)
		s.Font, s.Weight = c.font, 700
		if word := wordWidth(t, s, "Arial|b"); math.Abs(word-s.Width) > 0.005*s.Width {
			t.Fatalf("%s: Word width %.2f, source %.2f", c.font, word, s.Width)
		}
	}
}

// Word draws a 6.81pt span at 7pt (half points); the scale and tracking it
// gets must still give it its source width, or every line of small print
// wraps a word early.
// wordWidth is the width Word lays out a one-span page's text in, from the
// family's advances and the run's size, scale and tracking.
func wordWidth(t *testing.T, s Span, face string) float64 {
	t.Helper()
	var out bytes.Buffer
	if _, err := Write(&out, []Page{{Width: 612, Height: 792, Spans: []Span{s}}}); err != nil {
		t.Fatal(err)
	}
	doc := docParts(t, out.Bytes())["word/document.xml"]
	attr := func(tag string, fallback float64) float64 {
		if m := regexp.MustCompile(`<w:` + tag + ` w:val="(-?\d+)"/>`).FindStringSubmatch(doc); m != nil {
			v, _ := strconv.ParseFloat(m[1], 64)
			return v
		}
		return fallback
	}
	if !strings.Contains(doc, `w:ascii="`+strings.Split(face, "|")[0]+`"`) {
		t.Fatalf("not laid out in %s: %s", face, doc)
	}
	ems, n := 0.0, 0.0
	for _, r := range s.Text {
		ems += float64(advance(faceAdvances[face], r)) / 1000
		n++
	}
	return ems*attr("sz", 0)/2*attr("w", 100)/100 + n*attr("spacing", 0)/20
}

func emsOf(text, face string) float64 {
	ems := 0.0
	for _, r := range text {
		ems += float64(advance(faceAdvances[face], r)) / 1000
	}
	return ems
}

func TestHalfPointSizeKeepsSourceWidth(t *testing.T) {
	s := span("means the Revolving Commitments of all", 53, 100, emsOf("means the Revolving Commitments of all", "Courier New")*6.81)
	s.Font, s.Size, s.Mono = "DejaVuSansMono", 6.81, true
	if word := wordWidth(t, s, "Courier New"); math.Abs(word-s.Width) > 0.005*s.Width {
		t.Fatalf("Word width %.2f, source %.2f", word, s.Width)
	}
}

// A span within 2% of Word's width (a source "ﬁ" ligature is narrower than
// "f" and "i") keeps its source width through tracking, not a scale: else a
// full line's last word wraps, and on a full page spills onto a new one.
func TestNearWidthsTrackToSourceWidth(t *testing.T) {
	text := "until the Class Certificates"
	s := span(text, 72, 100, emsOf(text, "Times New Roman")*12-2) // three ligatures' worth narrower
	s.Font, s.Size = "TimesNewRomanPSMT", 12
	if word := wordWidth(t, s, "Times New Roman"); math.Abs(word-s.Width) > 0.005*s.Width {
		t.Fatalf("Word width %.2f, source %.2f", word, s.Width)
	}
	// Wider with spaces is a justified line's stretched spaces: Word's
	// justification redraws those, and tracking them too overfills the line.
	s.Width = emsOf(text, "Times New Roman")*12 + 2
	if word := wordWidth(t, s, "Times New Roman"); math.Abs(word-emsOf(text, "Times New Roman")*12) > 0.01 {
		t.Fatalf("justified stretch tracked: Word width %.2f", word)
	}
}

// reader.mjs needs Recto's per-glyph scales (0.8.1) and text-free renders (0.8.2).
func TestRectoVersion(t *testing.T) {
	for v, ok := range map[string]bool{"0.8.0": false, "0.8.1": false, "0.8.2": true, "0.8.12": true, "0.8.3-rc.1": true, "0.9.0": false, "0.7.1": false, "0.8.01": false} {
		if rectoVersion.MatchString(v) != ok {
			t.Errorf("%s: accepted %v, want %v", v, !ok, ok)
		}
	}
}

// Faux italic (a slant in the text matrix) widens each glyph's box, not its
// advance: the slanted line is one span as wide as the upright one.
// A shaded panel under text keeps its look as an image drawn without that
// text (Word draws the text over it), not dropped and not doubled.
func TestShadingUnderTextIsArtWithoutText(t *testing.T) {
	opts := integrationOptions(t)
	input := filepath.Join(t.TempDir(), "shaded.pdf")
	fixturePDF(t, input, 1, false, func(_ string) string {
		return "0.8 g 40 680 300 40 re f 0 g BT /F1 14 Tf 50 695 Td (Shaded cell text) Tj ET\n"
	})
	pages, err := Extract(context.Background(), input, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages[0].Images) != 1 {
		t.Fatalf("want the panel as one image: %+v", pages[0].Images)
	}
	im, err := png.Decode(bytes.NewReader(pages[0].Images[0].PNG))
	if err != nil {
		t.Fatal(err)
	}
	b := im.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if r, _, _, _ := im.At(x, y).RGBA(); r < 0x8000 {
				t.Fatalf("panel image has the text drawn into it at %d,%d", x, y)
			}
		}
	}
}

// An annotation's appearance (a stamped signature) keeps its look as an
// image, drawn without the page text under it.
func TestStampedSignatureIsKept(t *testing.T) {
	opts := integrationOptions(t)
	input := filepath.Join(t.TempDir(), "stamp.pdf")
	content := "BT /F1 12 Tf 80 640 Td (Lucy Director) Tj ET\n"
	stamp := "0 g 2 w 10 10 m 150 60 l S\n"
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Count 1 /Kids [3 0 R] >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R /Annots [6 0 R] >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content),
		"<< /Type /Annot /Subtype /Stamp /Rect [70 620 230 700] /AP << /N 7 0 R >> >>",
		fmt.Sprintf("<< /Type /XObject /Subtype /Form /BBox [0 0 160 80] /Length %d >>\nstream\n%sendstream", len(stamp), stamp),
	}
	writePDF(t, input, objects)
	pages, err := Extract(context.Background(), input, opts)
	if err != nil {
		t.Fatal(err)
	}
	var stampImage []byte
	for _, im := range pages[0].Images {
		if math.Abs(im.X-70) < 1 && math.Abs(im.Width-160) < 1 {
			stampImage = im.PNG
		}
	}
	if stampImage == nil {
		t.Fatalf("stamp not kept as an image: %+v", pages[0].Images)
	}
	im, err := png.Decode(bytes.NewReader(stampImage))
	if err != nil {
		t.Fatal(err)
	}
	dark := 0
	for y := im.Bounds().Min.Y; y < im.Bounds().Max.Y; y++ {
		for x := im.Bounds().Min.X; x < im.Bounds().Max.X; x++ {
			if r, _, _, _ := im.At(x, y).RGBA(); r < 0x8000 {
				dark++
			}
		}
	}
	if dark == 0 {
		t.Fatal("stamp image is blank")
	}
}

// A ligature's letters share one glyph box; they stay one span at their
// source width, not a second span stretched back over the ligature.
func TestLigatureIsOneSpan(t *testing.T) {
	opts := integrationOptions(t)
	input := filepath.Join(t.TempDir(), "ligature.pdf")
	fixturePDF(t, input, 1, false, func(_ string) string {
		return "BT /F1 12 Tf 50 700 Td (until the Class Certi\\256cates) Tj ET\n"
	})
	pages, err := Extract(context.Background(), input, opts)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(pages[0].Spans); n != 1 || !strings.HasSuffix(pages[0].Spans[0].Text, "cates") || !strings.Contains(pages[0].Spans[0].Text, "Certi") {
		t.Fatalf("want one span: %+v", pages[0].Spans)
	}
}

func TestFauxItalicKeepsAdvances(t *testing.T) {
	opts := integrationOptions(t)
	input := filepath.Join(t.TempDir(), "slant.pdf")
	fixturePDF(t, input, 1, false, func(_ string) string {
		return "BT /F1 14 Tf 1 0 0 1 50 700 Tm (WILL SELL THE PROPERTY) Tj ET\nBT /F1 14 Tf 1 0 0.3 1 50 650 Tm (WILL SELL THE PROPERTY) Tj ET\n"
	})
	pages, err := Extract(context.Background(), input, opts)
	if err != nil {
		t.Fatal(err)
	}
	widths := map[float64][]float64{}
	for _, s := range pages[0].Spans {
		if strings.Contains(s.Text, "PROPERTY") {
			widths[math.Round(s.Baseline)] = append(widths[math.Round(s.Baseline)], s.Width)
		}
	}
	upright, slanted := widths[92], widths[142]
	if len(upright) != 1 || len(slanted) != 1 || math.Abs(slanted[0]-upright[0]) > 0.02*upright[0] {
		t.Fatalf("upright %v, slanted %v (by baseline: %v)", upright, slanted, widths)
	}
}

// writePDF writes numbered objects (1 is the catalog) as a PDF file.
func writePDF(t *testing.T, path string, objects []string) {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	var offsets []int
	for i, obj := range objects {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, o := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer << /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Glyphs scaled far past the page (seen in the wild) neither exhaust the
// reader's heap (a rotated one is clipped before clustering) nor refuse the
// document (an upright one is left out).
func TestHugeRotatedGlyphIsClipped(t *testing.T) {
	opts := integrationOptions(t)
	input := filepath.Join(t.TempDir(), "huge.pdf")
	content := "BT /F1 12 Tf 80 640 Td (Body text) Tj ET\nBT /F1 1 Tf -100000 0 0 -100000 300 400 Tm (X) Tj ET\nBT /F1 1 Tf 100000 0 0 100000 -1000 -1000 Tm (Y) Tj ET\n"
	writePDF(t, input, []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Count 1 /Kids [3 0 R] >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content),
	})
	pages, err := Extract(context.Background(), input, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, im := range pages[0].Images {
		if im.X < 0 || im.Y < 0 || im.X+im.Width > 612.5 || im.Y+im.Height > 792.5 {
			t.Fatalf("image %+v leaves the page", im)
		}
	}
	var text string
	for _, sp := range pages[0].Spans {
		text += sp.Text
	}
	if !strings.Contains(text, "Body") || strings.Contains(text, "Y") {
		t.Fatalf("spans %+v: want the body text without the page-sized glyph", pages[0].Spans)
	}
}

// Sheet music's staff lines read as thousands of borders: the page keeps its
// text in the flow with a warning instead of refusing the document.
func TestTooManyBordersKeepTheText(t *testing.T) {
	p := Page{Width: 612, Height: 792, Spans: []Span{{Text: "Allegro", Box: Box{X: 72, Y: 60, Width: 40, Height: 12}, Baseline: 70, Size: 12, Font: "Helvetica", Color: "000000"}}}
	for i := range maxBorders + 1 {
		y := 100 + float64(i%600)
		p.Segments = append(p.Segments, Segment{X1: 72, Y1: y, X2: 540, Y2: y})
	}
	notes := p.sanitize()
	l, err := layout(p)
	if err != nil || len(l.Tables) != 0 || !strings.Contains(strings.Join(notes, " "), "Too many drawn lines") {
		t.Fatalf("layout %v, tables %d, notes %q", err, len(l.Tables), notes)
	}
}

// Past Recto's render limit (32 knockout groups) an image is approximate:
// the page converts with a warning instead of refusing the document.
func TestLayeredImagesAreApproximate(t *testing.T) {
	opts := integrationOptions(t)
	input := filepath.Join(t.TempDir(), "layers.pdf")
	content := "BT /F1 12 Tf 80 700 Td (Layers) Tj ET\n"
	for i := range 40 {
		content += fmt.Sprintf("q /G gs 1 0 0 1 %d 300 cm /X Do Q\n", 60+10*i)
	}
	group := "0.2 0.4 0.8 rg 0 0 40 40 re f 0.8 0.2 0.2 rg 10 10 40 40 re f\n"
	writePDF(t, input, []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Count 1 /Kids [3 0 R] >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> /XObject << /X 6 0 R >> /ExtGState << /G << /BM /Multiply /ca 0.5 >> >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content),
		fmt.Sprintf("<< /Type /XObject /Subtype /Form /BBox [0 0 60 60] /Group << /S /Transparency /K true >> /Length %d >>\nstream\n%sendstream", len(group), group),
	})
	pages, err := Extract(context.Background(), input, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages[0].Images) == 0 || !strings.Contains(strings.Join(pages[0].Warnings, " "), "approximate") {
		t.Fatalf("images %d, warnings %q", len(pages[0].Images), pages[0].Warnings)
	}
}

// A line that fills the column mid-sentence wraps onto the next, even when
// that line starts like a list item ("(30) days"); after a colon it is one.
func TestWrappedNumberIsNoListItem(t *testing.T) {
	for _, c := range []struct {
		last  string
		paras int
	}{{"cure such breach within thirty", 1}, {"the Provider shall do the following:", 2}} {
		var ls []Line
		for i, text := range []string{"Provider may terminate the Agreement if and only if Customer breaches in", "duty under the Agreement to pay undisputed charges, and fails to " + c.last, "(30) days after written notice."} {
			w := []float64{480, 480, 130}[i]
			ls = append(ls, Line{Spans: []Span{span(text, 72, 100+15*float64(i), w)}, X: 72, Baseline: 110 + 15*float64(i), Width: w, Height: 12})
		}
		if got := len(paragraphs(ls, 72, 552)); got != c.paras {
			t.Fatalf("after %q: %d paragraphs, want %d", c.last, got, c.paras)
		}
	}
}

// Word spreads a justified line's spare width over its spaces, so the width
// an 11.1pt size loses to half points is tracked back only on a ragged line:
// on a tightly set justified one it overfilled the line, and every later
// line rewrapped.
func TestJustifiedLinesTrackNoRoundingLoss(t *testing.T) {
	text := "which are in force and effect at the time of initiating arbitration"
	s := span(text, 72, 100, emsOf(text, "Times New Roman")*11.1+1)
	s.Font, s.Size = "TimesNewRomanPSMT", 11.1
	_, ragged, ok := wordFit(s, "Times New Roman", false)
	_, justified, _ := wordFit(s, "Times New Roman", true)
	if !ok || ragged <= 0 || justified != 0 {
		t.Fatalf("tracking ragged %.3f, justified %.3f: want the loss only on the ragged line", ragged, justified)
	}
}

// A justified line a hair wider in Word than in the source (-0.015pt a
// character, under a twip) is set narrower, not at Word's width: rounded to
// zero, a full line overfilled and Word wrapped its last word.
func TestJustifiedLinesRoundTrackingDown(t *testing.T) {
	text := "himself and of his family, including food, clothing, housing and medical care"
	var spans []Span
	for i := range 3 {
		s := span(text, 57, 100+16*float64(i), emsOf(text, "Times New Roman")*14-0.015*float64(len(text)-1))
		s.Font, s.Size = "TimesNewRomanPSMT", 14
		spans = append(spans, s)
	}
	var out bytes.Buffer
	if _, err := Write(&out, []Page{{Width: 612, Height: 792, Spans: spans}}); err != nil {
		t.Fatal(err)
	}
	doc := docParts(t, out.Bytes())["word/document.xml"]
	// The last line, which justification leaves as set, keeps its width.
	if !strings.Contains(doc, `<w:jc w:val="both"/>`) || strings.Count(doc, `<w:spacing w:val="-1"/>`) != 2 {
		t.Fatalf("want the two stretched lines set a twip narrower: %s", doc)
	}
}
