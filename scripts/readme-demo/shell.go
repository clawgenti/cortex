package main

// shell produces the states for the typed-terminal acts: the install one-liner,
// the installer's own output, and the three stacked mini-terminals.
//
// A shell act is ONE state, not one per keystroke. A terminal accumulates: text
// typed or printed stays on screen, so the whole act is a single screen whose rows
// appear at staggered offsets. That is also what keeps the SVG small — the screen
// is written once and revealed, rather than duplicated per frame.

import (
	"fmt"
	"time"
)

// Typing and printing cadences.
//
// AX jitters each keystroke between 35 and 80ms, which reads well but would make
// every regeneration produce a different file and defeat the staleness check. A
// fixed rate is deterministic and, at this speed, indistinguishable.
const (
	typeRate     = 42 * time.Millisecond
	afterCommand = 350 * time.Millisecond
	outputRate   = 120 * time.Millisecond
)

// ShellStates renders a typed terminal act.
func ShellStates(a Act, start time.Duration, g Grid) ([]State, error) {
	var (
		rows []Row
		at   time.Duration
	)
	for _, step := range a.Steps {
		if len(step.Slow) > len(step.Out) {
			return nil, fmt.Errorf("act %q: slow has %d entries but out has %d; the extra delays would do nothing",
				a.Name, len(step.Slow), len(step.Out))
		}
		if step.Cmd != "" {
			dur := time.Duration(len([]rune(step.Cmd))) * typeRate
			rows = append(rows, Row{
				Runs: []Run{
					{Text: "$ ", FG: promptFG, Bold: true},
					{Text: step.Cmd, FG: defaultFG},
				},
				Reveal: Reveal{Kind: "type", At: at, Dur: dur},
			})
			at += dur + afterCommand
		}
		if step.Bar != nil && (*step.Bar < 0 || *step.Bar >= len(step.Out)) {
			return nil, fmt.Errorf("act %q: bar is line %d but out has %d lines", a.Name, *step.Bar, len(step.Out))
		}
		for i, line := range step.Out {
			delay := outputRate
			if i < len(step.Slow) && step.Slow[i] > 0 {
				delay = step.Slow[i]
			}
			runs := []Run{{Text: line, FG: outputColor(line)}}
			if step.ANSI {
				runs = DecodeLine(line)
			}
			reveal := Reveal{Kind: "fill", At: at + delay}
			if step.Bar != nil && *step.Bar == i {
				// The bar fills while the line's delay runs, rather than appearing
				// at its end: the delay is the download.
				from, to, ok := barCells(PlainText(runs))
				if !ok {
					return nil, fmt.Errorf("act %q: bar line %d has no run of ━ to fill", a.Name, i)
				}
				reveal = Reveal{Kind: "bar", At: at, Dur: delay, From: from, To: to}
			}
			at += delay
			rows = append(rows, Row{Runs: runs, Reveal: reveal, SolidBlocks: step.ANSI})
		}
	}
	if len(rows) > g.Rows {
		return nil, fmt.Errorf("act %q: %d rows exceeds the %d-row grid; shorten the script",
			a.Name, len(rows), g.Rows)
	}
	if at > a.Runtime {
		return nil, fmt.Errorf("act %q: content needs %s but the act is %s; lengthen the act or cut lines",
			a.Name, at.Round(time.Millisecond), a.Runtime)
	}
	for len(rows) < g.Rows {
		rows = append(rows, Row{})
	}
	return []State{{Label: a.Name, Rows: rows, At: start, Dur: a.Runtime}}, nil
}

// barCells is the columns of a progress bar's first run of ━: from its first cell
// to just past its last.
func barCells(line string) (from, to int, ok bool) {
	for i, r := range []rune(line) {
		switch {
		case r == '━' && !ok:
			from, ok = i, true
		case r != '━' && ok:
			return from, i, true
		}
		to = i + 1
	}
	return from, to, ok
}

// outputColor gives installer output a little shape without asking the script to
// carry markup: prompts and confirmations read as the moments they are.
func outputColor(line string) string {
	switch {
	case hasPrefix(line, "  "):
		return dimFG
	case contains(line, "[y/N]"):
		return "#d29922"
	case hasPrefix(line, "Enabled") || hasPrefix(line, "Installed"):
		return promptFG
	default:
		return defaultFG
	}
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// WindowStates renders the stacked mini-terminals of act 3.
//
// Each window is drawn as text — box-drawing characters in a dim colour — rather
// than as SVG rectangles, so the chrome lives on the same grid as everything else
// and cannot drift out of alignment with the rows inside it.
func WindowStates(a Act, start time.Duration, g Grid) ([]State, error) {
	var rows []Row
	at := 400 * time.Millisecond
	// Divide the act's runtime between the windows so each lights up in turn.
	perWindow := a.Runtime / time.Duration(max(1, len(a.Windows)))

	var windows []Window
	for wi, w := range a.Windows {
		inner := len(w.Lines)
		height := inner + 2 // top chrome + content + bottom rule
		top := len(rows)
		windowAt := time.Duration(wi) * perWindow

		title := fmt.Sprintf("┌─ %s ", w.Title)
		for len([]rune(title)) < g.Cols-1 {
			title += "─"
		}
		title += "┐"
		rows = append(rows, Row{
			Runs:   []Run{{Text: title, FG: chromeFG}},
			Reveal: Reveal{Kind: "fill", At: windowAt},
		})

		for li, line := range w.Lines {
			fg := defaultFG
			runs := []Run{}
			if hasPrefix(line, "$ ") {
				runs = append(runs,
					Run{Text: "│ ", FG: chromeFG},
					Run{Text: "$ ", FG: promptFG, Bold: true},
					Run{Text: line[2:], FG: fg})
			} else {
				runs = append(runs,
					Run{Text: "│ ", FG: chromeFG},
					Run{Text: line, FG: dimFG})
			}
			rows = append(rows, Row{
				Runs:   runs,
				Reveal: Reveal{Kind: "fill", At: windowAt + at + time.Duration(li)*320*time.Millisecond},
			})
		}

		bottom := "└"
		for len([]rune(bottom)) < g.Cols-1 {
			bottom += "─"
		}
		bottom += "┘"
		rows = append(rows, Row{
			Runs:   []Run{{Text: bottom, FG: chromeFG}},
			Reveal: Reveal{Kind: "fill", At: windowAt + 200*time.Millisecond},
		})

		windows = append(windows, Window{
			Title: w.Title, TopRow: top, Height: height, ActiveAt: windowAt,
		})
		rows = append(rows, Row{}) // spacer between windows
	}
	if len(rows) > g.Rows {
		return nil, fmt.Errorf("act %q: %d rows of windows exceeds the %d-row grid",
			a.Name, len(rows), g.Rows)
	}
	for len(rows) < g.Rows {
		rows = append(rows, Row{})
	}
	return []State{{Label: a.Name, Rows: rows, Windows: windows, At: start, Dur: a.Runtime}}, nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
