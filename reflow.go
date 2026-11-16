package verso

import (
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// Paragraph is a run of source lines that Word may rewrap as one unit, so a
// sentence crossing a line break takes one tracked change, not five.
type Paragraph struct {
	Lines []Line
	// Left and Right bound the text column in page points. Indent is the
	// first line relative to Left; negative is a hanging indent.
	Left, Right, Indent float64
	Align               string // left, center, right, both
	Pitch               float64
}

// Block is a stack of paragraphs sharing one column of the page.
type Block struct {
	Paragraphs          []Paragraph
	X, Y, Right, Bottom float64
}

// Item is one step of a page's vertical flow; exactly one field is set.
type Item struct {
	Block   *Block
	Table   *Table
	Columns []Block  // side by side: written as a borderless layout table
	Tables  []*Table // ruled tables side by side: cells of one borderless row
	Frames  []Line   // last resort: positioned line frames
}

// top is the highest of an item's side-by-side Tables or Columns.
func (it Item) top() float64 {
	y := math.Inf(1)
	for _, t := range it.Tables {
		y = math.Min(y, t.Y)
	}
	for _, c := range it.Columns {
		y = math.Min(y, c.Y)
	}
	return y
}

func (l Line) size() float64 {
	best, count := 0.0, -1
	for _, s := range l.Spans {
		if n := utf8.RuneCountInString(s.Text); n > count {
			best, count = s.Size, n
		}
	}
	return best
}
func (l Line) right() float64 { return l.X + l.Width }

// spaced reports whether the source drew a word gap before span i; kerned
// glyph runs split a word into spans with no gap between them.
func (l Line) spaced(i int) bool {
	prev, s := l.Spans[i-1], l.Spans[i]
	return gap(prev, s) > math.Min(prev.Size, s.Size)*0.15 || strings.HasSuffix(prev.Text, " ") || strings.HasPrefix(s.Text, " ")
}

// needsSpace reports a word gap before span i that the source drew as
// space, not as a space character.
func (l Line) needsSpace(i int) bool {
	prev, s := l.Spans[i-1], l.Spans[i]
	return gap(prev, s) > math.Min(prev.Size, s.Size)*0.15 && !strings.HasSuffix(prev.Text, " ") && !strings.HasPrefix(s.Text, " ")
}

func (l Line) text() string {
	var b strings.Builder
	for i, s := range l.Spans {
		if i > 0 && l.needsSpace(i) {
			b.WriteByte(' ')
		}
		b.WriteString(s.Text)
	}
	return b.String()
}

// The width the line's first word would need on the line above, space
// included. The word may cross span boundaries.
func (l Line) firstWord() float64 {
	if len(l.Spans) == 0 {
		return 0
	}
	start := l.Spans[0].X
	for i, s := range l.Spans {
		if i > 0 && l.spaced(i) {
			return l.Spans[i-1].X + l.Spans[i-1].Width - start + 0.25*s.Size
		}
		// Positions inside a span are its width shared by the ems before
		// them (by character count where no table measures the face): by
		// count, "sit" in a long span measured 16pt for its 13.
		share := func(prefix string) float64 {
			if all := emsIn(s, s.Text); all > 0 {
				return s.Width * emsIn(s, prefix) / all
			}
			return 0
		}
		text := s.Text
		if i == 0 {
			text = strings.TrimLeft(text, " ")
			start += share(s.Text[:len(s.Text)-len(text)])
		}
		if j := strings.IndexByte(text, ' '); j >= 0 {
			end := s.X + share(s.Text[:len(s.Text)-len(text)+j])
			return end - start + 0.25*s.Size
		}
	}
	last := l.Spans[len(l.Spans)-1]
	return last.X + last.Width - start + 0.25*last.Size
}

// stretched reports a line justified by word spacing: every gap after a
// drawn space is wider again, by one amount, across at least three gaps; or
// its spans' spaces are drawn wider than Word's by a justified line's
// stretch (a tenth of an em or more each, over at least three spaces).
func (l Line) stretched() bool {
	var extra []float64
	spaces, stretch := 0, 0.0
	for i, s := range l.Spans {
		if i > 0 && strings.HasSuffix(l.Spans[i-1].Text, " ") {
			extra = append(extra, gap(l.Spans[i-1], s))
		}
		if n := strings.Count(strings.TrimSpace(s.Text), " "); n > 0 {
			if natural, ok := naturalWidth(s); ok {
				spaces, stretch = spaces+n, stretch+s.Width-natural
			}
		}
	}
	if spaces >= 3 && stretch/float64(spaces) >= 0.1*l.size() && stretch/float64(spaces) <= 2*l.size() {
		return true
	}
	if len(extra) < 3 {
		return false
	}
	lo, hi := slices.Min(extra), slices.Max(extra)
	return lo > 0.1*l.size() && hi-lo < 0.05*l.size()
}

// marked reports a line that starts with a list marker and a tab.
func (l Line) marked() bool { _, ok := l.hang(); return ok }

func (l Line) leader() bool {
	return slices.ContainsFunc(l.Spans, func(s Span) bool { return s.Leader })
}

// hang is where a list item's text starts: the span after its marker's tab.
func (l Line) hang() (float64, bool) {
	if len(l.Spans) > 1 && tabbed(l, 1) && isMarker(l.Spans[0].Text) {
		return l.Spans[1].X, true
	}
	return 0, false
}

var sentenceEnd = regexp.MustCompile(`[.:;!?][)"”’\]]*\s*$`)

var listMarker = regexp.MustCompile(`^\s*(?:[\x{2022}\x{25CF}\x{25AA}\x{25E6}\x{2023}\x{2043}\x{F0B7}\x{F0A7}\x{2013}\-\*]|\(?(?:\d{1,3}|[a-zA-Z]|[ivxlcIVXLC]{1,5})[.)](?:\d{1,3}[.)]?)*|\d{1,3}(?:\.\d{1,3})+)\s`)

// blocks groups line pieces into columns. A piece joins the nearest block
// above it that it overlaps horizontally. Two pieces of one row competing for
// the same block (a heading above two columns) both start fresh blocks.
func blocks(pieces []Line, tables []Table) []*Block {
	sort.SliceStable(pieces, func(i, j int) bool {
		if math.Abs(pieces[i].Baseline-pieces[j].Baseline) > 0.5 {
			return pieces[i].Baseline < pieces[j].Baseline
		}
		return pieces[i].X < pieces[j].X
	})
	type open struct {
		lines       []Line
		left, right float64
	}
	var all []*open
	blocked := func(o *open, p Line) bool {
		last := o.lines[len(o.lines)-1]
		for _, t := range tables {
			if t.Y > last.Baseline && t.Y < p.Y && t.X < math.Max(o.right, p.right()) && t.right() > math.Min(o.left, p.X) {
				return true
			}
		}
		return false
	}
	for i := 0; i < len(pieces); {
		j := i
		for j < len(pieces) && math.Abs(pieces[j].Baseline-pieces[i].Baseline) <= 0.5 {
			j++
		}
		row := pieces[i:j]
		choice := make([]*open, len(row))
		claims := map[*open]int{}
		for k, p := range row {
			best, gap := (*open)(nil), math.Inf(1)
			for _, o := range all {
				last := o.lines[len(o.lines)-1]
				dy := p.Baseline - last.Baseline
				limit := 3 * math.Max(p.size(), last.size())
				if dy <= 0.5 || dy > limit || p.X >= o.right || p.right() <= o.left || blocked(o, p) {
					continue
				}
				if dy < gap {
					best, gap = o, dy
				}
			}
			choice[k] = best
			if best != nil {
				claims[best]++
			}
		}
		for k, p := range row {
			o := choice[k]
			if o == nil || claims[o] > 1 {
				all = append(all, &open{lines: []Line{p}, left: p.X, right: p.right()})
				continue
			}
			o.lines = append(o.lines, p)
			o.left, o.right = math.Min(o.left, p.X), math.Max(o.right, p.right())
		}
		i = j
	}
	out := make([]*Block, 0, len(all))
	for _, o := range all {
		b := &Block{X: o.left, Right: o.right, Y: o.lines[0].Y}
		for _, l := range o.lines {
			b.Bottom = math.Max(b.Bottom, l.Y+l.Height)
		}
		b.Paragraphs = paragraphs(o.lines, o.left, o.right)
		out = append(out, b)
	}
	return out
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	v = append([]float64(nil), v...)
	sort.Float64s(v)
	return v[len(v)/2]
}

// itemRight is the right edge of a list item's widest line: its lines so
// far and the lines after them that start at its hanging indent.
func itemRight(current, next []Line, hang float64) float64 {
	r := 0.0
	for _, l := range current {
		r = math.Max(r, l.right())
	}
	for _, l := range next {
		if math.Abs(l.X-hang) > 1.5 {
			break
		}
		r = math.Max(r, l.right())
	}
	return r
}

// paragraphs splits a column's lines where the source broke deliberately.
// The main test is typesetting's own: if the next line's first word would
// have fit at the end of this line, the break was a paragraph end.
func paragraphs(lines []Line, left, right float64) []Paragraph {
	gaps := make([]float64, len(lines)) // gaps[i]: baseline gap above line i
	for i := 1; i < len(lines); i++ {
		gaps[i] = lines[i].Baseline - lines[i-1].Baseline
	}
	// A gap is a paragraph break when it is clearly wider than a gap of a
	// neighbouring line in the same size: one pitch per block or per size
	// fails where dense small print and larger text share a column. One
	// narrower neighbour is enough: around a one-line heading both gaps are
	// breaks.
	wider := func(i int) bool {
		size := lines[i].size()
		tol := math.Max(1.5, 0.35*size)
		neighbours := 0
		for _, j := range []int{i - 1, i + 1} {
			if j < 1 || j >= len(lines) || math.Abs(lines[j].size()-size) > 0.1*size || math.Abs(lines[j-1].size()-size) > 0.1*size {
				continue
			}
			neighbours++
			if gaps[i] > gaps[j]+tol && gaps[i] > 1.05*gaps[j] {
				return true
			}
		}
		return neighbours == 0 && gaps[i] > 1.6*size+tol
	}
	blockPitch := median(gaps[1:])
	// Justification stretches every line but a paragraph's last to the
	// column edge, so in a justified column a short line that ends a sentence
	// after lines that reach the edge ends its paragraph, even when the next
	// word would not have fit ("Article 8" after a sentence ending near the
	// edge). A short line mid-sentence is a manual line break.
	atEdge := func(l Line) bool { return math.Abs(l.right()-right) <= 1.5 }
	edge := 0
	for _, l := range lines[:max(0, len(lines)-1)] {
		if atEdge(l) {
			edge++
		}
	}
	justifiedColumn := edge >= 3 && 3*edge >= len(lines)
	// A line that fills the column mid-sentence wraps: "within thirty" then
	// "(30) days" is the sentence going on, not a list item.
	wrapped := func(l Line) bool { return atEdge(l) && !sentenceEnd.MatchString(l.text()) }
	var out [][]Line
	for i, l := range lines {
		if i == 0 {
			out = append(out, []Line{l})
			continue
		}
		prev := lines[i-1]
		current := out[len(out)-1]
		size := math.Max(l.size(), prev.size())
		tol := math.Max(2, 0.3*size)
		split := wider(i) ||
			math.Abs(l.size()-prev.size()) > 0.15*size ||
			right-prev.right() > l.firstWord()+0.5 ||
			listMarker.MatchString(l.text()) && !wrapped(prev) || l.marked() ||
			prev.leader() || l.leader() || // a contents entry is its own paragraph
			justifiedColumn && len(current) >= 2 && right-prev.right() > tol && sentenceEnd.MatchString(prev.text()) && !slices.ContainsFunc(current[:len(current)-1], func(l Line) bool { return !atEdge(l) })
		// A list item's next line starting where its text starts (after the
		// bullet's tab) is the item continuing, whatever the column's right
		// edge says: list styles often wrap short of the body's margin. The
		// item's own widest line still counts: a form's "(c) Single" then
		// "Married filing jointly" ended short of it, and became one line.
		if hang, ok := current[0].hang(); ok && math.Abs(l.X-hang) <= 1.5 && !listMarker.MatchString(l.text()) && !l.marked() &&
			!wider(i) && math.Abs(l.size()-prev.size()) <= 0.15*size && itemRight(current, lines[i:], hang)-prev.right() <= l.firstWord()+0.5 {
			out[len(out)-1] = append(current, l)
			continue
		}
		if !split && l.X > prev.X+tol {
			split = len(current) > 1 // indent after the first line = new first-line indent
		}
		if !split && l.X < prev.X-tol {
			split = len(current) > 1 // outdent after the first line = new hanging item
		}
		if split {
			out = append(out, []Line{l})
		} else {
			out[len(out)-1] = append(current, l)
		}
	}
	result := make([]Paragraph, 0, len(out))
	justified := false
	for _, ls := range out {
		p := shape(ls, left, right, blockPitch)
		justified = justified || p.Align == "both"
		result = append(result, p)
	}
	// Two lines cannot show justification on their own; in a justified
	// column, a first line that reaches the column edge was justified too.
	for i, p := range result {
		if justified && len(p.Lines) == 2 && p.Align == "left" && atEdge(p.Lines[0]) {
			// Justified lines end at the column edge (as in shape).
			result[i].Align, result[i].Right = "both", math.Max(p.Lines[0].right(), p.Lines[1].right())+0.25
		}
	}
	return result
}

func shape(ls []Line, left, right, blockPitch float64) Paragraph {
	p := Paragraph{Lines: ls, Align: "left"}
	n := len(ls)
	size := ls[0].size()
	if n == 1 {
		p.Pitch = 1.2 * size
		if blockPitch > size {
			p.Pitch = math.Min(blockPitch, 1.5*size)
		}
		p.Pitch = math.Max(p.Pitch, ls[0].Height) // a guess: room for the glyph box
		// One line never needs to wrap; give it room so Word's metrics cannot
		// push its last word down, and keep its x as a plain indent.
		p.Left, p.Right = ls[0].X, math.Max(right, ls[0].right())+2*size
		return p
	}
	p.Pitch = (ls[n-1].Baseline - ls[0].Baseline) / float64(n-1)
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, l := range ls[1:] {
		lo, hi = math.Min(lo, l.X), math.Max(hi, l.X)
	}
	spreadL := hi - lo
	rlo, rhi, maxRight := math.Inf(1), math.Inf(-1), 0.0
	clo, chi := math.Inf(1), math.Inf(-1)
	slack := math.Inf(1)
	for i, l := range ls {
		maxRight = math.Max(maxRight, l.right())
		c := l.X + l.Width/2
		clo, chi = math.Min(clo, c), math.Max(chi, c)
		if i < n-1 {
			rlo, rhi = math.Min(rlo, l.right()), math.Max(rhi, l.right())
		}
	}
	// Room past the widest line, so Word breaks where the PDF broke even when
	// its metrics run a little long, but short of where any line would take
	// the next line's first word: a shorter line has that much more room.
	for i := range n - 1 {
		slack = math.Min(slack, ls[i].right()+ls[i+1].firstWord()-maxRight)
	}
	slack = math.Max(0, math.Min(0.6*slack, size))
	stretched := !slices.ContainsFunc(ls[:n-1], func(l Line) bool { return !l.stretched() })
	switch {
	case (n >= 3 || stretched) && rhi-rlo <= 1.5 && spreadL <= 1.5:
		p.Align = "both"
	case chi-clo <= 2 && (spreadL > 3 || ls[0].X-lo > 3):
		p.Align = "center"
	case rhi-rlo <= 1.5 && spreadL > 3:
		p.Align = "right"
	}
	p.Left, p.Right = lo, maxRight+slack
	p.Indent = ls[0].X - lo
	switch p.Align {
	case "both":
		// Justified lines end near the column edge: room past it lets Word
		// pull up a short word the PDF put on the next line, and too little
		// breaks a line whose Word widths run a rounding error long.
		p.Right = maxRight + 0.25
	case "center":
		half := (maxRight-math.Min(lo, ls[0].X))/2 + slack
		mid := (clo + chi) / 2
		p.Left, p.Right, p.Indent = mid-half, mid+half, 0
	case "right":
		p.Left, p.Indent = math.Min(lo, ls[0].X), 0
	}
	return p
}

type bandPiece struct {
	top, bottom float64
	block       *Block
	table       *Table
}

// bands orders blocks and tables down the page. Blocks whose vertical ranges
// overlap form a band; a band of disjoint columns becomes a layout table.
func bands(bs []*Block, tables []Table) []Item {
	var ps []bandPiece
	for _, b := range bs {
		ps = append(ps, bandPiece{b.Y, b.Bottom, b, nil})
	}
	for i := range tables {
		t := &tables[i]
		ps = append(ps, bandPiece{t.Y, t.bottom(), nil, t})
	}
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].top < ps[j].top })
	var items []Item
	for i := 0; i < len(ps); {
		j, bottom := i+1, ps[i].bottom
		for j < len(ps) && ps[j].top < bottom-1 {
			bottom = math.Max(bottom, ps[j].bottom)
			j++
		}
		items = append(items, band(ps[i:j])...)
		i = j
	}
	return items
}

// apart reports whether tables share no horizontal extent.
func apart(items []Item) bool {
	for i, a := range items {
		for _, b := range items[i+1:] {
			if a.Table.X < b.Table.right() && b.Table.X < a.Table.right() {
				return false
			}
		}
	}
	return true
}

func band(group []bandPiece) []Item {
	if len(group) == 1 {
		if group[0].table != nil {
			return []Item{{Table: group[0].table}}
		}
		return []Item{{Block: group[0].block}}
	}
	var items []Item
	var bs []*Block
	for _, g := range group {
		if g.table != nil {
			items = append(items, Item{Table: g.table})
		} else {
			bs = append(bs, g.block)
		}
	}
	frames := func() []Item {
		var ls []Line
		for _, b := range bs {
			for _, para := range b.Paragraphs {
				ls = append(ls, para.Lines...)
			}
		}
		if len(ls) == 0 {
			return items
		}
		return append(items, Item{Frames: ls})
	}
	// Tables side by side in the flow stack one under the other, and a
	// figure of grids grew the page by their heights: one row holds them.
	if len(items) > 1 && apart(items) {
		ts := make([]*Table, len(items))
		for i, it := range items {
			ts[i] = it.Table
		}
		sort.SliceStable(ts, func(i, j int) bool { return ts[i].X < ts[j].X })
		items = []Item{{Tables: ts}}
	}
	if len(items) > 0 {
		return frames() // text beside a ruled table keeps its source position
	}
	// Columns: blocks overlapping horizontally stack into one column.
	sort.SliceStable(bs, func(i, j int) bool { return bs[i].X < bs[j].X })
	var cols []Block
	for _, b := range bs {
		k := -1
		for c := range cols {
			if b.X < cols[c].Right && b.Right > cols[c].X {
				k = c
			}
		}
		if k < 0 {
			cols = append(cols, *b)
			continue
		}
		c := &cols[k]
		for _, q := range c.Paragraphs {
			last := q.Lines[len(q.Lines)-1]
			if b.Y < last.Y+last.Height-1 && b.Bottom > q.Lines[0].Y+1 {
				return frames()
			}
		}
		c.Paragraphs = append(c.Paragraphs, b.Paragraphs...)
		sort.SliceStable(c.Paragraphs, func(i, j int) bool { return c.Paragraphs[i].Lines[0].Y < c.Paragraphs[j].Lines[0].Y })
		c.X, c.Right = math.Min(c.X, b.X), math.Max(c.Right, b.Right)
		c.Y, c.Bottom = math.Min(c.Y, b.Y), math.Max(c.Bottom, b.Bottom)
	}
	for i := 1; i < len(cols); i++ {
		if cols[i].X < cols[i-1].Right {
			return frames()
		}
	}
	if len(cols) == 1 {
		return []Item{{Block: &cols[0]}}
	}
	return []Item{{Columns: cols}}
}
