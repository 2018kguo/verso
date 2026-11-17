package verso

import (
	"math"
	"regexp"
	"strings"
)

//go:generate go run gen_metrics.go /Applications/LibreOffice.app/Contents/Resources/fonts/truetype "/Applications/Microsoft Word.app/Contents/Resources/DFonts"

// officeFonts are families Word ships or Office installs: Word lays them out
// itself. Others are not on the reader's machine, so Word would substitute a
// font of its own choosing; the writer picks one whose metrics it knows.
var officeFonts = map[string]bool{}

func init() {
	for _, name := range []string{
		"Arial", "Arial Black", "Arial Narrow", "Arial Rounded MT Bold", "Aptos", "Bahnschrift", "Book Antiqua", "Bookman Old Style",
		"Calibri", "Calibri Light", "Cambria", "Cambria Math", "Candara", "Century", "Century Gothic", "Comic Sans MS", "Consolas",
		"Constantia", "Corbel", "Courier New", "Franklin Gothic Book", "Franklin Gothic Medium", "Gabriola", "Garamond", "Georgia",
		"Gill Sans MT", "Impact", "Lucida Console", "Lucida Sans Unicode", "Microsoft Sans Serif", "Palatino Linotype", "Segoe UI",
		"Segoe UI Light", "Segoe UI Semibold", "Symbol", "Tahoma", "Times New Roman", "Trebuchet MS", "Verdana", "Webdings",
		"Wingdings", "Wingdings 2", "Wingdings 3", "MS Mincho", "MS Gothic", "SimSun", "Malgun Gothic", "Yu Gothic",
	} {
		officeFonts[fontKey(name)] = true
	}
}

var symbolFonts = map[string]bool{fontKey("Symbol"): true, fontKey("Wingdings"): true, fontKey("Wingdings 2"): true, fontKey("Wingdings 3"): true, fontKey("Webdings"): true}

// wordFamily is the family Word will lay a span out in: its own when Word has
// it, else a substitute with known metrics in the span's class.
func wordFamily(s Span) string {
	family := fontName(s.Font)
	// Word draws a symbol font only at its private-use codes (U+F020–F0FF):
	// a bullet decoded to "•" in Symbol came out blank. Real Unicode goes to
	// a text face. Spaces are neither: a bullet's trailing space sent it to
	// Arial, which draws U+F0B7 blank.
	if symbolFonts[fontKey(family)] && strings.ContainsFunc(s.Text, func(r rune) bool { return r != ' ' && (r < 0xF000 || r > 0xF0FF) }) {
		family = ""
	}
	if officeFonts[fontKey(family)] {
		return family
	}
	// An alias as wide as the source leaves no room for a glyph it lacks
	// (Word draws "▇" from a wider fallback, and lines wrapped): such a span
	// keeps the class substitute, narrower in the span's class.
	if a, ok := fontAliases[fontKey(family)]; ok && measurable(face(a, s), s.Text) {
		return a
	}
	key := fontKey(family)
	switch {
	case s.Mono || monoName.MatchString(key):
		return "Courier New"
	case s.Serif || serifName.MatchString(key):
		return "Times New Roman"
	}
	return "Arial"
}

// serifByWidth sets Serif on the spans of each font Word lacks and neither
// flags nor name class, when Times New Roman's widths fit its text clearly
// better than Arial's: Elsevier's "AdvOT1ef757c0" carries no serif flag, and
// set in Arial an article ran a page long. It changes pages in place.
func serifByWidth(pages []Page) {
	type misfit struct{ arial, times float64 }
	fonts := map[string]*misfit{}
	for _, p := range pages {
		for _, s := range p.Spans {
			text := strings.TrimRight(s.Text, " ")
			if s.Serif || s.Mono || len(text) < 4 || s.Size <= 0 || wordFamily(s) != "Arial" || officeFonts[fontKey(fontName(s.Font))] {
				continue
			}
			a, n, okA := measure(faceAdvances[face("Arial", s)], text)
			t, _, okT := measure(faceAdvances[face("Times New Roman", s)], text)
			width := s.Width
			if s.Ink > 0 && s.Ink <= s.Width {
				width = s.Ink
			}
			source := width - float64(n-1)*s.Tracking
			if !okA || !okT || a == 0 || t == 0 || source <= 0 {
				continue
			}
			m := fonts[s.Font]
			if m == nil {
				m = &misfit{}
				fonts[s.Font] = m
			}
			m.arial += math.Abs(math.Log(source/(a*s.Size))) * float64(n)
			m.times += math.Abs(math.Log(source/(t*s.Size))) * float64(n)
		}
	}
	for i := range pages {
		for j := range pages[i].Spans {
			if m := fonts[pages[i].Spans[j].Font]; m != nil && m.times < 0.7*m.arial {
				pages[i].Spans[j].Serif = true
			}
		}
	}
}

// Embedded Type 1 fonts often leave the serif flag unset (TeX's Computer
// Modern and its math fonts, URW's Nimbus Roman): their names say what the flags don't, and
// set in Arial a serif document grew by pages.
var (
	monoName  = regexp.MustCompile(`mono|courier|typewriter|^(cm|lm|ec)tt`)
	serifName = regexp.MustCompile(`roman|serif|times|mincho|ming|song|garamond|georgia|palatino|bookman|schoolbook|century|termes|stix|libertine|charter|math|^(cm|lm|ec)(r|mi|bx|ti|sl|csc|sy|ex|u|b)[0-9]|^(msam|msbm|bbold|eufm|eusm|rsfs)[0-9]`)
)

// wordFit is the character scale, in percent, and the tracking, in points,
// that give a span its source width in the family Word lays it out in; ok is
// false when unknown (a family or rune without metrics). Within 2% the scale
// stays 100 and tracking takes up the rest: a source ligature ("ﬁ" is
// narrower than "f" and "i") or a size rounded up to Word's half points would
// otherwise push a full line's last word onto a new line, and on a full page
// onto a new page.
func wordFit(s Span, family string, justified bool) (scale int, tracking float64, ok bool) {
	advances, known := faceAdvances[face(family, s)]
	if !known || s.Width <= 0 || s.Size <= 0 {
		return 0, 0, false
	}
	// A justified line stretches a word's trailing space: measured with it,
	// the stretch became glyph scale Word cannot shrink back, and full lines
	// overflowed. Fit the glyphs to their ink; Word restretches the space.
	text, width := s.Text, s.Width
	if t := strings.TrimRight(text, " "); justified && t != "" && t != text && s.Ink > 0 && s.Ink <= s.Width {
		text, width = t, s.Ink
	}
	ems, n, ok := measure(advances, text)
	if !ok || ems == 0 {
		return 0, 0, false
	}
	source := width - float64(n-1)*s.Tracking
	// Word sets the size in half points: a 6.81pt span is drawn at 7pt.
	size := float64(halfPoints(s.Size)) / 2
	ratio := source / (ems * size)
	if ratio < 0.5 || ratio > 2 {
		return 0, 0, false // not this text's real width (a stray box)
	}
	scale = 100
	if math.Abs(ratio-1) >= 0.02 {
		scale = int(math.Round(ratio * 100))
	}
	// Tracking takes up what the whole-percent scale leaves. On a justified
	// line a wider span's extra width is stretched spaces, which Word's own
	// justification redraws (tracking them overfilled tightly set lines); on
	// a ragged line it is the line's width, and dropped, Word set lines short
	// and pulled the next line's first word up.
	extra := source - ems*size*float64(scale)/100
	if justified && strings.Contains(text, " ") && extra > 0 {
		extra = 0
	}
	return scale, s.Tracking + extra/float64(n), true
}

func halfPoints(size float64) int { return max(1, int(math.Round(size*2))) }

// measure is text's width in ems and its rune count, or not ok when the
// advances lack one of its runes.
func measure(advances []uint16, text string) (ems float64, n int, ok bool) {
	for _, r := range text {
		a := advance(advances, r)
		if a == 0 {
			return 0, 0, false
		}
		ems, n = ems+float64(a)/1000, n+1
	}
	return ems, n, true
}

// scale is a span's character scale in percent; 0 means none.
func scale(s Span) float64 {
	if s.Scale == 0 {
		return 100
	}
	return float64(s.Scale)
}
func advance(advances []uint16, r rune) uint16 {
	if fullWidth(r) {
		return 1000 // a CJK font's ideographs, kana, Hangul and full-width forms
	}
	at := 0
	for _, rg := range metricRanges {
		if r >= rg[0] && r <= rg[1] {
			if i := at + int(r-rg[0]); i < len(advances) {
				return advances[i]
			}
			return 0
		}
		at += int(rg[1]-rg[0]) + 1
	}
	return 0
}

// fullWidth reports whether a rune sets one em wide in every CJK font, so a
// run of it fits its source width whichever East Asian font Word draws:
// SimSun set a Chinese line 1% wider than its source, and lines wrapped.
func fullWidth(r rune) bool {
	return r >= 0x3000 && r <= 0x30ff || r >= 0x3400 && r <= 0x4dbf || r >= 0x4e00 && r <= 0x9fff ||
		r >= 0xac00 && r <= 0xd7af || r >= 0xf900 && r <= 0xfaff || r >= 0xff01 && r <= 0xff60
}

// spaceWidth is a space's width in the family and size Word lays s out in,
// its character scale included.
func spaceWidth(s Span) float64 {
	return emsIn(s, " ") * float64(halfPoints(s.Size)) / 2 * scale(s) / 100
}

// naturalWidth is a span's width in Word with its source scale and tracking
// but none of wordFit's correction: its glyphs and spaces as Word draws them.
func naturalWidth(s Span) (float64, bool) {
	advances, known := faceAdvances[face(wordFamily(s), s)]
	if !known {
		return 0, false
	}
	ems, n, ok := measure(advances, s.Text)
	return ems*float64(halfPoints(s.Size))/2*scale(s)/100 + float64(n-1)*s.Tracking, ok
}

// face keys faceAdvances: the family and the span's bold and italic.
func face(family string, s Span) string {
	style := ""
	if s.Weight >= 600 {
		style += "b"
	}
	if s.Italic {
		style += "i"
	}
	if style != "" {
		family += "|" + style
	}
	return family
}

// measurable reports whether a face's width table covers every rune of text.
func measurable(face, text string) bool {
	advances, known := faceAdvances[face]
	_, _, ok := measure(advances, text)
	return known && ok
}
