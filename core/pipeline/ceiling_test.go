package pipeline

import "testing"

func TestWithinCeiling(t *testing.T) {
	ceiling := PluginCapabilities{
		ReadsBody:          true,
		WritesRequestBody:  true,
		WritesResponseBody: true,
		Requires:           []string{"inference-parser"},
		RequiresAny:        []string{"mcp-parser", "a2a-parser"},
		Description:        "the ceiling",
	}
	cases := []struct {
		name string
		caps PluginCapabilities
		want string
	}{
		{"the ceiling itself", ceiling, ""},
		{"nothing at all", PluginCapabilities{}, ""},
		{"one flag dropped", PluginCapabilities{ReadsBody: true, WritesRequestBody: true}, ""},
		// WritesResponseBody implies ReadsBody, so a narrowed set that keeps only the writer
		// is still inside a ceiling that lists both.
		{"implied flag", PluginCapabilities{WritesResponseBody: true}, ""},
		{"a requirement dropped", PluginCapabilities{RequiresAny: []string{"a2a-parser"}}, ""},
		{"description differs", PluginCapabilities{Description: "configured"}, ""},
		{"flag added", PluginCapabilities{WritesDestination: true}, "WritesDestination"},
		{"requirement added", PluginCapabilities{Requires: []string{"mcp-parser"}}, `Requires "mcp-parser"`},
		{"later requirement added", PluginCapabilities{RequiresLater: []string{"x"}}, `RequiresLater "x"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.caps.WithinCeiling(ceiling); got != tc.want {
				t.Errorf("WithinCeiling = %q, want %q", got, tc.want)
			}
		})
	}
}

// A ceiling that does not list ReadsBody still admits it when it lists a writer: the
// ceiling is normalized too, so it means what Pipeline.New will treat it as.
func TestWithinCeiling_NormalizesTheCeiling(t *testing.T) {
	ceiling := PluginCapabilities{WritesRequestBody: true}
	if got := (PluginCapabilities{ReadsBody: true}).WithinCeiling(ceiling); got != "" {
		t.Errorf("ReadsBody refused under a WritesRequestBody ceiling: %q", got)
	}
}
