//go:build include_plugin_script

package main

import (
	"testing"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/pipeline"
)

func outboundNames(c *config.Config) []string {
	var out []string
	for _, e := range c.Pipeline.Outbound.Plugins {
		out = append(out, e.Name)
	}
	return out
}

func TestAddScriptSlot(t *testing.T) {
	chain := func(entries ...config.PluginEntry) *config.Config {
		c := &config.Config{}
		c.Pipeline.Outbound.Plugins = entries
		return c
	}
	parser := config.PluginEntry{Name: "inference-parser"}

	c := chain(parser)
	addScriptSlot(c, true)
	if got := outboundNames(c); len(got) != 2 || got[1] != "script" {
		t.Errorf("local install: %v, want script appended last", got)
	}

	c = chain(parser)
	addScriptSlot(c, false)
	if got := outboundNames(c); len(got) != 1 {
		t.Errorf("not a local install: %v, want nothing added", got)
	}

	// A chain that names script — even switched off — is the user's arrangement.
	off := config.PluginEntry{Name: "script", OnError: pipeline.ErrorPolicyOff}
	c = chain(off, parser)
	addScriptSlot(c, true)
	if got := outboundNames(c); len(got) != 2 || got[0] != "script" {
		t.Errorf("user's own script entry: %v, want it left alone", got)
	}
}

func TestScriptFolders(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := &config.Config{}
	c.Pipeline.Outbound.Plugins = []config.PluginEntry{
		{Name: "script"},
		{Name: "tool-prune"},
		{Name: "script", Config: []byte(`{"dir":"late"}`)},
		{Name: "script", Config: []byte(`{"dir":"../escape"}`)},
	}
	got := scriptFolders(c)
	if len(got) != 2 {
		t.Fatalf("scriptFolders = %v, want the two valid slots", got)
	}
}

func TestScriptFingerprint_EmptyWithoutScripts(t *testing.T) {
	dir := t.TempDir()
	if fp := scriptFingerprint([]string{dir, dir + "/missing"}); fp != "" {
		t.Errorf("slots without scripts fingerprint as %q; /reload/status would stop being the config's sha256", fp)
	}
}
