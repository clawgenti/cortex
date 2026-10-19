package main

// svg emits the states as one animated SVG.
//
// Two techniques carry the whole file.
//
// ROWS, NOT CHARACTERS, WITH FORCED METRICS. Every run of text is one <tspan>
// carrying textLength and lengthAdjust="spacingAndGlyphs", so the grid holds its
// columns whatever monospace font the reader happens to have. Without it the
// columns shear on any machine whose default differs from the author's, which for
// a table-heavy TUI is the difference between a screenshot and a mess.
//
// ONE ABSOLUTE TIMELINE. Every animation runs the full duration with its own
// keyframe percentages marking when it acts. Nothing is chained off anything else,
// so nothing can drift, and no script is needed — which matters because GitHub
// serves this through camo as an image, where scripts never run but declarative
// CSS animation does.

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// Cell metrics. textLength pins the real advance, so these only have to be
// self-consistent, not matched to any particular font's metrics.
const (
	cellW    = 8.4
	lineH    = 18.0
	fontSize = 14.0
	padX     = 16.0
	padY     = 14.0
	barH     = 30.0
)

type emitter struct {
	w     io.Writer
	total time.Duration
	grid  Grid
	css   []string
}

// pct converts an absolute offset on the timeline into a keyframe percentage.
func (e *emitter) pct(d time.Duration) float64 {
	if e.total <= 0 {
		return 0
	}
	p := float64(d) / float64(e.total) * 100
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	return p
}

// EmitSVG writes the animated SVG.
func EmitSVG(w io.Writer, states []State, total time.Duration, g Grid) error {
	if len(states) == 0 {
		return fmt.Errorf("no states to emit")
	}
	e := &emitter{w: w, total: total, grid: g}

	width := padX*2 + float64(g.Cols)*cellW
	height := barH + padY*2 + float64(g.Rows)*lineH

	var body strings.Builder
	for i, st := range states {
		e.stateCSS(i, st, i == len(states)-1)
		body.WriteString(e.stateBody(i, st))
	}

	fmt.Fprintf(w, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.0f %.0f" width="%.0f" height="%.0f" font-size="%.0f" role="img" aria-label="Cortex: install, point Claude Code at it, run several sessions, then read what they cost in agentop">`+"\n",
		width, height, width, height, fontSize)

	fmt.Fprintf(w, "<style>\n%s</style>\n", e.styleSheet())

	// Window chrome: a rounded panel and a title bar with three dots, so the
	// asset reads as a terminal rather than as text floating on a page.
	fmt.Fprintf(w, `<rect width="%.0f" height="%.0f" rx="8" fill="%s"/>`+"\n", width, height, defaultBG)
	fmt.Fprintf(w, `<path d="M0 8a8 8 0 0 1 8-8h%.0fa8 8 0 0 1 8 8v%.0fH0z" fill="#161b22"/>`+"\n", width-16, barH-8)
	for i, c := range []string{"#ff5f57", "#febc2e", "#28c840"} {
		fmt.Fprintf(w, `<circle cx="%.0f" cy="%.0f" r="5" fill="%s"/>`+"\n", 18+float64(i)*18, barH/2, c)
	}
	fmt.Fprintf(w, `<text x="%.0f" y="%.1f" fill="%s" font-size="12" text-anchor="middle">cortex</text>`+"\n",
		width/2, barH/2+4, chromeFG)

	io.WriteString(w, body.String())
	io.WriteString(w, "</svg>\n")
	return nil
}

func (e *emitter) styleSheet() string {
	var b strings.Builder
	// State groups hide with OPACITY, not visibility. visibility is inheritable
	// but a child may re-declare `visible` and show through a hidden parent — and
	// every row reveal below does exactly that, which had all eight states drawn
	// on top of each other. opacity cannot be escaped by a descendant.
	b.WriteString(`text{font-family:ui-monospace,SFMono-Regular,"SF Mono",Menlo,Consolas,"Liberation Mono",monospace;white-space:pre}
g.st{opacity:0}
.rv{visibility:hidden}
.cur{fill:` + cursorFG + `}
@keyframes blink{0%,49%{opacity:1}50%,100%{opacity:0.15}}
`)
	for _, rule := range e.css {
		b.WriteString(rule)
	}
	// Reduced motion: freeze on the final state rather than on an install log.
	// Same courtesy the reference animation extends, and the only accessible
	// answer for an asset that cannot be paused without script.
	b.WriteString(`@media (prefers-reduced-motion:reduce){
g.st,.rv,.cur,clipPath rect{animation:none!important}
g.st{opacity:0}
g.st.final{opacity:1}
.rv{visibility:visible}
.cur{visibility:hidden}
clipPath rect{width:100000px}
}
`)
	return b.String()
}

// stateCSS writes the visibility keyframes for one state and the reveal keyframes
// for its animated rows.
func (e *emitter) stateCSS(i int, st State, final bool) {
	in, out := e.pct(st.At), e.pct(st.At+st.Dur)
	// Values are pinned at both ends of every range. Visibility is a discrete
	// property, so between two keyframes a browser flips it at the midpoint — the
	// near-duplicate stops confine that flip to a sliver instead of letting it
	// drift a whole state's width.
	var kf strings.Builder
	fmt.Fprintf(&kf, "@keyframes st%d{", i)
	if in > 0 {
		fmt.Fprintf(&kf, "0%%,%.4f%%{opacity:0}", maxf(in-0.001, 0))
	}
	if final {
		fmt.Fprintf(&kf, "%.4f%%,100%%{opacity:1}", in)
	} else {
		fmt.Fprintf(&kf, "%.4f%%,%.4f%%{opacity:1}", in, maxf(out-0.001, in))
		fmt.Fprintf(&kf, "%.4f%%,100%%{opacity:0}", out)
	}
	kf.WriteString("}\n")
	e.css = append(e.css, kf.String())
	e.css = append(e.css, fmt.Sprintf("g.st%d{animation:st%d %s linear infinite}\n", i, i, secs(e.total)))

	for ri, row := range st.Rows {
		switch row.Reveal.Kind {
		case "fill":
			at := e.pct(st.At + row.Reveal.At)
			e.css = append(e.css, fmt.Sprintf(
				"@keyframes rv%d_%d{0%%,%.4f%%{visibility:hidden}%.4f%%,100%%{visibility:visible}}\n",
				i, ri, maxf(at-0.001, 0), at))
			e.css = append(e.css, fmt.Sprintf(".rv%d_%d{animation:rv%d_%d %s linear infinite}\n",
				i, ri, i, ri, secs(e.total)))
		case "type":
			chars := runeLen(row)
			startAt := e.pct(st.At + row.Reveal.At)
			endAt := e.pct(st.At + row.Reveal.At + row.Reveal.Dur)
			full := float64(chars) * cellW
			// steps() applies per keyframe segment, so the reveal advances one
			// character at a time rather than sweeping smoothly across them.
			e.css = append(e.css, fmt.Sprintf(
				"@keyframes tp%d_%d{0%%,%.4f%%{width:0}%.4f%%,100%%{width:%.1fpx}}\n",
				i, ri, startAt, endAt, full))
			e.css = append(e.css, fmt.Sprintf("#cl%d_%d rect{animation:tp%d_%d %s steps(%d,end) infinite}\n",
				i, ri, i, ri, secs(e.total), maxi(chars, 1)))
			e.css = append(e.css, fmt.Sprintf(
				"@keyframes cx%d_%d{0%%,%.4f%%{x:%.1fpx}%.4f%%,100%%{x:%.1fpx}}\n",
				i, ri, startAt, padX, endAt, padX+full))
			e.css = append(e.css, fmt.Sprintf(
				"@keyframes cv%d_%d{0%%,%.4f%%{visibility:hidden}%.4f%%,%.4f%%{visibility:visible}%.4f%%,100%%{visibility:hidden}}\n",
				i, ri, maxf(startAt-0.001, 0), startAt, endAt, minf(endAt+0.6, 100)))
			e.css = append(e.css, fmt.Sprintf(
				".cur%d_%d{animation:cx%d_%d %s steps(%d,end) infinite,cv%d_%d %s linear infinite,blink 1s steps(1,end) infinite}\n",
				i, ri, i, ri, secs(e.total), maxi(chars, 1), i, ri, secs(e.total)))
		case "bar":
			// The clip shows the row up to From, widens a cell at a time to To,
			// then jumps to the whole row: a bar filling, then its figures. No
			// cursor, as install.sh hides it while the bar draws.
			startAt := e.pct(st.At + row.Reveal.At)
			endAt := e.pct(st.At + row.Reveal.At + row.Reveal.Dur)
			e.css = append(e.css, fmt.Sprintf(
				"@keyframes tp%d_%d{0%%,%.4f%%{width:%.1fpx}%.4f%%{width:%.1fpx}%.4f%%,100%%{width:%.1fpx}}\n",
				i, ri, startAt, float64(row.Reveal.From)*cellW, endAt, float64(row.Reveal.To)*cellW,
				minf(endAt+0.001, 100), float64(runeLen(row))*cellW))
			e.css = append(e.css, fmt.Sprintf("#cl%d_%d rect{animation:tp%d_%d %s steps(%d,end) infinite}\n",
				i, ri, i, ri, secs(e.total), maxi(row.Reveal.To-row.Reveal.From, 1)))
		}
	}
}

// stateBody draws one state's rows.
func (e *emitter) stateBody(i int, st State) string {
	var b strings.Builder
	class := fmt.Sprintf("st st%d", i)
	if st.Final {
		class += " final"
	}
	if st.Label != "" {
		class += " " + safeClass(st.Label)
	}
	fmt.Fprintf(&b, `<g class="%s">`+"\n", class)

	for ri, row := range st.Rows {
		if len(row.Runs) == 0 {
			continue
		}
		y := barH + padY + float64(ri+1)*lineH - 4
		rowClass := ""
		switch row.Reveal.Kind {
		case "fill":
			rowClass = fmt.Sprintf(` class="rv rv%d_%d"`, i, ri)
		case "type", "bar":
			rowClass = fmt.Sprintf(` clip-path="url(#cl%d_%d)"`, i, ri)
			fmt.Fprintf(&b, `<clipPath id="cl%d_%d"><rect x="%.1f" y="%.1f" width="0" height="%.1f"/></clipPath>`+"\n",
				i, ri, padX, y-lineH+5, lineH)
		}

		// Background runs first, so text lands on top of its own highlight.
		col := 0
		for _, run := range row.Runs {
			n := len([]rune(run.Text))
			if run.BG != "" {
				fmt.Fprintf(&b, `<rect%s x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s"/>`+"\n",
					rowClassForRect(rowClass), padX+float64(col)*cellW, y-lineH+5,
					float64(n)*cellW, lineH, run.BG)
			}
			col += n
		}
		if row.SolidBlocks {
			e.blockRects(&b, row, y, rowClassForRect(rowClass))
		}

		fmt.Fprintf(&b, `<text%s y="%.1f">`, rowClass, y)
		col = 0
		for _, run := range row.Runs {
			n := len([]rune(run.Text))
			if row.SolidBlocks {
				run.Text = strings.ReplaceAll(run.Text, "█", " ") // drawn by blockRects
			}
			if strings.TrimSpace(run.Text) != "" {
				fill := run.FG
				if fill == "" {
					fill = defaultFG
				}
				weight := ""
				if run.Bold {
					weight = ` font-weight="600"`
				}
				opacity := ""
				if run.Dim {
					opacity = ` opacity="0.65"`
				}
				fmt.Fprintf(&b, `<tspan x="%.1f" textLength="%.1f" lengthAdjust="spacingAndGlyphs" fill="%s"%s%s>%s</tspan>`,
					padX+float64(col)*cellW, float64(n)*cellW, fill, weight, opacity, escapeXML(run.Text))
			}
			col += n
		}
		b.WriteString("</text>\n")

		if row.Reveal.Kind == "type" {
			fmt.Fprintf(&b, `<rect class="cur cur%d_%d" x="%.1f" y="%.1f" width="%.1f" height="%.1f"/>`+"\n",
				i, ri, padX, y-lineH+6, cellW, lineH-4)
		}
	}
	b.WriteString("</g>\n")
	return b.String()
}

// blockRects draws each run of █ in a row as one rect the full size of its cells, in
// the run's colour, so that block art is solid across rows as a terminal draws it.
func (e *emitter) blockRects(b *strings.Builder, row Row, y float64, class string) {
	col := 0
	for _, run := range row.Runs {
		fill := run.FG
		if fill == "" {
			fill = defaultFG
		}
		rs := []rune(run.Text)
		for i := 0; i < len(rs); {
			if rs[i] != '█' {
				i++
				continue
			}
			j := i
			for j < len(rs) && rs[j] == '█' {
				j++
			}
			fmt.Fprintf(b, `<rect%s x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s"/>`+"\n",
				class, padX+float64(col+i)*cellW, y-lineH+5, float64(j-i)*cellW, lineH, fill)
			i = j
		}
		col += len(rs)
	}
}

// rowClassForRect keeps a background rect on the same reveal as its text, but a
// clip-path belongs only on the text element.
func rowClassForRect(rowClass string) string {
	if strings.HasPrefix(rowClass, ` clip-path`) {
		return ""
	}
	return rowClass
}

func runeLen(row Row) int {
	n := 0
	for _, r := range row.Runs {
		n += len([]rune(r.Text))
	}
	return n
}

func secs(d time.Duration) string { return fmt.Sprintf("%gs", d.Seconds()) }

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func minf(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func safeClass(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return "lbl-" + b.String()
}

// escapeXML escapes terminal text for XML. Box-drawing characters and the like
// pass through as UTF-8; only the five markup-significant characters change.
func escapeXML(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&apos;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
