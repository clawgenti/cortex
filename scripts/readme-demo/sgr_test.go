package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// Inputs are built with lipgloss itself rather than hand-written escapes, so the
// test tracks the encoder agentop actually renders with instead of a guess about it.
func styled() {
	lipgloss.SetColorProfile(termenv.TrueColor)
	lipgloss.SetHasDarkBackground(true)
}

func TestDecodeLine_TruecolorForeground(t *testing.T) {
	styled()
	in := lipgloss.NewStyle().Foreground(lipgloss.Color("#ff8800")).Render("SESSION")
	runs := DecodeLine(in)
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d: %+v", len(runs), runs)
	}
	if runs[0].Text != "SESSION" || runs[0].FG != "#ff8800" {
		t.Errorf("got %+v", runs[0])
	}
}

func TestDecodeLine_BoldAndBackground(t *testing.T) {
	styled()
	in := lipgloss.NewStyle().Bold(true).Background(lipgloss.Color("#303050")).Render("row")
	runs := DecodeLine(in)
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d: %+v", len(runs), runs)
	}
	if !runs[0].Bold {
		t.Error("bold not decoded")
	}
	if runs[0].BG != "#303050" {
		t.Errorf("background = %q, want #303050", runs[0].BG)
	}
}

func TestDecodeLine_PlainTextIsOneRun(t *testing.T) {
	runs := DecodeLine("no escapes here")
	if len(runs) != 1 || runs[0].FG != "" {
		t.Fatalf("got %+v", runs)
	}
}

func TestDecodeLine_StyledThenPlainSplits(t *testing.T) {
	styled()
	in := lipgloss.NewStyle().Foreground(lipgloss.Color("#3fb950")).Render("$") + " plain"
	runs := DecodeLine(in)
	if len(runs) < 2 {
		t.Fatalf("want at least 2 runs, got %d: %+v", len(runs), runs)
	}
	if runs[0].FG == "" {
		t.Error("first run should carry the colour")
	}
	if got := PlainText(runs); got != "$ plain" {
		t.Errorf("text = %q", got)
	}
}

// TestDecodeLine_TextMatchesStrippedInput is the invariant the whole grid rests
// on: decoding must not add or lose a single column, or every row below shears.
func TestDecodeLine_TextMatchesStrippedInput(t *testing.T) {
	styled()
	for _, in := range []string{
		lipgloss.NewStyle().Foreground(lipgloss.Color("#58a6ff")).Render("agentop · http://x"),
		lipgloss.NewStyle().Bold(true).Render("SESSION") + "   " +
			lipgloss.NewStyle().Faint(true).Render("TITLE"),
		"▕█▎       ▏ gauge glyphs",
		lipgloss.NewStyle().Reverse(true).Render("selected row"),
	} {
		if got, want := PlainText(DecodeLine(in)), ansi.Strip(in); got != want {
			t.Errorf("decode changed the text\n got: %q\nwant: %q", got, want)
		}
	}
}

func TestDecodeLine_ReverseVideoInvertsRatherThanVanishing(t *testing.T) {
	styled()
	runs := DecodeLine(lipgloss.NewStyle().Reverse(true).Render("x"))
	if len(runs) == 0 {
		t.Fatal("no runs")
	}
	if runs[0].FG == "" && runs[0].BG == "" {
		t.Error("reverse video must resolve to a concrete pair, or the run renders default-on-default")
	}
}

func TestDecodeLine_DropsNonSGREscapes(t *testing.T) {
	// A cursor-move sequence must not leak into the SVG as literal text.
	runs := DecodeLine("a\x1b[2Kb")
	if got := PlainText(runs); strings.Contains(got, "[") {
		t.Errorf("escape leaked into text: %q", got)
	}
}

// install.sh draws its bar in a 256-colour index, the one shape lipgloss never
// emits here, so it is tested by hand: an unmapped index would draw the bar in the
// default foreground.
func TestDecodeLine_IndexedColors(t *testing.T) {
	for _, c := range []struct {
		in, want string
	}{
		{"\x1b[38;5;167m━\x1b[0m", "#d75f5f"}, // the cube: install.sh's accent
		{"\x1b[38;5;1m━\x1b[0m", "#ff7b72"},   // a legacy colour, as SGR 31 draws it
		{"\x1b[38;5;244m━\x1b[0m", "#808080"}, // the grey ramp
	} {
		if runs := DecodeLine(c.in); len(runs) != 1 || runs[0].FG != c.want {
			t.Errorf("%q: got %+v, want FG %s", c.in, runs, c.want)
		}
	}
}
