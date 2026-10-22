package plugins

import (
	"context"
	"slices"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

type capsPlugin struct {
	name string
	caps pipeline.PluginCapabilities
}

func (p capsPlugin) Name() string                              { return p.name }
func (p capsPlugin) Capabilities() pipeline.PluginCapabilities { return p.caps }
func (p capsPlugin) OnRequest(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}
func (p capsPlugin) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// subsets returns every subset of names, the empty one included, each in names' order.
func subsets(names []string) [][]string {
	var out [][]string
	for mask := 0; mask < 1<<len(names); mask++ {
		var s []string
		for i, n := range names {
			if mask&(1<<i) != 0 {
				s = append(s, n)
			}
		}
		out = append(out, s)
	}
	return out
}

// chains returns every ordering of every subset of others, with the plugin under test
// "x" inserted at every position.
func chains(others []string) [][]string {
	var out [][]string
	var permute func(prefix, rest []string)
	permute = func(prefix, rest []string) {
		for i := 0; i <= len(prefix); i++ {
			out = append(out, slices.Concat(prefix[:i], []string{"x"}, prefix[i:]))
		}
		for i, n := range rest {
			permute(append(slices.Clone(prefix), n), slices.Concat(rest[:i], rest[i+1:]))
		}
	}
	permute(nil, others)
	return out
}

func validChain(chain []string, caps pipeline.PluginCapabilities) bool {
	ps := make([]pipeline.Plugin, len(chain))
	for i, n := range chain {
		ps[i] = capsPlugin{name: n}
		if n == "x" {
			ps[i] = capsPlugin{name: n, caps: caps}
		}
	}
	return validateRelationships(ps) == nil
}

// A configured instance WithinCeiling admits must be satisfied by every chain its
// ceiling is satisfied by, or the catalog — and agentop's edit check, which reads it —
// passes a chain the build then refuses.
func TestWithinCeiling_AdmitsNoRelationshipTheCeilingDoesNot(t *testing.T) {
	names := []string{"a", "b", "c"}
	all := chains(names)
	fields := map[string]func([]string) pipeline.PluginCapabilities{
		"Requires":      func(n []string) pipeline.PluginCapabilities { return pipeline.PluginCapabilities{Requires: n} },
		"RequiresAny":   func(n []string) pipeline.PluginCapabilities { return pipeline.PluginCapabilities{RequiresAny: n} },
		"RequiresLater": func(n []string) pipeline.PluginCapabilities { return pipeline.PluginCapabilities{RequiresLater: n} },
	}
	for field, mk := range fields {
		for _, c := range subsets(names) {
			for _, g := range subsets(names) {
				ceiling, configured := mk(c), mk(g)
				if configured.WithinCeiling(ceiling) != "" {
					continue
				}
				for _, chain := range all {
					if validChain(chain, ceiling) && !validChain(chain, configured) {
						t.Errorf("%s: configured %v is within ceiling %v, but chain %v satisfies the ceiling and not it",
							field, g, c, chain)
						break
					}
				}
			}
		}
	}
}
