package verso

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"hash/crc32"
	"io"
	"maps"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const word = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
const relationships = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
const packageRelationships = "http://schemas.openxmlformats.org/package/2006/relationships"
const declaration = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`

func esc(s string) string { var b bytes.Buffer; _ = xml.EscapeText(&b, []byte(s)); return b.String() }
func twips(v float64) int { return int(math.Round(v * 20)) }

var subset = regexp.MustCompile(`^[A-Z]{6}\+`)
var camel = regexp.MustCompile(`([a-z])([A-Z])`)

// fontName turns a PostScript name into the family Word knows:
// "ABCDEF+TimesNewRomanPS-BoldMT" is "Times New Roman"; "Arial-Black" is
// "Arial Black". Bold and italic are run properties, not family names.
func fontName(s string) string {
	s = subset.ReplaceAllString(s, "")
	family, style, _ := strings.Cut(strings.ReplaceAll(s, ",", "-"), "-")
	style, _, _ = strings.Cut(style, "-")
	for _, suffix := range []string{"PSMT", "MT", "PS"} {
		if trimmed := strings.TrimSuffix(family, suffix); trimmed != "" && trimmed != family {
			family = trimmed
			break
		}
	}
	switch family {
	case "Helvetica":
		family = "Arial"
	case "Times":
		family = "Times New Roman"
	case "Courier":
		family = "Courier New"
	}
	if !strings.Contains(family, " ") {
		family = camel.ReplaceAllString(family, "$1 $2")
	}
	for _, suffix := range []string{"PSMT", "MT", "PS"} {
		style = strings.TrimSuffix(style, suffix)
	}
	// Weights and widths other than bold are part of Word's family name.
	// Anything else after the hyphen (CMap names like UniGB-UCS2-H) is not.
	switch style {
	case "Black", "Light", "Medium", "Semibold", "SemiBold", "Condensed", "Narrow", "Thin", "Heavy", "ExtraBold", "Demibold", "Demi", "ExtraLight", "UltraLight":
		family += " " + camel.ReplaceAllString(style, "$1 $2")
	}
	if r := []rune(family); len(r) > 31 { // ST_String limit for w:rFonts
		family = strings.TrimSpace(string(r[:31]))
	}
	return family
}

// fontTable tells Word each family's kind and a substitute to use when the
// family is not installed, so a missing font keeps its metrics class.
func (w *writer) fontTable() string {
	var b strings.Builder
	b.WriteString(declaration + `<w:fonts xmlns:w="` + word + `">`)
	names := make([]string, 0, len(w.fonts))
	for name := range w.fonts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		s := w.fonts[name]
		alt, family, pitch := "Arial", "swiss", "variable"
		switch {
		case s.Mono:
			alt, family, pitch = "Courier New", "modern", "fixed"
		case s.Serif:
			alt, family = "Times New Roman", "roman"
		}
		fmt.Fprintf(&b, `<w:font w:name="%s">`, esc(name))
		if alt != name {
			fmt.Fprintf(&b, `<w:altName w:val="%s"/>`, esc(alt))
		}
		fmt.Fprintf(&b, `<w:family w:val="%s"/><w:pitch w:val="%s"/></w:font>`, family, pitch)
	}
	b.WriteString(`</w:fonts>`)
	return b.String()
}

// Fonts PDFs often carry that Word may not have, and the Office font with
// near-identical metrics Word lays each out in.
var fontAliases = map[string]string{
	"dejavusansmono": "Courier New", "liberationsans": "Arial", "liberationserif": "Times New Roman",
	"liberationmono": "Courier New", "arimo": "Arial", "tinos": "Times New Roman", "cousine": "Courier New",
	"carlito": "Calibri", "caladea": "Cambria", "nimbussans": "Arial", "nimbussansl": "Arial",
	"nimbusroman": "Times New Roman", "nimbusromanno9l": "Times New Roman", "nimbusmono": "Courier New",
	"freesans": "Arial", "freeserif": "Times New Roman", "freemono": "Courier New", "helveticaneue": "Arial",
	"notosans": "Arial", "notoserif": "Times New Roman",
}

func fontKey(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, s)
}

type writer struct {
	body, rels strings.Builder
	files      map[string][]byte
	next       int
	fonts      map[string]Span // family -> a span drawn in it, for its flags
	justified  bool            // writing a line justification stretches
}

func (w *writer) relationship(kind, target string, external bool) string {
	w.next++
	id := fmt.Sprintf("rId%d", w.next)
	mode := ""
	if external {
		mode = ` TargetMode="External"`
	}
	fmt.Fprintf(&w.rels, `<Relationship Id="%s" Type="%s/%s" Target="%s"%s/>`, id, relationships, kind, esc(target), mode)
	return id
}
func (w *writer) span(b *strings.Builder, s Span) {
	if s.Link != "" {
		if strings.HasPrefix(s.Link, "#") {
			fmt.Fprintf(b, `<w:hyperlink w:anchor="%s">`, esc(strings.TrimPrefix(s.Link, "#")))
		} else {
			id := w.relationship("hyperlink", s.Link, true)
			fmt.Fprintf(b, `<w:hyperlink r:id="%s">`, id)
		}
	}
	family := wordFamily(s)
	scale, tracking := s.Scale, s.Tracking
	if fit, t, ok := wordFit(s, family, w.justified); ok {
		scale, tracking = fit, t // the source width in Word's metrics, Tz included
	}
	if _, ok := w.fonts[family]; !ok {
		w.fonts[family] = s
	}
	f := esc(family)
	if s.PageNumber {
		b.WriteString(`<w:fldSimple w:instr=" PAGE ">`)
		defer b.WriteString(`</w:fldSimple>`)
	}
	fmt.Fprintf(b, `<w:r><w:rPr><w:rFonts w:ascii="%s" w:hAnsi="%s" w:eastAsia="%s" w:cs="%s"/>`, f, f, f, f)
	if s.Weight >= 600 {
		b.WriteString(`<w:b/><w:bCs/>`)
	}
	if s.Italic {
		b.WriteString(`<w:i/><w:iCs/>`)
	}
	color := s.Color
	if color == "" {
		color = "000000"
	}
	fmt.Fprintf(b, `<w:color w:val="%s"/>`, esc(color))
	t := twips(tracking)
	if over := (float64(t)/20 - tracking) * float64(utf8.RuneCountInString(s.Text)-1); w.justified && over > 0.25 {
		// Rounded to zero, -0.015pt a character overfilled a full line by
		// a point and Word wrapped its last word; justification restretches
		// a line set narrower (but a line of word runs each set narrower
		// pulled the next line's first word up).
		t = int(math.Floor(tracking * 20))
	}
	if t != 0 {
		fmt.Fprintf(b, `<w:spacing w:val="%d"/>`, t)
	}
	if scale != 0 && scale != 100 {
		fmt.Fprintf(b, `<w:w w:val="%d"/>`, min(600, max(1, scale)))
	}
	// Schema order: color, spacing, w, position, sz, szCs, u.
	if rise := int(math.Round(s.Rise * 2)); rise != 0 {
		fmt.Fprintf(b, `<w:position w:val="%d"/>`, rise)
	}
	fmt.Fprintf(b, `<w:sz w:val="%d"/><w:szCs w:val="%d"/>`, halfPoints(s.Size), halfPoints(s.Size))
	if s.Link != "" || s.Underline {
		b.WriteString(`<w:u w:val="single"/>`)
	}
	// A soft hyphen is Word's optional hyphen: shown only where a line breaks.
	text := strings.ReplaceAll(esc(s.Text), "\u00ad", `</w:t><w:softHyphen/><w:t xml:space="preserve">`)
	fmt.Fprintf(b, `</w:rPr><w:t xml:space="preserve">%s</w:t></w:r>`, text)
	if s.Link != "" {
		b.WriteString(`</w:hyperlink>`)
	}
}

var numberMarker = regexp.MustCompile(`^\(?(?:\d{1,3}|[a-zA-Z]|[ivxlcIVXLC]{1,5})[.)]$|^\d{1,3}(?:\.\d{1,3})+\.?$`)

// isMarker reports whether a span is a list marker: one symbol that is not a
// letter or digit (•, ☐, ➢, a private-use Wingdings bullet) or a number like
// "3.", "(b)" or "1.2.1".
func isMarker(text string) bool {
	text = strings.TrimSpace(text)
	if r := []rune(text); len(r) == 1 {
		return !unicode.IsLetter(r[0]) && !unicode.IsDigit(r[0]) || r[0] == 'o'
	}
	return numberMarker.MatchString(text)
}

// gap is the source gap before span i of a line.
func gap(prev, s Span) float64 { return s.X - (prev.X + prev.Width) }

// tabbed reports whether span i follows a tab: a wide gap (a label and its
// amount), or any visible gap after a list marker (a bullet and its text).
func tabbed(l Line, i int) bool {
	prev, s := l.Spans[i-1], l.Spans[i]
	g := gap(prev, s)
	// After a line's first word a gap wider than a space and than the line's
	// other word gaps is a tab: "3⇥Heading", "1⇥Entry ....... 4". Justification
	// stretches every gap alike, so one as wide as the rest is a space.
	return s.Tab || s.Leader || g > math.Max(18, s.Size*2) || i == 1 && (g > 0.6*s.Size && isMarker(prev.Text) || g > 0.9*s.Size && widest(l, g))
}

// widest reports whether gap g after a line's first span is clearly wider
// than every later gap between its spans but a leader's (a tab of its own).
func widest(l Line, g float64) bool {
	for j := 2; j < len(l.Spans); j++ {
		if !l.Spans[j].Leader && gap(l.Spans[j-1], l.Spans[j]) > g/1.5 {
			return false
		}
	}
	return true
}

// runs writes one source line: spans, the spaces the PDF drew as gaps, and
// tabs for wide gaps (a label and its amount on one baseline).
func (w *writer) runs(b *strings.Builder, l Line) {
	for i, s := range l.Spans {
		if i > 0 {
			previous := l.Spans[i-1]
			if tabbed(l, i) {
				b.WriteString(`<w:r><w:tab/></w:r>`)
			} else if l.needsSpace(i) {
				// A space as wide as the source's gap: typesetters shrink and
				// stretch word gaps, and Word's own space would rewrap the line.
				space := previous
				space.Text, space.Width = " ", 0
				space.Tracking = gap(previous, s) - spaceWidth(previous)
				if space.Link != s.Link {
					space.Link = ""
				}
				w.span(b, space)
			}
		}
		w.span(b, s)
	}
}

// Tab stops are measured from the text column's left edge (origin).
func tabStops(lines []Line, origin float64) string {
	stops := map[int]string{} // position -> the stop's markup
	for _, l := range lines {
		for i, s := range l.Spans {
			switch {
			case i == 0 || !tabbed(l, i):
			case s.Leader:
				pos := twips(s.X + s.Width - origin)
				stops[pos] = fmt.Sprintf(`<w:tab w:val="right" w:leader="dot" w:pos="%d"/>`, pos)
			case stops[twips(s.X-origin)] == "":
				stops[twips(s.X-origin)] = fmt.Sprintf(`<w:tab w:val="left" w:pos="%d"/>`, twips(s.X-origin))
			}
		}
	}
	if len(stops) == 0 {
		return ""
	}
	var tabs strings.Builder
	tabs.WriteString(`<w:tabs>`)
	for _, pos := range slices.Sorted(maps.Keys(stops)) {
		tabs.WriteString(stops[pos])
	}
	return tabs.String() + `</w:tabs>`
}

// frame writes one line at its source position: the fallback for text that
// overlaps a ruled table or another column.
func (w *writer) frame(l Line, pageWidth float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<w:p><w:pPr><w:framePr w:w="%d" w:h="%d" w:wrap="none" w:vAnchor="page" w:hAnchor="page" w:x="%d" w:y="%d" w:hRule="atLeast"/>`, max(1, twips(pageWidth-l.X)), max(1, twips(l.Height+1)), twips(l.X), twips(l.Baseline-baselineDepth(l.Height)))
	b.WriteString(tabStops([]Line{l}, l.X))
	fmt.Fprintf(&b, `<w:spacing w:before="0" w:after="0" w:line="%d" w:lineRule="exact"/><w:ind w:left="0" w:right="0"/><w:jc w:val="left"/></w:pPr>`, max(1, twips(l.Height)))
	w.runs(&b, l)
	b.WriteString(`</w:p>`)
	return b.String()
}

// Explicit defaults: without them Word applies widow/orphan control and
// paragraph spacing of its own and moves source lines to other pages.
const styles = declaration + `<w:styles xmlns:w="` + word + `"><w:docDefaults><w:rPrDefault><w:rPr><w:rFonts w:ascii="Arial" w:hAnsi="Arial" w:eastAsia="Arial" w:cs="Arial"/><w:sz w:val="20"/><w:szCs w:val="20"/></w:rPr></w:rPrDefault><w:pPrDefault><w:pPr><w:widowControl w:val="0"/><w:spacing w:before="0" w:after="0" w:line="240" w:lineRule="auto"/></w:pPr></w:pPrDefault></w:docDefaults><w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style><w:style w:type="table" w:default="1" w:styleId="TableNormal"><w:name w:val="Normal Table"/><w:tblPr><w:tblCellMar><w:top w:w="0" w:type="dxa"/><w:left w:w="0" w:type="dxa"/><w:bottom w:w="0" w:type="dxa"/><w:right w:w="0" w:type="dxa"/></w:tblCellMar></w:tblPr></w:style></w:styles>`

// Word puts an exact-spaced line's baseline this share of the line's height
// below its top, in every font and at every height (measured in Word for Mac
// across Arial, Times New Roman, Courier New and Calibri, 15pt to 42pt lines
// of 14pt text).
const baselineShare = 0.8

func baselineDepth(height float64) float64 { return baselineShare * height }

// flow writes a paragraph into the column [origin, wrap] starting below
// cursor, and returns the new cursor. Positions are page points; Word sees
// only twips, so the cursor follows the rounded values it will lay out.
func (w *writer) flow(b *strings.Builder, p Paragraph, origin, wrap, cursor float64) float64 {
	pitch := float64(max(1, twips(p.Pitch))) / 20
	// One line's pitch is a guess (1.2 times its size); where the line above
	// ends closer than that (tight display leading), the guess would push it
	// and everything after it down the page, so the line takes the room it has.
	if room := (p.Lines[0].Baseline - cursor) / baselineShare; len(p.Lines) == 1 && room < pitch {
		pitch = float64(max(1, twips(math.Max(room, 0.75*p.Lines[0].size())))) / 20
	}
	top := p.Lines[0].Baseline - baselineDepth(pitch)
	before := float64(max(0, twips(top-cursor))) / 20
	left := max(0, twips(p.Left-origin))
	b.WriteString(`<w:p><w:pPr>`)
	b.WriteString(tabStops(p.Lines, origin))
	fmt.Fprintf(b, `<w:spacing w:before="%d" w:after="0" w:line="%d" w:lineRule="exact"/>`, twips(before), twips(pitch))
	indent := ""
	if first := twips(p.Indent); first > 0 {
		indent = fmt.Sprintf(` w:firstLine="%d"`, first)
	} else if first < 0 {
		hang := min(-first, left)
		indent = fmt.Sprintf(` w:hanging="%d"`, hang)
	}
	fmt.Fprintf(b, `<w:ind w:left="%d" w:right="%d"%s/><w:jc w:val="%s"/></w:pPr>`, left, max(0, twips(wrap-p.Right)), indent, p.Align)
	for i, l := range p.Lines {
		// Justification stretches every line but the last.
		w.justified = p.Align == "both" && i < len(p.Lines)-1
		if i > 0 {
			prev := p.Lines[i-1].Spans
			last := prev[len(prev)-1]
			if !strings.HasSuffix(last.Text, " ") && !strings.HasSuffix(last.Text, "-") && !strings.HasSuffix(last.Text, "\u00ad") {
				space := last
				space.Text, space.Width, space.Tracking = " ", 0, 0
				if space.Link != l.Spans[0].Link {
					space.Link = ""
				}
				w.span(b, space)
			}
		}
		w.runs(b, l)
	}
	w.justified = false
	b.WriteString(`</w:p>`)
	return cursor + before + pitch*float64(len(p.Lines))
}

// spacer moves the flow down to y before a table, which takes no spacing.
func spacer(b *strings.Builder, cursor, y float64) float64 {
	if g := twips(y - cursor); g > 0 {
		fmt.Fprintf(b, `<w:p><w:pPr><w:spacing w:before="0" w:after="0" w:line="%d" w:lineRule="exact"/><w:rPr><w:sz w:val="1"/></w:rPr></w:pPr></w:p>`, g)
		return cursor + float64(g)/20
	}
	return cursor
}

const tablePrefix = `<w:tblLayout w:type="fixed"/><w:tblCellMar><w:top w:w="0" w:type="dxa"/><w:left w:w="0" w:type="dxa"/><w:bottom w:w="0" w:type="dxa"/><w:right w:w="0" w:type="dxa"/></w:tblCellMar>`

func (w *writer) table(t Table) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<w:tbl><w:tblPr><w:tblW w:w="%d" w:type="dxa"/><w:jc w:val="left"/><w:tblInd w:w="%d" w:type="dxa"/><w:tblBorders>`, twips(t.XS[len(t.XS)-1]-t.X), twips(t.X))
	for _, edge := range []string{"top", "left", "bottom", "right", "insideH", "insideV"} {
		fmt.Fprintf(&b, `<w:%s w:val="single" w:sz="4" w:color="000000"/>`, edge)
	}
	b.WriteString(`</w:tblBorders>` + tablePrefix + `</w:tblPr><w:tblGrid>`)
	for i := 1; i < len(t.XS); i++ {
		fmt.Fprintf(&b, `<w:gridCol w:w="%d"/>`, twips(t.XS[i]-t.XS[i-1]))
	}
	b.WriteString(`</w:tblGrid>`)
	for r, row := range t.Rows {
		fmt.Fprintf(&b, `<w:tr><w:trPr><w:cantSplit/><w:trHeight w:val="%d" w:hRule="exact"/></w:trPr>`, max(1, twips(t.YS[r+1]-t.YS[r])))
		for _, cell := range row {
			x := t.XS[cell.Start]
			right := t.XS[cell.Start+cell.Columns]
			fmt.Fprintf(&b, `<w:tc><w:tcPr><w:tcW w:w="%d" w:type="dxa"/>`, twips(right-x))
			if cell.Columns > 1 {
				fmt.Fprintf(&b, `<w:gridSpan w:val="%d"/>`, cell.Columns)
			}
			if cell.Continue {
				b.WriteString(`<w:vMerge/>`)
			} else if cell.Merge {
				b.WriteString(`<w:vMerge w:val="restart"/>`)
			}
			b.WriteString(`</w:tcPr>`)
			ll := baselineLines(cell.Spans)
			if len(ll) == 0 {
				b.WriteString(`<w:p><w:pPr><w:spacing w:before="0" w:after="0" w:line="1" w:lineRule="exact"/></w:pPr></w:p>`)
			}
			cursor := t.YS[r]
			if len(ll) > 0 {
				for _, p := range paragraphs(ll, x, right) {
					cursor = w.flow(&b, p, x, right, cursor)
				}
			}
			b.WriteString(`</w:tc>`)
		}
		b.WriteString(`</w:tr>`)
	}
	b.WriteString(`</w:tbl>`)
	return b.String()
}

// columns writes side-by-side blocks as one borderless row, so each column
// rewraps on its own when edited. Returns the row's bottom.
func (w *writer) columns(b *strings.Builder, cols []Block, top float64) float64 {
	edges := make([]float64, len(cols)+1)
	for i, c := range cols {
		edges[i] = c.X
	}
	last := cols[len(cols)-1]
	edges[len(cols)] = last.Right + 12
	for _, p := range last.Paragraphs {
		edges[len(cols)] = math.Max(edges[len(cols)], p.Right)
	}
	fmt.Fprintf(b, `<w:tbl><w:tblPr><w:tblW w:w="%d" w:type="dxa"/><w:jc w:val="left"/><w:tblInd w:w="%d" w:type="dxa"/><w:tblBorders><w:top w:val="nil"/><w:left w:val="nil"/><w:bottom w:val="nil"/><w:right w:val="nil"/><w:insideH w:val="nil"/><w:insideV w:val="nil"/></w:tblBorders>%s</w:tblPr><w:tblGrid>`, twips(edges[len(cols)]-edges[0]), twips(edges[0]), tablePrefix)
	for i := range cols {
		fmt.Fprintf(b, `<w:gridCol w:w="%d"/>`, twips(edges[i+1]-edges[i]))
	}
	b.WriteString(`</w:tblGrid>`)
	var cells strings.Builder
	bottom := top
	for i, c := range cols {
		fmt.Fprintf(&cells, `<w:tc><w:tcPr><w:tcW w:w="%d" w:type="dxa"/></w:tcPr>`, twips(edges[i+1]-edges[i]))
		cursor := top
		for _, p := range c.Paragraphs {
			cursor = w.flow(&cells, p, edges[i], edges[i+1], cursor)
		}
		bottom = math.Max(bottom, cursor)
		cells.WriteString(`</w:tc>`)
	}
	fmt.Fprintf(b, `<w:tr><w:trPr><w:cantSplit/><w:trHeight w:val="%d" w:hRule="atLeast"/></w:trPr>%s</w:tr></w:tbl>`, max(1, twips(bottom-top)), cells.String())
	return bottom
}

// section closes a page: its header and footer references, its size, and
// where page numbering starts (start >= 0).
func section(p Page, refs string, start int) string {
	numbering := ""
	if start >= 0 {
		numbering = fmt.Sprintf(`<w:pgNumType w:start="%d"/>`, start)
	}
	// A negative top or bottom margin places the body exactly there, however
	// tall the header or footer content is.
	edge := "0"
	if refs != "" {
		edge = "-1"
	}
	return fmt.Sprintf(`<w:sectPr>%s<w:type w:val="nextPage"/><w:pgSz w:w="%d" w:h="%d"/><w:pgMar w:top="%s" w:right="0" w:bottom="%s" w:left="0" w:header="0" w:footer="0" w:gutter="0"/>%s</w:sectPr>`, refs, twips(p.Width), twips(p.Height), edge, edge, numbering)
}

// Write is the deterministic low-level serializer, used by conversion and the
// geometry tests. Call Convert for the mandatory validation/publication gate.
func Write(dst io.Writer, pages []Page) (report Report, err error) {
	return write(context.Background(), dst, pages)
}

func write(ctx context.Context, dst io.Writer, pages []Page) (report Report, err error) {
	if len(pages) < 1 || len(pages) > 2000 {
		return report, fmt.Errorf("document page limit")
	}
	w := writer{files: map[string][]byte{}, fonts: map[string]Span{}}
	w.body.WriteString(declaration + `<w:document xmlns:w="` + word + `" xmlns:r="` + relationships + `" xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:pic="http://schemas.openxmlformats.org/drawingml/2006/picture"><w:body>`)
	w.rels.WriteString(declaration + `<Relationships xmlns="` + packageRelationships + `">`)
	margins, body, firstNumber := w.margins(pages)
	for i, p := range body {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		l, e := layout(p)
		if e != nil {
			return report, fmt.Errorf("page %d: %w", i+1, e)
		}
		fmt.Fprintf(&w.body, `<w:p><w:pPr><w:spacing w:before="0" w:after="0" w:line="1" w:lineRule="exact"/></w:pPr><w:bookmarkStart w:id="%d" w:name="page%d"/><w:bookmarkEnd w:id="%d"/></w:p>`, i, i+1, i)
		cursor := 0.0
		// A page's images anchor to one hairline paragraph at its top: they
		// sit at page positions, and a paragraph each at the page's end took
		// a line each and spilled full pages onto a new one.
		if len(l.Images) > 0 {
			w.body.WriteString(`<w:p><w:pPr><w:spacing w:before="0" w:after="0" w:line="1" w:lineRule="exact"/><w:rPr><w:sz w:val="1"/></w:rPr></w:pPr>`)
			for j, im := range l.Images {
				name := fmt.Sprintf("media/page%d-image%d.png", i+1, j+1)
				w.files["word/"+name] = im.PNG
				id := w.relationship("image", name, false)
				w.next++
				shape := w.next
				cx, cy := int64(math.Round(im.Width*12700)), int64(math.Round(im.Height*12700))
				fmt.Fprintf(&w.body, `<w:r><w:drawing><wp:anchor distT="0" distB="0" distL="0" distR="0" simplePos="0" relativeHeight="0" behindDoc="1" locked="0" layoutInCell="1" allowOverlap="1"><wp:simplePos x="0" y="0"/><wp:positionH relativeFrom="page"><wp:posOffset>%d</wp:posOffset></wp:positionH><wp:positionV relativeFrom="page"><wp:posOffset>%d</wp:posOffset></wp:positionV><wp:extent cx="%d" cy="%d"/><wp:wrapNone/><wp:docPr id="%d" name="Image %d"/><wp:cNvGraphicFramePr/><a:graphic><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/picture"><pic:pic><pic:nvPicPr><pic:cNvPr id="%d" name="Image %d"/><pic:cNvPicPr/></pic:nvPicPr><pic:blipFill><a:blip r:embed="%s"/><a:stretch><a:fillRect/></a:stretch></pic:blipFill><pic:spPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="%d" cy="%d"/></a:xfrm><a:prstGeom prst="rect"><a:avLst/></a:prstGeom></pic:spPr></pic:pic></a:graphicData></a:graphic></wp:anchor></w:drawing></w:r>`, int64(math.Round(im.X*12700)), int64(math.Round(im.Y*12700)), cx, cy, shape, shape, shape, shape, id, cx, cy)
			}
			w.body.WriteString(`</w:p>`)
		}
		for _, it := range l.Items {
			switch {
			case it.Block != nil:
				for _, para := range it.Block.Paragraphs {
					cursor = w.flow(&w.body, para, 0, p.Width, cursor)
				}
			case it.Table != nil:
				cursor = spacer(&w.body, cursor, it.Table.Y)
				w.body.WriteString(w.table(*it.Table))
				cursor = it.Table.YS[len(it.Table.YS)-1]
			case len(it.Columns) > 0:
				cursor = spacer(&w.body, cursor, it.top())
				cursor = w.columns(&w.body, it.Columns, cursor)
			default:
				for _, line := range it.Frames {
					w.body.WriteString(w.frame(line, p.Width))
				}
			}
		}
		// Only the first section sets where page numbering starts.
		start := -1
		if i == 0 {
			start = firstNumber
		}
		if i < len(pages)-1 {
			w.body.WriteString(`<w:p><w:pPr><w:rPr><w:sz w:val="1"/></w:rPr>` + section(p, margins[i], start) + `</w:pPr></w:p>`)
		} else {
			w.body.WriteString(section(p, margins[i], start))
		}
		report.Pages++
		report.Tables += len(l.Tables)
		report.Images += len(l.Images)
		for _, s := range pages[i].Spans { // header and footer text counts too
			report.Characters += utf8.RuneCountInString(s.Text)
		}
		generated := w.body.Len()
		for _, data := range w.files {
			generated += len(data)
		}
		if generated > 128<<20 {
			return report, fmt.Errorf("Word content exceeds 128 MiB output limit")
		}
	}
	w.body.WriteString(`</w:body></w:document>`)
	w.relationship("styles", "styles.xml", false)
	w.relationship("fontTable", "fontTable.xml", false)
	w.rels.WriteString(`</Relationships>`)
	w.files["word/fontTable.xml"] = []byte(w.fontTable())
	w.files["word/styles.xml"] = []byte(styles)
	w.files["word/document.xml"] = []byte(w.body.String())
	w.files["word/_rels/document.xml.rels"] = []byte(w.rels.String())
	w.files["_rels/.rels"] = []byte(declaration + `<Relationships xmlns="` + packageRelationships + `"><Relationship Id="rId1" Type="` + relationships + `/officeDocument" Target="word/document.xml"/></Relationships>`)
	var names []string
	for name := range w.files {
		names = append(names, name)
	}
	w.files["[Content_Types].xml"] = []byte(declaration + `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Default Extension="png" ContentType="image/png"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/><Override PartName="/word/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.styles+xml"/><Override PartName="/word/fontTable.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.fontTable+xml"/>` + marginTypes(names) + `</Types>`)
	names = append(names, "[Content_Types].xml")
	sort.Strings(names)
	archive := zip.NewWriter(dst)
	for _, name := range names {
		data := w.files[name]
		header := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)}
		var f io.Writer
		var e error
		if strings.HasSuffix(name, ".png") {
			// Already deflated: stored as is, with sizes up front so streaming
			// readers (Java's ZipInputStream) can read it.
			// CreateRaw writes the header as given: Word refuses an entry
			// whose version needed to extract is 0, so state ZIP 2.0.
			header.CreatorVersion, header.ReaderVersion = 20, 20
			header.Method, header.CRC32 = zip.Store, crc32.ChecksumIEEE(data)
			header.CompressedSize64, header.UncompressedSize64 = uint64(len(data)), uint64(len(data))
			f, e = archive.CreateRaw(header)
		} else {
			f, e = archive.CreateHeader(header)
		}
		if e != nil {
			return report, e
		}
		if _, e = f.Write(data); e != nil {
			return report, e
		}
	}
	err = archive.Close()
	return report, err
}

func marginTypes(names []string) string {
	var b strings.Builder
	for _, name := range slices.Sorted(slices.Values(names)) {
		for _, kind := range []string{"header", "footer"} {
			if strings.HasPrefix(name, "word/"+kind) {
				fmt.Fprintf(&b, `<Override PartName="/%s" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.%s+xml"/>`, name, kind)
			}
		}
	}
	return b.String()
}
