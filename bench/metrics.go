package main

import (
	"archive/zip"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"unicode"
)

type textScore struct {
	Status        string   `json:"status"`
	SourceTokens  int      `json:"sourceTokens"`
	OutputTokens  int      `json:"outputTokens"`
	MatchedTokens int      `json:"matchedTokens"`
	Recall        *float64 `json:"recall"`
	Precision     *float64 `json:"precision"`
}

func ratio(a, b int) *float64 {
	if b == 0 {
		return nil
	}
	v := float64(a) / float64(b)
	return &v
}

// CJK characters are individual tokens; Latin/Arabic/etc use letter/number runs.
// This is a lexical multiset, not a claim about order, semantics or editability.
func tokens(s string) []string {
	var result []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			result = append(result, b.String())
			b.Reset()
		}
	}
	for _, r := range strings.ToLower(s) {
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
			flush()
			result = append(result, string(r))
		} else if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return result
}
func scoreText(source, output string) textScore {
	a, b := tokens(source), tokens(output)
	counts := map[string]int{}
	for _, t := range b {
		counts[t]++
	}
	matched := 0
	for _, t := range a {
		if counts[t] > 0 {
			counts[t]--
			matched++
		}
	}
	s := textScore{Status: "measured", SourceTokens: len(a), OutputTokens: len(b), MatchedTokens: matched, Recall: ratio(matched, len(a)), Precision: ratio(matched, len(b))}
	if len(a) == 0 {
		s.Status = "no_source_text"
	}
	return s
}

const wordNS = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"

// wordText is the text a reader sees: the body, notes, and each header and
// footer once per section that shows it (a section without its own reference
// inherits the previous one), with a PAGE field reading that section's number.
func wordText(path string) (string, error) {
	z, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer z.Close()
	if len(z.File) > 10000 {
		return "", errors.New("too many ZIP parts")
	}
	parts := map[string]string{}
	seen := map[string]bool{}
	var total uint64
	for _, f := range z.File {
		if seen[f.Name] {
			return "", errors.New("duplicate ZIP part")
		}
		seen[f.Name] = true
		wanted := f.Name == "word/document.xml" || strings.HasPrefix(f.Name, "word/header") || strings.HasPrefix(f.Name, "word/footer") || f.Name == "word/footnotes.xml" || f.Name == "word/endnotes.xml"
		if !(wanted && strings.HasSuffix(f.Name, ".xml")) && f.Name != "word/_rels/document.xml.rels" {
			continue
		}
		total += f.UncompressedSize64
		if total > 64<<20 {
			return "", errors.New("DOCX text parts exceed 64 MiB")
		}
		r, e := f.Open()
		if e != nil {
			return "", e
		}
		data, e := io.ReadAll(io.LimitReader(r, 64<<20))
		r.Close()
		if e != nil {
			return "", e
		}
		parts[f.Name] = string(data)
	}
	body, ok := parts["word/document.xml"]
	if !ok {
		return "", errors.New("no main document")
	}
	var b strings.Builder
	sections, err := partText(&b, body, false)
	if err != nil {
		return "", err
	}
	for _, name := range []string{"word/footnotes.xml", "word/endnotes.xml"} {
		if _, err := partText(&b, parts[name], false); err != nil {
			return "", err
		}
	}
	targets := map[string]string{}
	var rels struct {
		Items []struct {
			ID     string `xml:"Id,attr"`
			Target string `xml:"Target,attr"`
		} `xml:"Relationship"`
	}
	if xml.Unmarshal([]byte(parts["word/_rels/document.xml.rels"]), &rels) == nil {
		for _, r := range rels.Items {
			targets[r.ID] = "word/" + strings.TrimPrefix(r.Target, "/word/")
		}
	}
	margins := map[string]string{} // relationship id -> text, \x00 at PAGE
	for id, target := range targets {
		var m strings.Builder
		if _, err := partText(&m, parts[target], true); err != nil {
			return "", err
		}
		margins[id] = m.String()
	}
	// ponytail: one count per section, exact for one-section-per-page output;
	// a multi-page section's header counts once (pages per section need layout).
	page := 1
	var header, footer string
	for _, sec := range sections {
		if sec.start >= 0 {
			page = sec.start
		}
		if sec.header != "" {
			header = sec.header
		}
		if sec.footer != "" {
			footer = sec.footer
		}
		for _, id := range []string{header, footer} {
			b.WriteString(strings.ReplaceAll(margins[id], "\x00", fmt.Sprint(page)))
		}
		page++
	}
	return b.String(), nil
}

func attr(t xml.StartElement, local string) string {
	for _, a := range t.Attr {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

type sectionRefs struct {
	header, footer string // default header and footer relationship ids
	start          int    // pgNumType start, or -1
}

// partText appends a part's text and returns its sections. With numbered,
// each PAGE field's result is \x00, for the caller to number.
func partText(b *strings.Builder, data string, numbered bool) ([]sectionRefs, error) {
	if data == "" {
		return nil, nil
	}
	d := xml.NewDecoder(strings.NewReader(data))
	var sections []sectionRefs
	inText, inPage := false, false
	for {
		t, e := d.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
		switch t := t.(type) {
		case xml.StartElement:
			if t.Name.Space != wordNS {
				continue
			}
			switch t.Name.Local {
			case "t":
				inText = true
			case "tab", "br":
				b.WriteByte(' ')
			case "fldSimple":
				if numbered && strings.TrimSpace(attr(t, "instr")) == "PAGE" {
					inPage = true
					b.WriteByte(0)
				}
			case "sectPrChange":
				d.Skip() // a tracked change's old section properties
			case "sectPr":
				sections = append(sections, sectionRefs{start: -1})
			case "headerReference", "footerReference":
				if n := len(sections); n > 0 && attr(t, "type") == "default" {
					if t.Name.Local == "headerReference" {
						sections[n-1].header = attr(t, "id")
					} else {
						sections[n-1].footer = attr(t, "id")
					}
				}
			case "pgNumType":
				if v, err := strconv.Atoi(attr(t, "start")); err == nil && len(sections) > 0 {
					sections[len(sections)-1].start = v
				}
			}
		case xml.EndElement:
			if t.Name.Space != wordNS {
				continue
			}
			switch t.Name.Local {
			case "t":
				inText = false
			case "fldSimple":
				inPage = false
			case "p", "tc":
				b.WriteByte('\n')
			}
		case xml.CharData:
			if inText && !inPage {
				b.Write(t)
			}
		}
	}
	b.WriteByte(' ')
	return sections, nil
}

type boxWord struct {
	XMin float64 `xml:"xMin,attr"`
	YMin float64 `xml:"yMin,attr"`
	XMax float64 `xml:"xMax,attr"`
	YMax float64 `xml:"yMax,attr"`
	Text string  `xml:",chardata"`
}
type boxPage struct {
	Width  float64   `xml:"width,attr"`
	Height float64   `xml:"height,attr"`
	Words  []boxWord `xml:"word"`
}

func readBoxes(s string) ([]boxPage, error) {
	d := xml.NewDecoder(strings.NewReader(s))
	var pages []boxPage
	for {
		t, e := d.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
		if t, ok := t.(xml.StartElement); ok && t.Name.Local == "page" {
			var p boxPage
			if e = d.DecodeElement(&p, &t); e != nil {
				return nil, e
			}
			if p.Width <= 0 || p.Height <= 0 || math.IsNaN(p.Width) || math.IsNaN(p.Height) || math.IsInf(p.Width, 0) || math.IsInf(p.Height, 0) {
				return nil, errors.New("invalid page geometry")
			}
			pages = append(pages, p)
		}
	}
	if len(pages) == 0 {
		return nil, errors.New("no bbox pages")
	}
	return pages, nil
}

type positionScore struct {
	Status           string   `json:"status"`
	SourceWords      int      `json:"sourceWords"`
	MatchedWords     int      `json:"matchedWords"`
	Within2Percent   int      `json:"within2Percent"`
	PositionedRecall *float64 `json:"positionedRecall"`
	MeanDistance     *float64 `json:"meanNormalizedDistance"`
	MaxDistancePt    *float64 `json:"maxDistancePt"`
}

func positions(a, b boxPage) positionScore {
	s := positionScore{Status: "measured", SourceWords: len(a.Words)}
	if len(a.Words) > 20000 || len(b.Words) > 20000 {
		s.Status = "word_limit"
		return s
	}
	used := make([]bool, len(b.Words))
	candidates := map[string][]int{}
	for j, v := range b.Words {
		key := strings.Join(tokens(v.Text), " ")
		candidates[key] = append(candidates[key], j)
	}
	comparisons := 0
	var sum, maximum float64
	for _, w := range a.Words {
		key := strings.Join(tokens(w.Text), " ")
		if key == "" {
			s.SourceWords--
			continue
		}
		best := -1
		distance := math.Inf(1)
		absoluteDistance := 0.0
		for _, j := range candidates[key] {
			comparisons++
			if comparisons > 2_000_000 {
				s.Status = "comparison_limit"
				s.PositionedRecall = nil
				s.MeanDistance = nil
				return s
			}
			v := b.Words[j]
			if used[j] {
				continue
			}
			dx := (w.XMin+w.XMax)/(2*a.Width) - (v.XMin+v.XMax)/(2*b.Width)
			dy := (w.YMin+w.YMax)/(2*a.Height) - (v.YMin+v.YMax)/(2*b.Height)
			d := math.Hypot(dx, dy)
			if d < distance {
				best = j
				distance = d
				absoluteDistance = math.Hypot(dx*a.Width, dy*a.Height)
			}
		}
		if best >= 0 {
			used[best] = true
			s.MatchedWords++
			maximum = math.Max(maximum, absoluteDistance)
			sum += distance
			if distance <= .02 {
				s.Within2Percent++
			}
		}
	}
	s.PositionedRecall = ratio(s.Within2Percent, s.SourceWords)
	if s.MatchedWords > 0 {
		v := sum / float64(s.MatchedWords)
		s.MeanDistance = &v
		s.MaxDistancePt = &maximum
	}
	return s
}

type inkScore struct {
	SourceInk      int      `json:"sourceInkPixels"`
	OutputInk      int      `json:"outputInkPixels"`
	Recall         *float64 `json:"recall"`
	Precision      *float64 `json:"precision"`
	F1             *float64 `json:"f1"`
	SameDimensions bool     `json:"sameDimensions"`
}

func loadPNG(path string) (image.Image, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	cfg, e := png.DecodeConfig(f)
	if e != nil {
		return nil, e
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 4_000_000 {
		return nil, fmt.Errorf("raster exceeds 4 million pixels")
	}
	if _, e = f.Seek(0, 0); e != nil {
		return nil, e
	}
	return png.Decode(f)
}
func ink(im image.Image, w, h int) []bool {
	a := make([]bool, w*h)
	if im == nil {
		return a
	}
	b := im.Bounds()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x >= b.Dx() || y >= b.Dy() {
				continue
			}
			c := color.NRGBAModel.Convert(im.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
			lum := (299*int(c.R) + 587*int(c.G) + 114*int(c.B)) / 1000
			lum = (lum*int(c.A) + 255*(255-int(c.A))) / 255
			a[y*w+x] = lum < 220
		}
	}
	return a
}
func inkMatch(a, b image.Image) inkScore {
	w, h := 0, 0
	if a != nil {
		w = a.Bounds().Dx()
		h = a.Bounds().Dy()
	}
	if b != nil {
		w = max(w, b.Bounds().Dx())
		h = max(h, b.Bounds().Dy())
	}
	s := inkScore{SameDimensions: a != nil && b != nil && a.Bounds().Size() == b.Bounds().Size()}
	aa, bb := ink(a, w, h), ink(b, w, h)
	matched := func(source, target []bool) (int, int) {
		n, m := 0, 0
		for i, on := range source {
			if !on {
				continue
			}
			n++
			x, y := i%w, i/w
			found := false
			for yy := max(0, y-2); yy <= min(h-1, y+2) && !found; yy++ {
				for xx := max(0, x-2); xx <= min(w-1, x+2); xx++ {
					if target[yy*w+xx] {
						found = true
						break
					}
				}
			}
			if found {
				m++
			}
		}
		return n, m
	}
	var ar, br int
	s.SourceInk, ar = matched(aa, bb)
	s.OutputInk, br = matched(bb, aa)
	s.Recall = ratio(ar, s.SourceInk)
	s.Precision = ratio(br, s.OutputInk)
	if s.SourceInk+s.OutputInk == 0 {
		return s
	}
	f := 0.0
	if s.Recall != nil && s.Precision != nil && *s.Recall+*s.Precision > 0 {
		f = 2 * *s.Recall * *s.Precision / (*s.Recall + *s.Precision)
	}
	s.F1 = &f
	return s
}
