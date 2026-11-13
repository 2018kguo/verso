package verso

import (
	"math"
	"strings"
)

//go:generate go run gen_metrics.go /Applications/LibreOffice.app/Contents/Resources/fonts/truetype

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

// wordFamily is the family Word will lay a span out in: its own when Word has
// it, else a substitute with known metrics in the span's class.
func wordFamily(s Span) string {
	family := fontName(s.Font)
	if officeFonts[fontKey(family)] {
		return family
	}
	if a, ok := fontAliases[fontKey(family)]; ok {
		return a
	}
	switch {
	case s.Mono:
		return "Courier New"
	case s.Serif:
		return "Times New Roman"
	}
	return "Arial"
}

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
	ems, n := 0.0, 0
	for _, r := range s.Text {
		a := advance(advances, r)
		if a == 0 {
			return 0, 0, false
		}
		ems += float64(a) / 1000
		n++
	}
	source := s.Width - float64(n-1)*s.Tracking
	// Word sets the size in half points: a 6.81pt span is drawn at 7pt.
	size := float64(halfPoints(s.Size)) / 2
	if ems == 0 {
		return 0, 0, false
	}
	ratio := source / (ems * size)
	if ratio < 0.5 || ratio > 2 {
		return 0, 0, false // not this text's real width (a stray box)
	}
	scale = 100
	if math.Abs(ratio-1) >= 0.02 {
		scale = int(math.Round(ratio * 100))
	}
	// Tracking takes up what the whole-percent scale leaves. A wider span
	// with spaces may be justified: past what the half-point size rounded
	// away, its extra width is stretched spaces, which Word's own
	// justification redraws.
	extra := source - ems*size*float64(scale)/100
	if strings.Contains(s.Text, " ") && extra > 0 {
		// the width the half-point size took off (a scale already restores
		// it; justification spreads it over the spaces, and tracking on a
		// tightly set justified line overfills it)
		rounded := 0.0
		if scale == 100 && !justified {
			rounded = math.Max(0, ems*(s.Size-size))
		}
		extra = math.Min(extra, rounded)
	}
	return scale, s.Tracking + extra/float64(n), true
}

func halfPoints(size float64) int { return max(1, int(math.Round(size*2))) }
func advance(advances []uint16, r rune) uint16 {
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

// spaceWidth is a space's width in the family and size Word lays s out in,
// its character scale included.
func spaceWidth(s Span) float64 {
	scale := float64(s.Scale)
	if scale == 0 {
		scale = 100
	}
	return emsIn(s, " ") * float64(halfPoints(s.Size)) / 2 * scale / 100
}

// naturalWidth is a span's width in Word with its source scale and tracking
// but none of wordFit's correction: its glyphs and spaces as Word draws them.
func naturalWidth(s Span) (float64, bool) {
	advances, known := faceAdvances[face(wordFamily(s), s)]
	if !known {
		return 0, false
	}
	scale := float64(s.Scale)
	if scale == 0 {
		scale = 100
	}
	ems, n := 0.0, 0
	for _, r := range s.Text {
		a := advance(advances, r)
		if a == 0 {
			return 0, false
		}
		ems, n = ems+float64(a)/1000, n+1
	}
	return ems*float64(halfPoints(s.Size))/2*scale/100 + float64(n-1)*s.Tracking, true
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
