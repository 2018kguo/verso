package verso

import (
	"cmp"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

var digitRun = regexp.MustCompile(`[0-9]+`)

// Only a line shaped like a page number may count pages: "3", "- 3 -",
// "Page 3", "3 of 10". A counting title ("Exhibit 3") stays in the body.
var pageNumber = regexp.MustCompile(`(?i)^[\s\-–—(\[]*(?:page|p\.|pg\.?)?\s*#\s*(?:(?:of|/)\s*#)?[\s\-–—)\]]*$`)

// A running line repeats in a page's top or bottom twelfth (inside a 1in
// margin, where body text rarely starts): the same text at the same height
// on three or more pages, its numbers constant or the page's own.
type running struct {
	foot      bool
	template  string // the line's text with each digit run as #
	line      Line   // as drawn on its first page, underlines marked
	first     int    // that page's index
	pages     map[int]Line
	digits    map[int][]int // each page's digit runs, in order
	ambiguous bool          // twice on one page, or a number split across spans
	counter   int           // the digit run that counts pages, or -1
}

// runningLines finds a document's running heads and feet, and the number the
// first page shows (-1 when no line counts pages).
func runningLines(pages []Page) ([]*running, int) {
	if len(pages) < 2 {
		return nil, -1
	}
	byKey := map[string]*running{}
	var order []*running
	for i, p := range pages {
		for _, l := range lines(p.Spans) {
			foot := l.Y >= p.Height*11/12
			inMargin := foot || l.Y+l.Height <= p.Height/12
			// A linked line stays in the body, where its relationship lives;
			// a line in a ruled grid (a header table) stays with its grid.
			if !inMargin || slices.ContainsFunc(l.Spans, func(s Span) bool { return s.Link != "" }) || ruled(l, p.Segments) {
				continue
			}
			template := digitRun.ReplaceAllString(l.text(), "#")
			// Baselines are compared in 2pt buckets.
			key := fmt.Sprintf("%t|%.0f|%s", foot, l.Baseline/2, template)
			r := byKey[key]
			if r == nil {
				r = &running{foot: foot, template: template, line: underlined(l, p.Segments), first: i, pages: map[int]Line{}, digits: map[int][]int{}, counter: -1}
				byKey[key] = r
				order = append(order, r)
			}
			d, whole := spanDigits(l)
			_, twice := r.pages[i]
			r.ambiguous = r.ambiguous || twice || !whole
			r.pages[i], r.digits[i] = l, d
		}
	}
	start := -1
	var kept []*running
	for _, r := range order {
		// Three pages (or every page of a shorter document) make it running.
		if len(r.pages) < min(3, len(pages)) || r.ambiguous {
			continue
		}
		want := r.digits[r.first]
		ok := true
		for _, d := range r.digits {
			ok = ok && len(d) == len(want) // "Item #" and "Item 5" share a template
		}
		counter, offset := -1, 0
		for j := 0; ok && j < len(want); j++ {
			constant, counting := true, true
			for i, d := range r.digits {
				constant = constant && d[j] == want[j]
				counting = counting && d[j]-i == want[j]-r.first
			}
			switch {
			case constant:
			case counting && counter < 0 && pageNumber.MatchString(r.template):
				counter, offset = j, want[j]-r.first
			default:
				ok = false
			}
		}
		// Every counting line must agree on the first page's number.
		if !ok || counter >= 0 && (offset < 0 || start >= 0 && offset != start) {
			continue
		}
		if counter >= 0 {
			start = offset
		}
		r.counter = counter
		kept = append(kept, r)
	}
	return kept, start
}

// spanDigits reads a line's digit runs, and reports whether each lies in one
// span (a PAGE field replaces a run inside one span).
func spanDigits(l Line) ([]int, bool) {
	var d []int
	for _, s := range l.Spans {
		for _, run := range digitRun.FindAllString(s.Text, -1) {
			v, _ := strconv.Atoi(run)
			d = append(d, v)
		}
	}
	return d, len(d) == len(digitRun.FindAllString(l.text(), -1))
}

// ruled reports whether a vertical rule crosses the line: it sits in a grid.
func ruled(l Line, segments []Segment) bool {
	return slices.ContainsFunc(segments, func(s Segment) bool {
		s = norm(s)
		return s.Y2-s.Y1 > 10 && s.X2-s.X1 < 1 && s.Y1 < l.Y+l.Height && s.Y2 > l.Y
	})
}

// underlined marks the line's spans that a rule on its page underlines:
// layout does that for body text, and this line leaves the body.
func underlined(l Line, segments []Segment) Line {
	l.Spans = slices.Clone(l.Spans)
	for _, s := range segments {
		underlines(l.Spans, norm(s))
	}
	return l
}

// withoutLine drops the spans a running line was built from.
func withoutLine(spans []Span, l Line) []Span {
	return slices.DeleteFunc(slices.Clone(spans), func(s Span) bool {
		return slices.ContainsFunc(l.Spans, func(t Span) bool { return t.X == s.X && t.Y == s.Y && t.Text == s.Text })
	})
}

// pageField splits the span holding a line's page-counting digit run so that
// run becomes Word's PAGE field.
func pageField(l Line, run int) Line {
	if run < 0 {
		return l
	}
	out := l
	out.Spans = nil
	seen := 0
	for _, s := range l.Spans {
		locs := digitRun.FindAllStringIndex(s.Text, -1)
		if run < seen || run >= seen+len(locs) {
			seen += len(locs)
			out.Spans = append(out.Spans, s)
			continue
		}
		at := locs[run-seen]
		seen += len(locs)
		perRune := s.Width / float64(max(1, utf8.RuneCountInString(s.Text)))
		x := s.X
		for k, text := range []string{s.Text[:at[0]], s.Text[at[0]:at[1]], s.Text[at[1]:]} {
			if text == "" {
				continue
			}
			part := s
			part.Text, part.X, part.Width, part.Ink = text, x, perRune*float64(utf8.RuneCountInString(text)), 0
			part.PageNumber = k == 1
			x += part.Width
			out.Spans = append(out.Spans, part)
		}
	}
	return out
}

func shifted(l Line, dx float64) Line {
	l.X += dx
	l.Spans = slices.Clone(l.Spans)
	for i := range l.Spans {
		l.Spans[i].X += dx
	}
	return l
}

// marginPart is a header or footer: its lines as exact-height paragraphs at
// their source positions. Lines on one baseline share a paragraph, tabbed
// apart (baselineLines would fold the pieces again but drop their marked
// underlines and page fields). A footer is anchored to the page's bottom, so
// its last paragraph ends with the space below its text.
func (w *writer) marginPart(foot bool, ls []Line, height float64) string {
	tag := "hdr"
	if foot {
		tag = "ftr"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `%s<w:%s xmlns:w="%s" xmlns:r="%s">`, declaration, tag, word, relationships)
	slices.SortFunc(ls, func(a, b Line) int { return cmp.Or(cmp.Compare(a.Baseline, b.Baseline), cmp.Compare(a.X, b.X)) })
	var merged []Line
	for _, l := range ls {
		if n := len(merged); n > 0 && math.Abs(merged[n-1].Baseline-l.Baseline) < 1 {
			m := &merged[n-1]
			m.Spans = append(slices.Clone(m.Spans), l.Spans...)
			m.Y, m.Height = min(m.Y, l.Y), max(m.Y+m.Height, l.Y+l.Height)-min(m.Y, l.Y)
			m.Width = l.X + l.Width - m.X
			continue
		}
		merged = append(merged, l)
	}
	// Each line's exact-height box starts where Word puts its baseline at the
	// source's.
	top := func(l Line) float64 { return l.Baseline - baselineDepth(l.Height) }
	cursor := 0.0
	if foot && len(merged) > 0 {
		cursor = top(merged[0])
	}
	for i, l := range merged {
		after := 0.0
		if foot && i == len(merged)-1 {
			after = height - (top(l) + l.Height)
		}
		fmt.Fprintf(&b, `<w:p><w:pPr>%s<w:spacing w:before="%d" w:after="%d" w:line="%d" w:lineRule="exact"/><w:ind w:left="%d" w:right="0"/><w:jc w:val="left"/></w:pPr>`, tabStops([]Line{l}, 0), max(0, twips(top(l)-cursor)), max(0, twips(after)), max(1, twips(l.Height)), twips(l.X))
		w.runs(&b, l)
		b.WriteString(`</w:p>`)
		cursor = max(cursor, top(l)+l.Height)
	}
	if len(merged) == 0 {
		b.WriteString(`<w:p><w:pPr><w:spacing w:before="0" w:after="0" w:line="1" w:lineRule="exact"/><w:rPr><w:sz w:val="1"/></w:rPr></w:pPr></w:p>`)
	}
	fmt.Fprintf(&b, `</w:%s>`, tag)
	return b.String()
}

// margins moves running lines into header and footer parts. It returns each
// page's sectPr references (every section names its own parts, so a page
// without a running line shows none), the pages without those lines, and the
// first page's number (-1 when nothing counts pages).
func (w *writer) margins(pages []Page) ([]string, []Page, int) {
	refs := make([]string, len(pages))
	body := slices.Clone(pages)
	found, start := runningLines(pages)
	if len(found) == 0 {
		return refs, body, -1
	}
	ids := map[string]string{} // part contents -> relationship id
	count := map[string]int{}
	part := func(kind string, ls []Line, height float64) string {
		xml := w.marginPart(kind == "footer", ls, height)
		if id, ok := ids[xml]; ok {
			return id
		}
		count[kind]++
		name := fmt.Sprintf("%s%d.xml", kind, count[kind])
		w.files["word/"+name] = []byte(xml)
		ids[xml] = w.relationship(kind, name, false)
		return ids[xml]
	}
	fields := make([]Line, len(found))
	for k, r := range found {
		fields[k] = pageField(r.line, r.counter)
	}
	for i := range body {
		var heads, feet []Line
		for k, r := range found {
			l, ok := r.pages[i]
			if !ok {
				continue
			}
			body[i].Spans = withoutLine(body[i].Spans, l)
			// Centred numbers shift a little as they grow; a mirrored head
			// (left on even pages, right on odd) keeps its own side.
			line := fields[k]
			if dx := l.X - line.X; math.Abs(dx) > 6 {
				line = shifted(line, dx)
			}
			if r.foot {
				feet = append(feet, line)
			} else {
				heads = append(heads, line)
			}
		}
		refs[i] = fmt.Sprintf(`<w:headerReference w:type="default" r:id="%s"/><w:footerReference w:type="default" r:id="%s"/>`, part("header", heads, pages[i].Height), part("footer", feet, pages[i].Height))
	}
	return refs, body, start
}
