package main

// sgr turns the ANSI that lipgloss renders into styled runs the SVG emitter can
// draw.
//
// The TUI's View() is a string of text interleaved with SGR escape sequences.
// Rendering it means knowing, for every span of characters, what colour it is —
// so this walks the escapes, tracks the current style, and emits one Run per
// contiguous span sharing a style.

import (
	"fmt"
	"strconv"
	"strings"
)

// Run is a span of characters sharing one style.
type Run struct {
	Text string
	FG   string // "#rrggbb", or "" for the terminal default
	BG   string
	Bold bool
	Dim  bool
}

// style is the SGR state while scanning.
type style struct {
	fg, bg    string
	bold, dim bool
	reverse   bool
}

// basicColors maps the 8+8 legacy SGR colours onto a GitHub-dark-ish palette.
// agentop mostly emits truecolor, but a `lipgloss.Color("2")` or a reverse-video
// cell still lands here, and leaving those unmapped renders them as default
// foreground — invisible against their own background.
var basicColors = map[int]string{
	30: "#484f58", 31: "#ff7b72", 32: "#3fb950", 33: "#d29922",
	34: "#58a6ff", 35: "#bc8cff", 36: "#39c5cf", 37: "#b1bac4",
	90: "#6e7681", 91: "#ffa198", 92: "#56d364", 93: "#e3b341",
	94: "#79c0ff", 95: "#d2a8ff", 96: "#56d4dd", 97: "#f0f6fc",
}

// DecodeLine splits one line of ANSI into styled runs.
//
// Runs are merged only when adjacent and identically styled, so the emitter gets
// the smallest number of <tspan>s that still describes the line. The concatenated
// run text always equals the line with its escapes stripped — the invariant that
// keeps the rendered grid aligned with the terminal's own column arithmetic.
func DecodeLine(s string) []Run {
	var (
		runs []Run
		cur  style
		buf  strings.Builder
	)
	flush := func() {
		if buf.Len() == 0 {
			return
		}
		r := Run{Text: buf.String(), Bold: cur.bold, Dim: cur.dim}
		r.FG, r.BG = cur.fg, cur.bg
		if cur.reverse {
			// Reverse video with no explicit pair still has to invert
			// something, or the run renders as default-on-default.
			fg, bg := r.FG, r.BG
			if fg == "" {
				fg = defaultFG
			}
			if bg == "" {
				bg = defaultBG
			}
			r.FG, r.BG = bg, fg
		}
		runs = append(runs, r)
		buf.Reset()
	}

	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			end := strings.IndexByte(s[i:], 'm')
			if end == -1 {
				// Not an SGR sequence; skip the escape so it cannot reach output.
				i += 2
				continue
			}
			flush()
			cur = applySGR(cur, s[i+2:i+end])
			i += end + 1
			continue
		}
		buf.WriteByte(s[i])
		i++
	}
	flush()
	return runs
}

// applySGR folds one parameter list into the running style.
func applySGR(cur style, params string) style {
	if params == "" {
		return style{} // bare ESC[m is a reset
	}
	fields := strings.Split(params, ";")
	for i := 0; i < len(fields); i++ {
		n, err := strconv.Atoi(fields[i])
		if err != nil {
			continue
		}
		switch {
		case n == 0:
			cur = style{}
		case n == 1:
			cur.bold = true
		case n == 2:
			cur.dim = true
		case n == 7:
			cur.reverse = true
		case n == 22:
			cur.bold, cur.dim = false, false
		case n == 27:
			cur.reverse = false
		case n == 39:
			cur.fg = ""
		case n == 49:
			cur.bg = ""
		case n == 38 || n == 48:
			// 38;2;r;g;b truecolor, or 38;5;n indexed.
			if i+1 >= len(fields) {
				return cur
			}
			mode, _ := strconv.Atoi(fields[i+1])
			switch mode {
			case 2:
				if i+4 >= len(fields) {
					return cur
				}
				r, _ := strconv.Atoi(fields[i+2])
				g, _ := strconv.Atoi(fields[i+3])
				b, _ := strconv.Atoi(fields[i+4])
				hex := fmt.Sprintf("#%02x%02x%02x", r, g, b)
				if n == 38 {
					cur.fg = hex
				} else {
					cur.bg = hex
				}
				i += 4
			case 5:
				if i+2 >= len(fields) {
					return cur
				}
				idx, _ := strconv.Atoi(fields[i+2])
				if hex, ok := indexedColor(idx); ok {
					if n == 38 {
						cur.fg = hex
					} else {
						cur.bg = hex
					}
				}
				i += 2
			}
		default:
			if hex, ok := basicColors[n]; ok {
				cur.fg = hex
			}
			if n >= 40 && n <= 47 {
				if hex, ok := basicColors[n-10]; ok {
					cur.bg = hex
				}
			}
			if n >= 100 && n <= 107 {
				if hex, ok := basicColors[n-10]; ok {
					cur.bg = hex
				}
			}
		}
	}
	return cur
}

// indexedColor is the colour of a 38;5;n index: the 16 legacy colours from the same
// palette as their SGR codes, then xterm's 6x6x6 cube and its grey ramp. install.sh
// draws its progress bar in one of these (167), because a shell cannot ask the
// terminal whether truecolor works.
func indexedColor(idx int) (string, bool) {
	switch {
	case idx >= 0 && idx < 8:
		return basicColors[30+idx], true
	case idx >= 8 && idx < 16:
		return basicColors[90+idx-8], true
	case idx >= 16 && idx < 232:
		level := []int{0, 95, 135, 175, 215, 255}
		c := idx - 16
		return fmt.Sprintf("#%02x%02x%02x", level[c/36], level[c/6%6], level[c%6]), true
	case idx >= 232 && idx < 256:
		v := 8 + 10*(idx-232)
		return fmt.Sprintf("#%02x%02x%02x", v, v, v), true
	}
	return "", false
}

// PlainText is the visible text of a decoded line, for assertions and tests.
func PlainText(runs []Run) string {
	var b strings.Builder
	for _, r := range runs {
		b.WriteString(r.Text)
	}
	return b.String()
}
