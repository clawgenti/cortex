//go:build include_plugin_script && include_plugin_toolprune && include_plugin_inferenceparser && include_plugin_mcpparser && include_plugin_a2aparser

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/listener/forwardproxy"
	"github.com/rossoctl/cortex/core/plugins"
)

// The shipped local config, with the script slot appended, builds whatever hooks
// the dropped-in script declares. The slot sits after tool-prune, a request
// writer, so a script that only reads responses must not count as a request reader.
func TestLocalChain_BuildsWithEachKindOfScript(t *testing.T) {
	for name, handshake := range map[string]string{
		"response reader": `{"cortex":1,"hooks":["response"]}`,
		"response writer": `{"cortex":1,"hooks":["response"],"rewrite_response":true}`,
		"request script":  `{"cortex":1,"hooks":["request"]}`,
		"both hooks":      `{"cortex":1,"hooks":["request","response"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("SHELL", "")
			dir := filepath.Join(home, ".cortex", "plugins")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			body := "#!/bin/sh\necho '" + handshake + "'\ncat >/dev/null\n"
			if err := os.WriteFile(filepath.Join(dir, "watch"), []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			p, err := writeBuiltinConfig(filepath.Join(home, ".cortex"), filepath.Join(home, ".cortex", "ca"))
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(p)
			if err != nil {
				t.Fatal(err)
			}
			config.ApplyPreset(cfg)
			addScriptSlot(cfg, true)
			pipe, err := plugins.BuildWithDeps(cfg.Pipeline.Outbound.Plugins,
				plugins.Deps{Listener: forwardproxy.Support(false), LocalInstall: true})
			if err != nil {
				t.Fatalf("the local chain did not build: %v", err)
			}
			pipe.Stop(t.Context())
		})
	}
}
