// Package verso reconstructs editable Word documents from PDF page geometry.
// PDF decoding belongs to Recto; layout and Word XML are written in Go.
package verso

import (
	"fmt"
	"math"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

type Box struct{ X, Y, Width, Height float64 }
type Span struct {
	Box
	Text, Font, Color, Link string
	Baseline, Size          float64
	Tracking                float64
	Scale                   int // horizontal scale (Tz) in percent; 0 means 100
	Weight                  int
	Italic                  bool
	Serif, Mono             bool    // the PDF font's flags, for Word's fallback
	Tab                     bool    // drawn after a tab: Word reaches it with a tab stop
	Rise                    float64 `json:"-"` // superscript or subscript offset, points up
	Underline               bool    `json:"-"` // set from a rule under the span
	PageNumber              bool    `json:"-"` // written as Word's PAGE field
	Leader                  bool    `json:"-"` // after drawn leader dots: a right tab with a dot leader
	Ink                     float64 // width to the last glyph that isn't a space; 0 when unknown
}
type Segment struct{ X1, Y1, X2, Y2 float64 }
type Image struct {
	Box
	PNG []byte
}
type Page struct {
	Width, Height float64
	Spans         []Span
	Segments      []Segment
	Images        []Image
	Warnings      []string
}
type Line struct {
	Spans                         []Span
	X, Y, Baseline, Width, Height float64
}
type Cell struct {
	Spans           []Span
	Start, Columns  int
	Continue, Merge bool
}
type Table struct {
	X, Y                float64
	XS, YS              []float64
	Rows                [][]Cell
	OpenLeft, OpenRight bool // no rule on that side: the grid's rules run on past it
}

func (t Table) right() float64  { return t.XS[len(t.XS)-1] }
func (t Table) bottom() float64 { return t.YS[len(t.YS)-1] }

// openSides adds an edge where a grid's top and bottom rules run on past
// its outermost vertical rule: forms often leave the page-edge side open
// ("Employer identification number" beside the last rule), and a grid
// bounded at its last vertical squeezed a full-width row's text into a
// wrapped, clipped cell.
func openSides(g []Segment, xs, ys []float64) (_ []float64, left, right bool) {
	reach := func(y float64) (lo, hi float64) {
		lo, hi = math.Inf(1), math.Inf(-1)
		for _, s := range g {
			if s.Y2-s.Y1 < 1 && math.Abs(s.Y1-y) < 1.5 {
				lo, hi = math.Min(lo, s.X1), math.Max(hi, s.X2)
			}
		}
		return lo, hi
	}
	topLo, topHi := reach(ys[0])
	bottomLo, bottomHi := reach(ys[len(ys)-1])
	if hi := math.Min(topHi, bottomHi); hi > xs[len(xs)-1]+3 && math.Abs(topHi-bottomHi) < 3 {
		xs, right = append(xs, hi), true
	}
	if lo := math.Max(topLo, bottomLo); lo < xs[0]-3 && math.Abs(topLo-bottomLo) < 3 {
		xs, left = append([]float64{lo}, xs...), true
	}
	return xs, left, right
}

type Layout struct {
	Items  []Item
	Tables []Table
	Images []Image
}

// Bounds precede geometry algorithms, not only the writer or child process.
var colorPattern = regexp.MustCompile(`^[0-9A-Fa-f]{6}$`)
var anchorPattern = regexp.MustCompile(`^#page[1-9][0-9]*$`)

// wordLink reports whether Word can carry a link: none, a page anchor, or a
// web or mail address.
func wordLink(link string) bool {
	if link == "" || anchorPattern.MatchString(link) {
		return true
	}
	u, err := url.Parse(link)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http" || u.Scheme == "mailto")
}

func xmlText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if (r < 32 && r != '\t' && r != '\n' && r != '\r') || r == 0xfffe || r == 0xffff {
			return false
		}
	}
	return true
}

func (p Page) validate() error {
	if !finite(p.Width) || !finite(p.Height) || p.Width < 1 || p.Height < 1 || p.Width > maxPage || p.Height > maxPage {
		return fmt.Errorf("page size outside Word's 22-inch limit")
	}
	if len(p.Spans) > 20000 || len(p.Segments) > 10000 || len(p.Images) > 100 {
		return fmt.Errorf("page complexity limit")
	}
	for _, s := range p.Spans {
		if math.IsNaN(s.Tracking) || math.IsInf(s.Tracking, 0) || math.Abs(s.Tracking) > 128 {
			return fmt.Errorf("invalid character spacing")
		}
		switch {
		case !xmlText(s.Text):
			return fmt.Errorf("text is not XML-safe: %q", s.Text)
		case !xmlText(s.Font):
			return fmt.Errorf("font name is not XML-safe: %q", s.Font)
		case !xmlText(s.Link):
			return fmt.Errorf("link is not XML-safe: %q", s.Link)
		case !colorPattern.MatchString(s.Color):
			return fmt.Errorf("color is not RRGGBB: %q", s.Color)
		}
		if !wordLink(s.Link) {
			return fmt.Errorf("unsupported hyperlink")
		}
		if !finite(s.X) || !finite(s.Y) || !finite(s.Width) || !finite(s.Height) || !finite(s.Baseline) || !finite(s.Size) || s.Size < 0.5 || s.Size > 400 || s.Width < 0 || s.Height < 0 || s.X < -2 || s.Y < -2 || s.X+s.Width > p.Width+2 || s.Y+s.Height > p.Height+2 {
			return fmt.Errorf("invalid or out-of-page text geometry: %q at %.1f,%.1f %.1fx%.1f size %.2f on %.0fx%.0f", s.Text, s.X, s.Y, s.Width, s.Height, s.Size, p.Width, p.Height)
		}
		if strings.ContainsRune(s.Text, '\ufffd') {
			return fmt.Errorf("unmapped text requires OCR or manual review")
		}
	}
	for _, s := range p.Segments {
		for _, n := range []float64{s.X1, s.X2, s.Y1, s.Y2} {
			if !finite(n) || n < -2 || n > maxPage+2 {
				return fmt.Errorf("invalid path geometry")
			}
		}
	}
	return nil
}

// maxPage is Word's largest page side: 22 inches, in points.
const maxPage = 1584

func finite(v ...float64) bool {
	for _, x := range v {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return false
		}
	}
	return true
}

// sanitize drops what a PDF draws outside its page (clipped away by every
// viewer) and scales a page beyond Word's 22-inch limit to fit, so strict
// validation then refuses only malformed input. It returns what it changed.
func (p *Page) sanitize() []string {
	var notes []string
	if finite(p.Width, p.Height) && p.Width >= 1 && p.Height >= 1 {
		if k := maxPage / math.Max(p.Width, p.Height); k < 1 {
			// Clamped: 2592*(1584/2592) is 1584.0000000000002.
			p.Width, p.Height = math.Min(p.Width*k, maxPage), math.Min(p.Height*k, maxPage)
			for i := range p.Spans {
				s := &p.Spans[i]
				s.X, s.Y, s.Width, s.Height, s.Baseline, s.Size, s.Tracking, s.Ink = s.X*k, s.Y*k, s.Width*k, s.Height*k, s.Baseline*k, math.Max(0.5, s.Size*k), s.Tracking*k, s.Ink*k
			}
			for i := range p.Segments {
				g := &p.Segments[i]
				g.X1, g.Y1, g.X2, g.Y2 = g.X1*k, g.Y1*k, g.X2*k, g.Y2*k
			}
			for i := range p.Images {
				m := &p.Images[i]
				m.X, m.Y, m.Width, m.Height = m.X*k, m.Y*k, m.Width*k, m.Height*k
			}
			notes = append(notes, fmt.Sprintf("Page scaled to %.0f%% to fit Word's 22-inch limit", k*100))
		}
	}
	spans := p.Spans[:0]
	links := 0
	for _, s := range p.Spans {
		// A link Word cannot carry (a malformed escape, another scheme)
		// keeps its text without the link.
		if !wordLink(s.Link) {
			s.Link = ""
			links++
		}
		if finite(s.X, s.Y, s.Width, s.Height, s.Size) && s.Size >= 0.5 && s.Size <= 400 && s.X+s.Width > 0 && s.Y+s.Height > 0 && s.X < p.Width && s.Y < p.Height {
			s.X, s.Y = math.Max(s.X, 0), math.Max(s.Y, 0)
			s.Width, s.Height = math.Min(s.Width, p.Width-s.X), math.Min(s.Height, p.Height-s.Y)
			spans = append(spans, s)
		}
	}
	if links > 0 {
		notes = append(notes, fmt.Sprintf("%d text runs keep their text without a link Word cannot carry", links))
	}
	if dropped := len(p.Spans) - len(spans); dropped > 0 {
		notes = append(notes, fmt.Sprintf("%d text runs outside the page, under 0.5pt or over 400pt were left out", dropped))
	}
	p.Spans = spans
	in := func(v, limit float64) bool { return finite(v) && v >= -2 && v <= limit+2 }
	segments := p.Segments[:0]
	for _, g := range p.Segments {
		if in(g.X1, p.Width) && in(g.X2, p.Width) && in(g.Y1, p.Height) && in(g.Y2, p.Height) {
			segments = append(segments, g)
		}
	}
	p.Segments = segments
	if len(p.Segments) > 10000 || len(borders(*p)) > maxBorders {
		p.Segments = nil
		notes = append(notes, "Too many drawn lines to find ruled tables; their text stays in the flow")
	}
	return notes
}

func baselineLines(spans []Span) []Line {
	spans = append([]Span(nil), spans...)
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].Baseline < spans[j].Baseline })
	var result []Line
	for _, s := range spans {
		if strings.TrimSpace(s.Text) == "" {
			continue
		}
		if len(result) == 0 || math.Abs(result[len(result)-1].Baseline-s.Baseline) > math.Min(1.5, s.Size*0.18) {
			result = append(result, Line{Spans: []Span{s}, Baseline: s.Baseline, X: s.X, Y: s.Y, Width: s.Width, Height: s.Height})
		} else {
			l := &result[len(result)-1]
			right := math.Max(l.X+l.Width, s.X+s.Width)
			bottom := math.Max(l.Y+l.Height, s.Y+s.Height)
			l.X = math.Min(l.X, s.X)
			l.Y = math.Min(l.Y, s.Y)
			l.Width = right - l.X
			l.Height = bottom - l.Y
			l.Spans = append(l.Spans, s)
		}
	}
	result = scripts(result)
	for i := range result {
		sort.SliceStable(result[i].Spans, func(a, b int) bool { return result[i].Spans[a].X < result[i].Spans[b].X })
	}
	return result
}

// scripts folds a short line of smaller text sitting within a larger line's
// ascent or descent ("23rd", footnote marks, "H2O") into that line as
// raised or lowered text, instead of a line of its own.
func scripts(ls []Line) []Line {
	keep := make([]bool, len(ls))
	for i := range keep {
		keep[i] = true
	}
	for i, l := range ls {
		// Math sets each index apart ("b₇ b₆ … b₀"): a line of many short
		// scripts folds too.
		n, short := 0, true
		for _, s := range l.Spans {
			c := utf8.RuneCountInString(strings.TrimSpace(s.Text))
			n, short = n+c, short && c <= 3
		}
		if (n > 12 || len(l.Spans) > 3) && (!short || n > 40) {
			continue
		}
		size := l.size()
		for j := range ls {
			m := ls[j]
			if j == i || !keep[j] || m.size() < size/0.85 || math.Abs(m.Baseline-l.Baseline) > 0.6*m.size() {
				continue
			}
			if l.X < m.X-1 || l.X+l.Width > m.X+m.Width+m.size() {
				continue
			}
			for _, s := range l.Spans {
				s.Rise = m.Baseline - s.Baseline
				ls[j].Spans = append(ls[j].Spans, s)
			}
			keep[i] = false
			break
		}
	}
	out := ls[:0]
	for i, l := range ls {
		if keep[i] {
			out = append(out, l)
		}
	}
	return out
}

func lines(spans []Span) []Line {
	// A baseline can cross two newspaper columns: split it at wide gaps so
	// the gutter never becomes spaces inside one paragraph.
	var blocks []Line
	for _, l := range baselineLines(spans) {
		start := 0
		for i := 1; i <= len(l.Spans); i++ {
			if i < len(l.Spans) {
				g := gap(l.Spans[i-1], l.Spans[i])
				// A list marker keeps its text across a tab of up to an inch.
				// A drawn tab stays on its line (a tab-aligned label and value).
				if g <= math.Max(18, l.Spans[i].Size*2) || i == 1 && isMarker(l.Spans[0].Text) && g <= l.Spans[i].Size*6 || l.Spans[i].Tab && g <= l.Spans[i].Size*12 {
					continue
				}
			}
			ss := leaders(l.Spans[start:i])
			first, last := ss[0], ss[len(ss)-1]
			blocks = append(blocks, Line{Spans: ss, X: first.X, Y: l.Y, Baseline: l.Baseline, Width: last.X + last.Width - first.X, Height: l.Height})
			start = i
		}
	}
	return blocks
}

// underlines marks the spans a thin horizontal rule sits just under, and
// reports whether the rule was one: Word draws it as run underlining.
func underlines(spans []Span, s Segment) bool {
	if s.Y2-s.Y1 > 1.5 || s.X2-s.X1 < 2 {
		return false
	}
	var under []int
	lo, hi := math.Inf(1), math.Inf(-1)
	for i := range spans {
		t := &spans[i]
		below := s.Y1 - t.Baseline
		if below < -0.5 || below > 0.3*t.Size+1 || strings.TrimSpace(t.Text) == "" {
			continue
		}
		if s.X1 <= t.X+1 && s.X2 >= t.X+t.Width-1 {
			under = append(under, i)
			lo, hi = math.Min(lo, t.X), math.Max(hi, t.X+t.Width)
		}
	}
	// An underline ends with its text; a rule running well past it is a
	// border under a heading, kept as art.
	if len(under) == 0 || s.X1 < lo-4 || s.X2 > hi+math.Max(4, spans[under[0]].Size) {
		return false
	}
	for _, i := range under {
		spans[i].Underline = true
	}
	return true
}

func cluster(values []float64) []float64 {
	sort.Float64s(values)
	var out []float64
	for _, v := range values {
		if len(out) == 0 || v-out[len(out)-1] > 2.0 {
			out = append(out, v)
		}
	}
	return out
}
func norm(s Segment) Segment {
	if s.X1 > s.X2 {
		s.X1, s.X2 = s.X2, s.X1
	}
	if s.Y1 > s.Y2 {
		s.Y1, s.Y2 = s.Y2, s.Y1
	}
	return s
}
func touches(a, b Segment) bool {
	return a.X1 <= b.X2+1.5 && b.X1 <= a.X2+1.5 && a.Y1 <= b.Y2+1.5 && b.Y1 <= a.Y2+1.5
}

// maxBorders bounds the cell search; past it a page's rules (sheet music
// staves, hatching) are not a table.
const maxBorders = 2500

// borders are the rules that can bound a table cell: long horizontals and
// tall verticals, less underlines.
func borders(p Page) []Segment {
	var walls []Segment
	for _, s := range p.Segments {
		if s = norm(s); s.Y2-s.Y1 > 10 && s.X2-s.X1 < 1 {
			walls = append(walls, s)
		}
	}
	var segments []Segment
	for _, s := range p.Segments {
		s = norm(s)
		// A rule that meets a wall is a cell border, never an underline.
		if !slices.ContainsFunc(walls, func(w Segment) bool { return touches(s, w) }) && underlines(p.Spans, s) {
			continue
		}
		if (s.X2-s.X1 > 25 && s.Y2-s.Y1 < 1) || (s.Y2-s.Y1 > 10 && s.X2-s.X1 < 1) {
			segments = append(segments, s)
		}
	}
	return segments
}

// Connected ruled cells become real Word tables. Missing vertical walls become
// gridSpan; missing horizontal walls become vMerge. Unruled text stays text.
func layout(p Page) (Layout, error) {
	if err := p.validate(); err != nil {
		return Layout{}, err
	}
	p.Spans = append([]Span(nil), p.Spans...)
	segments := borders(p)
	if len(segments) > maxBorders { // sanitize already dropped them
		return Layout{}, fmt.Errorf("too many table borders")
	}
	parent := make([]int, len(segments))
	for i := range parent {
		parent[i] = i
	}
	root := func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	for i, a := range segments {
		for j := 0; j < i; j++ {
			if touches(a, segments[j]) {
				parent[root(i)] = root(j)
			}
		}
	}
	groups := map[int][]Segment{}
	var order []int
	for i, s := range segments {
		k := root(i)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], s)
	}
	var result Layout
	assigned := make([]bool, len(p.Spans))
grids:
	for _, key := range order {
		g := groups[key]
		var xx, yy []float64
		for _, s := range g {
			if s.X2-s.X1 < 1 {
				xx = append(xx, s.X1)
			}
			if s.Y2-s.Y1 < 1 {
				yy = append(yy, s.Y1)
			}
		}
		xs, ys := cluster(xx), cluster(yy)
		var openLeft, openRight bool
		if len(xs) >= 2 && len(ys) >= 3 {
			xs, openLeft, openRight = openSides(g, xs, ys)
		}
		// One column is a table when rules divide it into rows (a schedule
		// boxed with only row rules) across a third of the page; a lone box
		// is a frame, and a narrow strip of rows a drawing's (a terminal
		// strip in a wiring diagram, which as a table ran onto a new page).
		if len(xs) < 2 || len(ys) < 3 || len(xs) == 2 && (len(ys) < 4 || xs[1]-xs[0] < p.Width/3) {
			continue
		}
		if len(xs) > 64 || len(ys) > 150 {
			continue // too fine to be a Word table; its text stays in the flow
		}
		claimed := append([]bool(nil), assigned...)
		table := Table{X: xs[0], Y: ys[0], XS: xs, YS: ys, OpenLeft: openLeft, OpenRight: openRight}
		wall := func(x, y float64, vertical bool) bool {
			for _, s := range g {
				if vertical && s.X2-s.X1 < 1 && math.Abs(s.X1-x) < 1.5 && s.Y1 <= y+1 && s.Y2 >= y-1 {
					return true
				}
				if !vertical && s.Y2-s.Y1 < 1 && math.Abs(s.Y1-y) < 1.5 && s.X1 <= x+1 && s.X2 >= x-1 {
					return true
				}
			}
			return false
		}
		for r := 0; r < len(ys)-1; r++ {
			var row []Cell
			for c := 0; c < len(xs)-1; {
				end := c + 1
				for end < len(xs)-1 && !wall(xs[end], (ys[r]+ys[r+1])/2, true) {
					end++
				}
				cell := Cell{Start: c, Columns: end - c}
				cell.Continue = r > 0 && !wall((xs[c]+xs[end])/2, ys[r], false)
				cell.Merge = r < len(ys)-2 && !wall((xs[c]+xs[end])/2, ys[r+1], false)
				for i, s := range p.Spans {
					cx, cy := s.X+s.Width/2, s.Y+s.Height/2
					// Half-open bounds give a span center exactly one cell.
					// Padding these bounds lets earlier cells steal border text.
					if !claimed[i] && cx >= xs[c] && cx < xs[end] && cy >= ys[r] && cy < ys[r+1] {
						cell.Spans = append(cell.Spans, s)
						claimed[i] = true
					}
				}
				if cell.Continue {
					// A merged cell owns all its text at the top-left anchor,
					// even when a source line falls in a later physical row.
					for above := r - 1; above >= 0; above-- {
						anchor := -1
						for j := range table.Rows[above] {
							if table.Rows[above][j].Start == c && table.Rows[above][j].Columns == end-c {
								anchor = j
								break
							}
						}
						if anchor < 0 || !table.Rows[above][anchor].Merge {
							continue grids // merges Word cannot express: keep the text as flow
						}
						a := &table.Rows[above][anchor]
						if !a.Continue {
							a.Spans = append(a.Spans, cell.Spans...)
							cell.Spans = nil
							break
						}
					}
				}
				row = append(row, cell)
				c = end
			}
			table.Rows = append(table.Rows, row)
		}
		result.Tables = append(result.Tables, table)
		assigned = claimed
	}
	var free []Span
	for i, s := range p.Spans {
		if !assigned[i] {
			free = append(free, s)
		}
	}
	result.Items = bands(blocks(lines(free), result.Tables), result.Tables)
	result.Images = p.Images
	return result, nil
}

var (
	leaderDots = regexp.MustCompile(`(?:\s*[.·…]){4,}\s*$`)
	pageRef    = regexp.MustCompile(`(?i)^\s*(?:[0-9]{1,4}|[ivxlcdm]{1,7}|[A-Z]-?[0-9]{1,3})\s*$`)
)

// leaders turns drawn leader dots before a line's closing page number
// ("Background ........ 4") into a right tab with a dot leader at the
// number's right edge: Word fills the room exactly, where drawn dots in
// Word's metrics run a little long and wrap the number onto a new line.
func leaders(spans []Span) []Span {
	n := len(spans)
	if n < 2 || !pageRef.MatchString(spans[n-1].Text) {
		return spans
	}
	dots := spans[n-2]
	loc := leaderDots.FindStringIndex(dots.Text)
	if loc == nil {
		return spans
	}
	out := slices.Clone(spans[:n-2])
	if head := strings.TrimRight(dots.Text[:loc[0]], " "); head != "" {
		// The title keeps its share of the span's width, by Word's metrics.
		full, kept := emsIn(dots, dots.Text), emsIn(dots, head)
		dots.Text, dots.Width, dots.Ink = head, dots.Width*kept/max(full, 1e-9), 0
		out = append(out, dots)
	}
	if len(out) == 0 {
		return spans // no title before the dots (it wrapped above): keep them drawn
	}
	number := spans[n-1]
	number.Leader = true
	return append(out, number)
}

// emsIn is text's width in ems in the family Word lays s out in, or its rune
// count where that family's metrics are unknown.
func emsIn(s Span, text string) float64 {
	advances, known := faceAdvances[face(wordFamily(s), s)]
	ems := 0.0
	for _, r := range text {
		a := uint16(0)
		if known {
			a = advance(advances, r)
		}
		if a > 0 {
			ems += float64(a) / 1000
		} else {
			ems += 0.5
		}
	}
	return ems
}
