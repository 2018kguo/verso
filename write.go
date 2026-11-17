package verso

import (
	"archive/zip"
	"bytes"
	"cmp"
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
	"sync"
	"sync/atomic"
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

// memo caches a pure string function: font names are few, and the writer
// asks for each span's family many times (a regexp and allocations a call
// were a fifth of its time). Bounded: past 4096 names it stops storing.
type memo struct {
	m sync.Map
	n atomic.Int32
}

var fontNames, fontKeys memo

func (c *memo) get(k string, f func(string) string) string {
	if v, ok := c.m.Load(k); ok {
		return v.(string)
	}
	v := f(k)
	if c.n.Add(1) <= 4096 {
		c.m.Store(k, v)
	}
	return v
}

// withoutSubset drops a subset tag ("ABCDEF+"), by hand: a regexp here,
// called for every span's every measure, was an eighth of the writer's time.
func withoutSubset(s string) string {
	if len(s) < 7 || s[6] != '+' {
		return s
	}
	for i := range 6 {
		if s[i] < 'A' || s[i] > 'Z' {
			return s
		}
	}
	return s[7:]
}

var camel = regexp.MustCompile(`([a-z])([A-Z])`)

// fontName turns a PostScript name into the family Word knows:
// "ABCDEF+TimesNewRomanPS-BoldMT" is "Times New Roman"; "Arial-Black" is
// "Arial Black". Bold and italic are run properties, not family names.
func fontName(s string) string { return fontNames.get(s, nameOf) }
func nameOf(s string) string {
	s = withoutSubset(s)
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
	"nimbusromno9l": "Times New Roman", "nimbusmonl": "Courier New", "texgyretermes": "Times New Roman",
	"texgyreheros": "Arial", "texgyrecursor": "Courier New",
	// Bitstream Vera's design and widths (DejaVu extends it) are Verdana's
	// within 1% (median per letter 1.000); Arial is 13% narrower.
	"dejavusans": "Verdana", "bitstreamverasans": "Verdana",
}

func fontKey(s string) string { return fontKeys.get(s, keyOf) }
func keyOf(s string) string {
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
	} else if n := utf8.RuneCountInString(s.Text); n > 0 && family == fontName(s.Font) && !w.justified {
		// A font Word has but no table measures: its source width is Word's
		// at the source size, so track back what half points take off
		// (9.12pt drawn at 9pt ran lines 1.3% short).
		tracking += s.Width * (1 - float64(halfPoints(s.Size))/2/s.Size) / float64(n)
	}
	if _, ok := w.fonts[family]; !ok {
		w.fonts[family] = s
	}
	f := esc(family)
	if s.PageNumber {
		b.WriteString(`<w:fldSimple w:instr=" PAGE ">`)
		defer b.WriteString(`</w:fldSimple>`)
	}
	east, lang := eastAsian(s)
	if east == "" {
		east = family
	}
	var props strings.Builder
	if s.Weight >= 600 {
		props.WriteString(`<w:b/><w:bCs/>`)
	}
	if s.Italic {
		props.WriteString(`<w:i/><w:iCs/>`)
	}
	color := s.Color
	if color == "" {
		color = "000000"
	}
	fmt.Fprintf(&props, `<w:color w:val="%s"/>`, esc(color))
	t := twips(tracking)
	if over := (float64(t)/20 - tracking) * float64(utf8.RuneCountInString(s.Text)-1); w.justified && over > 0.25 {
		// Rounded to zero, -0.015pt a character overfilled a full line by
		// a point and Word wrapped its last word; justification restretches
		// a line set narrower (but a line of word runs each set narrower
		// pulled the next line's first word up).
		t = int(math.Floor(tracking * 20))
	}
	if t != 0 {
		fmt.Fprintf(&props, `<w:spacing w:val="%d"/>`, t)
	}
	if scale != 0 && scale != 100 {
		fmt.Fprintf(&props, `<w:w w:val="%d"/>`, min(600, max(1, scale)))
	}
	// Schema order: color, spacing, w, position, sz, szCs, u.
	if rise := int(math.Round(s.Rise * 2)); rise != 0 {
		fmt.Fprintf(&props, `<w:position w:val="%d"/>`, rise)
	}
	fmt.Fprintf(&props, `<w:sz w:val="%d"/><w:szCs w:val="%d"/>`, halfPoints(s.Size), halfPoints(s.Size))
	// A link keeps the look the PDF drew: a link drawn plain (a whole
	// paragraph made one link area) underlined was a rule under every line.
	if s.Underline {
		props.WriteString(`<w:u w:val="single"/>`)
	}
	if lang != "" {
		fmt.Fprintf(&props, `<w:lang w:eastAsia="%s"/>`, lang)
	}
	// A soft hyphen is Word's optional hyphen: shown only where a line breaks.
	// Word sets each complex script in the run's cs face: a run per script
	// (Malayalam in Kartika, Devanagari in Mangal), the rest as one.
	for _, piece := range scriptPieces(s.Text) {
		// A soft hyphen is Word's optional hyphen: shown only where a line breaks.
		text := strings.ReplaceAll(esc(piece.text), "\u00ad", `</w:t><w:softHyphen/><w:t xml:space="preserve">`)
		fmt.Fprintf(b, `<w:r><w:rPr><w:rFonts w:ascii="%s" w:hAnsi="%s" w:eastAsia="%s" w:cs="%s"/>%s</w:rPr><w:t xml:space="preserve">%s</w:t></w:r>`, f, f, esc(east), cmp.Or(piece.cs, f), props.String(), text)
	}
	if s.Link != "" {
		b.WriteString(`</w:hyperlink>`)
	}
}

// lines draws a page's rules as one shape per stroke (width and color), a path of
// line pieces: a shape each took 25 elements a rule, and a label's 13,000
// rules made most of its document (and its validation time).
func (w *writer) lines(lines []Segment) {
	type stroke struct {
		width float64
		color string
	}
	groups := map[stroke][]Segment{}
	var strokes []stroke
	for _, s := range lines {
		// A stroke Recto did not measure is a hairline form rule.
		k := stroke{math.Round(cmp.Or(s.Width, 0.75)*4) / 4, cmp.Or(s.Color, "000000")}
		if _, ok := groups[k]; !ok {
			strokes = append(strokes, k)
		}
		groups[k] = append(groups[k], s)
	}
	emu := func(v float64) int64 { return int64(math.Round(v * 12700)) }
	for _, k := range strokes {
		group, width := groups[k], k.width
		x0, y0, x1, y1 := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
		for _, s := range group {
			x0, y0 = min(x0, s.X1, s.X2), min(y0, s.Y1, s.Y2)
			x1, y1 = max(x1, s.X1, s.X2), max(y1, s.Y1, s.Y2)
		}
		cx, cy := emu(x1)-emu(x0), emu(y1)-emu(y0)
		var path strings.Builder
		for _, s := range group {
			fmt.Fprintf(&path, `<a:moveTo><a:pt x="%d" y="%d"/></a:moveTo><a:lnTo><a:pt x="%d" y="%d"/></a:lnTo>`, emu(s.X1)-emu(x0), emu(s.Y1)-emu(y0), emu(s.X2)-emu(x0), emu(s.Y2)-emu(y0))
		}
		w.next++
		fmt.Fprintf(&w.body, `<w:r><w:drawing><wp:anchor distT="0" distB="0" distL="0" distR="0" simplePos="0" relativeHeight="0" behindDoc="1" locked="0" layoutInCell="1" allowOverlap="1"><wp:simplePos x="0" y="0"/><wp:positionH relativeFrom="page"><wp:posOffset>%d</wp:posOffset></wp:positionH><wp:positionV relativeFrom="page"><wp:posOffset>%d</wp:posOffset></wp:positionV><wp:extent cx="%d" cy="%d"/><wp:wrapNone/><wp:docPr id="%d" name="Lines %[5]d"/><wp:cNvGraphicFramePr/><a:graphic><a:graphicData uri="http://schemas.microsoft.com/office/word/2010/wordprocessingShape"><wps:wsp><wps:cNvSpPr/><wps:spPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="%[3]d" cy="%[4]d"/></a:xfrm><a:custGeom><a:pathLst><a:path w="%[9]d" h="%[10]d" fill="none">%[6]s</a:path></a:pathLst></a:custGeom><a:noFill/><a:ln w="%[7]d"><a:solidFill><a:srgbClr val="%[8]s"/></a:solidFill></a:ln></wps:spPr><wps:bodyPr/></wps:wsp></a:graphicData></a:graphic></wp:anchor></w:drawing></w:r>`,
			// A path's frame divides its points: one row of rules (or a
			// column) is 0 deep, and Word drew nothing for 0/0.
			emu(x0), emu(y0), cx, cy, w.next, path.String(), emu(width), k.color, max(cx, 1), max(cy, 1))
	}
}

// complexScript is the face Word ships (on Windows and Mac) for a script it
// lays out as complex: Word sets such text in the run's cs font, and a Latin
// face there (Arial) left it to a fallback of Word's choosing, whose widths ran
// Malayalam Wikipedia from 71 pages to 79.
// scriptPieces splits text where its complex script changes; text with
// one script (or none) is one piece.
func scriptPieces(text string) []struct{ text, cs string } {
	var pieces []struct{ text, cs string }
	start, cs := 0, ""
	for i, r := range text {
		if f := complexFace(r); f != "" && f != cs {
			if cs != "" {
				pieces = append(pieces, struct{ text, cs string }{text[start:i], cs})
				start = i
			}
			cs = f
		}
	}
	return append(pieces, struct{ text, cs string }{text[start:], cs})
}

func complexScript(text string) string {
	for _, r := range text {
		if f := complexFace(r); f != "" {
			return f
		}
	}
	return ""
}

func complexFace(r rune) string {
	switch {
	case r >= 0x900 && r <= 0x97f:
		return "Mangal"
	case r >= 0xb80 && r <= 0xbff:
		return "Latha"
	case r >= 0xc00 && r <= 0xc7f:
		return "Gautami"
	case r >= 0xc80 && r <= 0xcff:
		return "Tunga"
	case r >= 0xd00 && r <= 0xd7f:
		return "Kartika"
	case r >= 0xf00 && r <= 0xfff:
		return "Microsoft Himalaya"
	case r >= 0x1000 && r <= 0x109f:
		return "Myanmar Text"
	}
	return ""
}

// eastAsian is the East Asian font and language for a span's CJK text, from
// its script (kana is Japanese, Hangul Korean) or its source font's region
// ("NotoSansCJKsc", "MingLiU"); "" for text without any. Left to Word, Han
// text took a Japanese fallback without simplified Chinese forms, and 论 or
// 这 drew blank. The fonts are ones Word installs on Windows and Mac.
func eastAsian(s Span) (font, lang string) {
	han, kana, hangul := false, false, false
	for _, r := range s.Text {
		switch {
		case unicode.Is(unicode.Hangul, r):
			hangul = true
		case unicode.In(r, unicode.Hiragana, unicode.Katakana):
			kana = true
		// CJK punctuation and full-width forms ("，", "《") are set in the
		// same face: left to Word's fallback, a comma took another font's
		// width and line breaks.
		case unicode.Is(unicode.Han, r) || r >= 0x3000 && r <= 0x303f || r >= 0xff00 && r <= 0xffef:
			han = true
		}
	}
	if !han && !kana && !hangul {
		return "", ""
	}
	key := fontKey(fontName(s.Font))
	serif := s.Serif || cjkSerif.MatchString(key)
	pick := func(serifFont, sansFont, lang string) (string, string) {
		if serif {
			return serifFont, lang
		}
		return sansFont, lang
	}
	switch {
	case hangul || cjkRegion(key, "kr", "korean", "malgun", "batang", "gulim", "dotum", "myeongjo", "gothicneo"):
		return pick("Batang", "Malgun Gothic", "ko-KR")
	case kana || cjkRegion(key, "jp", "japan", "mincho", "gothic", "meiryo", "hiragino", "yugoth", "yumin", "ipa"):
		return pick("MS Mincho", "MS Gothic", "ja-JP")
	case cjkRegion(key, "tc", "hk", "tw", "ming", "jhenghei", "big5"):
		return pick("MingLiU", "MingLiU", "zh-TW")
	}
	return pick("SimSun", "SimHei", "zh-CN")
}

// FangZheng's names abbreviate theirs: FZSSK ShuSong, FZBSK BaoSong,
// FZXBSK XiaoBiaoSong, FZFSK FangSong, FZKTK KaiTi.
var cjkSerif = regexp.MustCompile(`serif|mincho|ming|song|sun|batang|myeongjo|kai|fang|^fz(ss|bs|xbs|fs|kt)`)

// cjkRegion reports whether a font key names a region: "notosanscjkjp"
// ends in jp; "msmincho" contains mincho.
func cjkRegion(key string, marks ...string) bool {
	for _, m := range marks {
		if len(m) <= 2 && (strings.HasSuffix(key, m) || strings.Contains(key, "cjk"+m) || strings.Contains(key, m+"regular") || strings.Contains(key, m+"bold")) {
			return true
		}
		if len(m) > 2 && strings.Contains(key, m) {
			return true
		}
	}
	return false
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

// gap is the source gap between prev and the span s after it.
func gap(prev, s Span) float64 { return s.X - (prev.X + prev.Width) }

// tabbed reports whether span i follows a tab: a wide gap (a label and its
// amount), or a gap over 0.6 em after a list marker (a bullet and its text).
func tabbed(l Line, i int) bool {
	prev, s := l.Spans[i-1], l.Spans[i]
	g := gap(prev, s)
	// After a line's first word a gap wider than a space and than the line's
	// other word gaps is a tab: "3⇥Heading", "1⇥Entry ....... 4". Justification
	// stretches every gap alike, so one as wide as the rest is a space.
	// A marker's gap counts from its ink: "4.15 " drawn with its space left
	// only 6.5pt after it.
	ink := g
	if prev.Ink > 0 && prev.Ink < prev.Width {
		ink = s.X - (prev.X + prev.Ink)
	}
	return s.Tab || s.Leader || g > math.Max(18, s.Size*2) || i == 1 && (ink > 0.6*s.Size && isMarker(prev.Text) || g > 0.9*s.Size && widest(l, g))
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
				space.Tracking = 0 // a face no table measures keeps Word's own space
				if measurable(face(wordFamily(previous), previous), " ") {
					space.Tracking = gap(previous, s) - spaceWidth(previous)
				}
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
const styles = declaration + `<w:styles xmlns:w="` + word + `"><w:docDefaults><w:rPrDefault><w:rPr><w:rFonts w:ascii="Arial" w:hAnsi="Arial" w:eastAsia="Arial" w:cs="Arial"/><w:sz w:val="20"/><w:szCs w:val="20"/></w:rPr></w:rPrDefault><w:pPrDefault><w:pPr><w:widowControl w:val="0"/><w:autoSpaceDE w:val="0"/><w:autoSpaceDN w:val="0"/><w:spacing w:before="0" w:after="0" w:line="240" w:lineRule="auto"/></w:pPr></w:pPrDefault></w:docDefaults><w:style w:type="paragraph" w:default="1" w:styleId="Normal"><w:name w:val="Normal"/></w:style><w:style w:type="table" w:default="1" w:styleId="TableNormal"><w:name w:val="Normal Table"/><w:tblPr><w:tblCellMar><w:top w:w="0" w:type="dxa"/><w:left w:w="0" w:type="dxa"/><w:bottom w:w="0" w:type="dxa"/><w:right w:w="0" w:type="dxa"/></w:tblCellMar></w:tblPr></w:style></w:styles>`

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

// grid is the rows of side-by-side columns that share one baseline per
// line (a table without rules: "MONTH  ASSAY %  pH"), one line per column
// each; nil otherwise. Three columns at least: two of prose can share a
// leading.
func grid(cols []Block) [][]Line {
	if len(cols) < 3 {
		return nil
	}
	var rows [][]Line
	for i, c := range cols {
		var ls []Line
		for _, p := range c.Paragraphs {
			ls = append(ls, p.Lines...)
		}
		if i == 0 {
			if len(ls) < 2 {
				return nil
			}
			rows = make([][]Line, len(ls))
		}
		if len(ls) != len(rows) {
			return nil
		}
		for r, l := range ls {
			if i > 0 && math.Abs(l.Baseline-rows[r][0].Baseline) > 1 {
				return nil
			}
			rows[r] = append(rows[r], l)
		}
	}
	return rows
}

// gridRows writes grid rows as table rows of exact height, each cell one
// line: columns flowed apart drifted a few points from row to row.
func (w *writer) gridRows(b *strings.Builder, rows [][]Line, edges []float64, top float64) float64 {
	pitch := func(r int) float64 {
		if r+1 < len(rows) {
			return rows[r+1][0].Baseline - rows[r][0].Baseline
		}
		return rows[r][0].Baseline - rows[r-1][0].Baseline
	}
	for r, line := range rows {
		bottom := top
		if r+1 < len(rows) {
			bottom = rows[r+1][0].Baseline - baselineDepth(pitch(r+1))
		}
		var cells strings.Builder
		for i, l := range line {
			cell(&cells, edges[i+1]-edges[i])
			for _, p := range paragraphs([]Line{l}, edges[i], edges[i+1]) {
				p.Pitch = pitch(r)
				bottom = math.Max(bottom, w.flow(&cells, p, edges[i], edges[i+1], top))
			}
			cells.WriteString(`</w:tc>`)
		}
		row(b, bottom-top, "exact")
		b.WriteString(cells.String() + `</w:tr>`)
		top = bottom
	}
	return top
}

// openTable starts a table whose columns run between edges, indented from
// origin, through its grid.
func openTable(b *strings.Builder, edges []float64, origin float64, borders string) {
	fmt.Fprintf(b, `<w:tbl><w:tblPr><w:tblW w:w="%d" w:type="dxa"/><w:jc w:val="left"/><w:tblInd w:w="%d" w:type="dxa"/>%s%s</w:tblPr><w:tblGrid>`, twips(edges[len(edges)-1]-edges[0]), twips(edges[0]-origin), borders, tablePrefix)
	for i := 1; i < len(edges); i++ {
		fmt.Fprintf(b, `<w:gridCol w:w="%d"/>`, twips(edges[i]-edges[i-1]))
	}
	b.WriteString(`</w:tblGrid>`)
}

// row starts a row that does not split across pages; rule is how its
// height holds ("exact" or "atLeast").
func row(b *strings.Builder, height float64, rule string) {
	fmt.Fprintf(b, `<w:tr><w:trPr><w:cantSplit/><w:trHeight w:val="%d" w:hRule="%s"/></w:trPr>`, max(1, twips(height)), rule)
}

// cell starts a plain cell of a width.
func cell(b *strings.Builder, width float64) {
	fmt.Fprintf(b, `<w:tc><w:tcPr><w:tcW w:w="%d" w:type="dxa"/></w:tcPr>`, twips(width))
}

// hairline is an empty paragraph one twip tall: a cell must end in one.
const hairline = `<w:p><w:pPr><w:spacing w:before="0" w:after="0" w:line="1" w:lineRule="exact"/></w:pPr></w:p>`

// table writes a ruled table; origin is the left edge its indent counts
// from (the page, or the layout cell holding it).
func (w *writer) table(t Table, origin float64) string {
	var b, borders strings.Builder
	borders.WriteString(`<w:tblBorders>`)
	for _, edge := range []string{"top", "left", "bottom", "right", "insideH", "insideV"} {
		if edge == "left" && t.OpenLeft || edge == "right" && t.OpenRight {
			fmt.Fprintf(&borders, `<w:%s w:val="nil"/>`, edge)
			continue
		}
		fmt.Fprintf(&borders, `<w:%s w:val="single" w:sz="4" w:color="000000"/>`, edge)
	}
	borders.WriteString(`</w:tblBorders>`)
	openTable(&b, t.XS, origin, borders.String())
	for r, cells := range t.Rows {
		row(&b, t.YS[r+1]-t.YS[r], "exact")
		for _, cell := range cells {
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
				b.WriteString(hairline)
			}
			cursor := t.YS[r]
			if len(ll) > 0 {
				// Text that wraps in the cell (its widest line near the cell's
				// width) is measured to its own right edge, as a block's: from
				// the cell's edge every line had room for the next line's first
				// word and became a paragraph. Short lines (centered labels)
				// keep the cell's edge and stay a paragraph each.
				edge, lo := 0.0, right
				for _, l := range ll {
					edge, lo = math.Max(edge, l.right()), math.Min(lo, l.X)
				}
				if edge-lo < 0.75*(right-x) {
					edge = right
				}
				for _, p := range paragraphs(ll, x, math.Min(right, edge)) {
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

// tables writes ruled tables that sit side by side as the cells of one
// borderless row of their combined height, each nested at its source
// position. Returns the row's bottom.
func (w *writer) tables(b *strings.Builder, ts []*Table, top float64) float64 {
	bottom, right := top, ts[len(ts)-1].right()
	for _, t := range ts {
		bottom = math.Max(bottom, t.bottom())
	}
	edges := make([]float64, len(ts)+1)
	for i, t := range ts {
		edges[i] = t.X
	}
	edges[len(ts)] = right
	openTable(b, edges, 0, "")
	row(b, bottom-top, "exact")
	for i, t := range ts {
		cell(b, edges[i+1]-edges[i])
		spacer(b, top, t.Y)
		b.WriteString(w.table(*t, t.X) + hairline + `</w:tc>`)
	}
	b.WriteString(`</w:tr></w:tbl>`)
	return bottom
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
	openTable(b, edges, 0, `<w:tblBorders><w:top w:val="nil"/><w:left w:val="nil"/><w:bottom w:val="nil"/><w:right w:val="nil"/><w:insideH w:val="nil"/><w:insideV w:val="nil"/></w:tblBorders>`)
	if rows := grid(cols); rows != nil {
		bottom := w.gridRows(b, rows, edges, top)
		b.WriteString(`</w:tbl>`)
		return bottom
	}
	var cells strings.Builder
	bottom := top
	for i, c := range cols {
		cell(&cells, edges[i+1]-edges[i])
		cursor := top
		for _, p := range c.Paragraphs {
			cursor = w.flow(&cells, p, edges[i], edges[i+1], cursor)
		}
		bottom = math.Max(bottom, cursor)
		cells.WriteString(`</w:tc>`)
	}
	row(b, bottom-top, "atLeast")
	b.WriteString(cells.String() + `</w:tr></w:tbl>`)
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
	w.body.WriteString(declaration + `<w:document xmlns:w="` + word + `" xmlns:r="` + relationships + `" xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:pic="http://schemas.openxmlformats.org/drawingml/2006/picture" xmlns:wps="http://schemas.microsoft.com/office/word/2010/wordprocessingShape"><w:body>`)
	w.rels.WriteString(declaration + `<Relationships xmlns="` + packageRelationships + `">`)
	serifByWidth(pages)
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
		if len(l.Images) > 0 || len(l.Lines) > 0 {
			w.body.WriteString(`<w:p><w:pPr><w:spacing w:before="0" w:after="0" w:line="1" w:lineRule="exact"/><w:rPr><w:sz w:val="1"/></w:rPr></w:pPr>`)
			w.lines(l.Lines)
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
				w.body.WriteString(w.table(*it.Table, 0))
				cursor = it.Table.bottom()
			case len(it.Tables) > 0:
				cursor = spacer(&w.body, cursor, it.top())
				cursor = w.tables(&w.body, it.Tables, cursor)
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
