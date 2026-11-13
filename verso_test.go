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
				source = filepath.Join(dir, "scaled.pdf")
				fixturePDF(t, source, 1, false, func(c string) string { return strings.ReplaceAll(c, "Td (Review", "Td 150 Tz (Review") })
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

func TestTextTransformsRequireReview(t *testing.T) {
	opts := integrationOptions(t)
	opts.AllowWarnings = false
	for _, scale := range []int{50, 100, 150} {
		t.Run(strconv.Itoa(scale), func(t *testing.T) {
			dir := t.TempDir()
			input, output := filepath.Join(dir, "scaled.pdf"), filepath.Join(dir, "scaled.docx")
			fixturePDF(t, input, 1, false, func(_ string) string {
				return fmt.Sprintf("BT /F1 12 Tf 50 740 Td %d Tz (Horizontal scaling needs review.) Tj ET\n", scale)
			})
			report, err := Convert(context.Background(), input, output, opts)
			if scale == 100 {
				if err != nil || len(report.Warnings) > 0 {
					t.Fatalf("plain text needs no review: %+v %v", report, err)
				}
				return
			}
			if err == nil || !strings.Contains(strings.Join(report.Warnings, " "), "Horizontally scaled") {
				t.Fatalf("unchecked text transform published: scale=%d report=%+v error=%v", scale, report, err)
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatal("review refusal left output")
			}
		})
	}
}

// Letters that are wide by design (n in a bold monospace face) are not
// horizontally scaled text.
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
	for in, want := range map[string]string{"ABCDEF+TimesNewRomanPS-BoldMT": "Times New Roman", "ArialMT": "Arial", "Arial-Black": "Arial Black", "Arial,Bold": "Arial", "Helvetica-Oblique": "Arial", "SegoeUI-Semibold": "Segoe UI Semibold", "Calibri": "Calibri", "CourierNewPSMT": "Courier New", "Times-Roman": "Times New Roman", "BookAntiqua-Bold": "Book Antiqua", "STSong-Light-UniGB-UCS2-H": "STSong Light", "MSungStd-Light-Acro": "MSung Std Light", "ABCDEF+SomeVeryLongFontFamilyNameThatGoesOnAndOn": "Some Very Long Font Family Name", "DejaVuSans-Bold": "DejaVu Sans", "LiberationSerif": "Liberation Serif"} {
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
	os.WriteFile(filepath.Join(planted, "package.json"), []byte(`{"name":"@2018kguo/recto","version":"0.8.0"}`), 0o600)
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
// which streaming ZIP readers require.
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
