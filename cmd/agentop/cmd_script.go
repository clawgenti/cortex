package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/cmd/agentop/edit"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/plugins/script/catalog"
)

const scriptUsage = `agentop script — the script plugins Cortex ships
Usage:
  agentop script list [--json] [--config PATH]
  agentop script install <name> [--config PATH]
  agentop script remove <name> [--config PATH]

Each script plugin in this release is a folder of scripts with its own settings.
Installing one copies the folder to ~/.cortex/plugins/<name>/ and adds a script
slot for it to the config, so it runs on every request without a restart.

"list" shows every one and its state: not installed, installed, outdated (an
earlier release's copy), modified (its files were edited), or incomplete (only
the slot or only the folder is there). With Cortex running it also shows how many
of the slot's scripts loaded.

"install" waits for Cortex to load the slot and checks that every script in it
started. If one did not, it takes back what it added and says why. Run again, it
updates an outdated copy and leaves a current one alone. A modified copy is
refused: remove it first.

"remove" takes out the slot, then the files. It refuses a folder holding a file
install did not write.
`

// scriptRecord is the file, inside an installed entry's folder, that says what
// install wrote there. Its leading "." keeps the slot from running it.
const scriptRecord = ".agentop-install.json"

// scriptLoadWait bounds how long install waits, after the reload, for the slot to
// report its scripts. A var so a test can shorten it.
var scriptLoadWait = 10 * time.Second

type installRecord struct {
	Version string            `json:"version"`
	Files   map[string]string `json:"files"` // name -> sha256 hex
}

// scriptState is one of the five states list reports.
type scriptState string

const (
	stateNotInstalled scriptState = "not installed"
	stateInstalled    scriptState = "installed"
	stateOutdated     scriptState = "outdated"
	stateModified     scriptState = "modified"
	stateIncomplete   scriptState = "incomplete"
)

// scriptStatus is one entry as list shows it. Its JSON is what other tools read,
// so the field names are pinned by a test.
type scriptStatus struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	State       scriptState `json:"state"`
	FailClosed  bool        `json:"failClosed"`
	// Loaded and NotStarted are the slot's counts from the running proxy, absent
	// when no proxy answered or it runs no slot for this entry.
	Loaded     *int `json:"loaded,omitempty"`
	NotStarted *int `json:"notStarted,omitempty"`
	// Scripts is how many scripts the entry has: what Loaded should reach.
	Scripts int `json:"scripts"`
}

// scriptTarget is what every action works on.
type scriptTarget struct {
	cfg      *config.Config
	path     string // the config file
	statsURL string // "" when no proxy answers
	apiURL   string // the session API, "" when no proxy answers
	root     string // ~/.cortex/plugins
}

func runScript(args []string, stdout, stderr io.Writer) int {
	action := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action, args = args[0], args[1:]
	}
	switch action {
	case "", "help":
		fmt.Fprint(stdout, scriptUsage)
		if action == "" {
			return 2
		}
		return 0
	case "list":
		return scriptList(args, stdout, stderr)
	case "install":
		return scriptInstall(args, stdout, stderr)
	case "remove":
		return scriptRemove(args, stdout, stderr)
	}
	fmt.Fprintf(stderr, "agentop script: unknown action %q\n\n", action)
	fmt.Fprint(stderr, scriptUsage)
	return 2
}

// scriptFlags parses an action's flags around at most one positional name.
func scriptFlags(name string, args []string, stdout, stderr io.Writer, wantName bool) (cfgPath, entry string, asJSON bool, code int, ok bool) {
	flags := flag.NewFlagSet("script "+name, flag.ContinueOnError)
	p := flags.String("config", "", "Cortex config file (default ~/.cortex/config.yaml)")
	j := new(bool)
	if name == "list" {
		j = flags.Bool("json", false, "print JSON for another program to read")
	}
	pos, code, ok := serverFlags(flags, args, stdout, stderr)
	if !ok {
		return "", "", false, code, false
	}
	if wantName != (len(pos) == 1) || len(pos) > 1 {
		if wantName {
			fmt.Fprintf(stderr, "agentop script %s: want one name: agentop script %s <name>\n", name, name)
		} else {
			fmt.Fprintf(stderr, "agentop script %s: takes no arguments\n", name)
		}
		return "", "", false, 2, false
	}
	if wantName {
		entry = pos[0]
	}
	return *p, entry, *j, 0, true
}

func loadScriptTarget(cfgPath string) (scriptTarget, error) {
	cfg, path, statsURL, err := serverTarget(cfgPath)
	if err != nil {
		return scriptTarget{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return scriptTarget{}, err
	}
	t := scriptTarget{cfg: cfg, path: path, statsURL: statsURL, root: filepath.Join(home, ".cortex", "plugins")}
	if statsURL != "" {
		t.apiURL = dialURL(cfg.Listener.SessionAPIAddr)
	}
	return t, nil
}

// findCatalogEntry is the catalog entry named name, or an error listing the names.
func findCatalogEntry(name string) (catalog.CatalogEntry, error) {
	entries, err := catalog.Catalog()
	if err != nil {
		return catalog.CatalogEntry{}, err
	}
	var names []string
	for _, e := range entries {
		if e.Name == name {
			return e, nil
		}
		names = append(names, e.Name)
	}
	return catalog.CatalogEntry{}, fmt.Errorf("no script plugin named %q; this release has: %s", name, strings.Join(names, ", "))
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// hasSlot reports whether the outbound chain has a script entry for dir, under
// any on_error.
func hasSlot(cfg *config.Config, dir string) bool {
	return slices.ContainsFunc(cfg.Pipeline.Outbound.Plugins, func(e config.PluginEntry) bool {
		return e.Name == scriptPlugin && entryDir(e) == dir
	})
}

const scriptPlugin = "script"

func entryDir(e config.PluginEntry) string {
	var c struct {
		Dir string `json:"dir"`
	}
	if len(e.Config) > 0 {
		_ = json.Unmarshal(e.Config, &c)
	}
	return c.Dir
}

func readRecord(dir string) (installRecord, bool) {
	var r installRecord
	b, err := os.ReadFile(filepath.Join(dir, scriptRecord))
	if err != nil || json.Unmarshal(b, &r) != nil {
		return installRecord{}, false
	}
	return r, true
}

// stateOf is e's state on this machine. The record says what install wrote: files
// matching it but not this release are outdated, files differing from it modified.
func stateOf(t scriptTarget, e catalog.CatalogEntry) scriptState {
	dir := filepath.Join(t.root, e.Name)
	_, statErr := os.Stat(dir)
	folder := statErr == nil
	slot := hasSlot(t.cfg, e.Name)
	switch {
	case !folder && !slot:
		return stateNotInstalled
	case !folder || !slot:
		return stateIncomplete
	}
	rec, ok := readRecord(dir)
	if !ok {
		return stateModified
	}
	current := len(rec.Files) == len(e.Files)
	for name, want := range rec.Files {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || sum(b) != want {
			return stateModified
		}
		if src, ok := e.Files[name]; !ok || sum(src) != want {
			current = false
		}
	}
	if current {
		return stateInstalled
	}
	return stateOutdated
}

// slotCounts is the running proxy's scripts loaded / scripts not started for the
// slot whose dir is name. ok is false when no proxy answered or it runs no such slot.
func slotCounts(ctx context.Context, apiURL, name string) (loaded, notStarted int, ok bool) {
	if apiURL == "" {
		return 0, 0, false
	}
	view, err := apiclient.New(apiURL).GetPipeline(ctx)
	if err != nil {
		return 0, 0, false
	}
	for _, p := range view.Outbound {
		if p.Name != scriptPlugin || len(p.Config) == 0 {
			continue
		}
		var c struct {
			Dir string `json:"dir"`
		}
		if json.Unmarshal(p.Config, &c) != nil || c.Dir != name {
			continue
		}
		for _, m := range p.Metrics {
			switch m.Name {
			case "scripts loaded":
				loaded = int(m.Value)
			case "scripts not started":
				notStarted = int(m.Value)
			}
		}
		return loaded, notStarted, true
	}
	return 0, 0, false
}

func scriptList(args []string, stdout, stderr io.Writer) int {
	cfgPath, _, asJSON, code, ok := scriptFlags("list", args, stdout, stderr, false)
	if !ok {
		return code
	}
	t, err := loadScriptTarget(cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agentop script list: %v\n", err)
		return 1
	}
	entries, err := catalog.Catalog()
	if err != nil {
		fmt.Fprintf(stderr, "agentop script list: %v\n", err)
		return 1
	}
	rows := make([]scriptStatus, 0, len(entries))
	for _, e := range entries {
		r := scriptStatus{Name: e.Name, Description: e.Description, State: stateOf(t, e), FailClosed: e.FailClosed, Scripts: len(e.Scripts())}
		if l, n, ok := slotCounts(context.Background(), t.apiURL, e.Name); ok {
			r.Loaded, r.NotStarted = &l, &n
		}
		rows = append(rows, r)
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"scripts": rows})
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATE\tLOADED\tDESCRIPTION")
	for _, r := range rows {
		loaded := ""
		if r.Loaded != nil {
			loaded = fmt.Sprintf("%d/%d", *r.Loaded, r.Scripts)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Name, r.State, loaded, r.Description)
	}
	_ = tw.Flush()
	return 0
}

// slotChange adds or removes e's slot.
func slotChange(e catalog.CatalogEntry, remove bool) edit.ConfigChange {
	ch := edit.ConfigChange{Chain: "outbound", Plugin: scriptPlugin, Match: map[string]string{"dir": e.Name}}
	if remove {
		ch.RemoveEntry = true
		return ch
	}
	ch.CreatePlugin = true
	ch.Path, ch.Value = []string{"fail_closed"}, edit.BoolValue(e.FailClosed)
	return ch
}

func slotChanges(e catalog.CatalogEntry) []edit.ConfigChange {
	out := []edit.ConfigChange{slotChange(e, false)}
	if e.Timeout != "" {
		out = append(out, edit.ConfigChange{Chain: "outbound", Plugin: scriptPlugin, Match: map[string]string{"dir": e.Name},
			Path: []string{"timeout"}, Value: edit.ScalarValue(e.Timeout)})
	}
	return out
}

// writeFiles puts e's files in dir, each through a temp file and a rename so the
// reloader never sees half a script, then the record.
func writeFiles(dir string, e catalog.CatalogEntry) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	rec := installRecord{Version: version, Files: map[string]string{}}
	names := make([]string, 0, len(e.Files))
	for n := range e.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	scripts := e.Scripts()
	for _, n := range names {
		mode := os.FileMode(0o600)
		if slices.Contains(scripts, n) {
			mode = 0o700
		}
		if err := writeAtomic(filepath.Join(dir, n), e.Files[n], mode); err != nil {
			return err
		}
		rec.Files[n] = sum(e.Files[n])
	}
	b, _ := json.MarshalIndent(rec, "", "  ")
	return writeAtomic(filepath.Join(dir, scriptRecord), append(b, '\n'), 0o600)
}

// writeAtomic writes b to path through a temp file whose name a slot ignores.
func writeAtomic(path string, b []byte, mode os.FileMode) error {
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// removeFiles deletes what rec lists, the record, then dir when it is empty.
func removeFiles(dir string, rec installRecord) error {
	for n := range rec.Files {
		if err := os.Remove(filepath.Join(dir, n)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(filepath.Join(dir, scriptRecord)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// unrecorded is the files in dir that rec does not list: the user's.
func unrecorded(dir string, rec installRecord) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, d := range ents {
		n := d.Name()
		if n == scriptRecord {
			continue
		}
		if _, ok := rec.Files[n]; !ok {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func scriptInstall(args []string, stdout, stderr io.Writer) int {
	cfgPath, name, _, code, ok := scriptFlags("install", args, stdout, stderr, true)
	if !ok {
		return code
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "agentop script install: "+format+"\n", a...)
		return 1
	}
	e, err := findCatalogEntry(name)
	if err != nil {
		fmt.Fprintf(stderr, "agentop script install: %v\n", err)
		return 2
	}
	t, err := loadScriptTarget(cfgPath)
	if err != nil {
		return fail("%v", err)
	}
	dir := filepath.Join(t.root, e.Name)
	state := stateOf(t, e)
	switch state {
	case stateInstalled:
		fmt.Fprintf(stdout, "%s is already installed.\n", e.Name)
		return 0
	case stateModified:
		return fail("%s holds files that were edited since it was installed; run `agentop script remove %s` first", homeTilde(dir), e.Name)
	}
	_, statErr := os.Stat(dir)
	hadFolder := statErr == nil
	if hadFolder {
		if rec, ok := readRecord(dir); ok {
			if extra := unrecorded(dir, rec); len(extra) > 0 {
				return fail("%s holds files install did not write (%s); move them out first", homeTilde(dir), strings.Join(extra, ", "))
			}
		} else if extra := unrecorded(dir, installRecord{}); len(extra) > 0 {
			return fail("%s already exists and was not written by install (%s); move it out first", homeTilde(dir), strings.Join(extra, ", "))
		}
	}
	hadSlot := hasSlot(t.cfg, e.Name)

	if err := writeFiles(dir, e); err != nil {
		return fail("writing %s: %v", homeTilde(dir), err)
	}
	undoFiles := func() {
		if !hadFolder {
			rec, _ := readRecord(dir)
			_ = removeFiles(dir, rec)
		}
	}

	addedSlot := false
	if !hadSlot {
		res, err := writePluginConfig(context.Background(), edit.ConfigWrite{Path: t.path, StatsURL: t.statsURL, Changes: slotChanges(e)})
		if err != nil {
			undoFiles()
			return fail("%v", err)
		}
		switch res.Outcome {
		case edit.WriteReloadFailed, edit.WriteStatusUnreachable:
			undoFiles()
			return fail("Cortex did not take the slot, so %s was put back as it was:\n  %s", homeTilde(t.path), res.ReloadError)
		case edit.WriteReloadTimedOut:
			fmt.Fprintf(stdout, "Added the slot, but Cortex has not reported the reload yet; check with `agentop script list`.\n")
			return 0
		}
		addedSlot = res.Outcome != edit.WriteUnchanged
		if res.Outcome == edit.WriteNotRunning {
			fmt.Fprintf(stdout, "Installed %s. Cortex is not running; the slot loads when it starts.\n", e.Name)
			return 0
		}
	}

	if t.apiURL != "" {
		want := len(e.Scripts())
		deadline := time.Now().Add(scriptLoadWait)
		var loaded, notStarted int
		var seen bool
		for {
			loaded, notStarted, seen = slotCounts(context.Background(), t.apiURL, e.Name)
			if seen && (loaded == want || notStarted > 0) {
				break
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if !seen || loaded != want || notStarted > 0 {
			why := fmt.Sprintf("%d of %d scripts started", loaded, want)
			if !seen {
				why = "Cortex runs no slot for it"
			}
			if addedSlot {
				if _, err := writePluginConfig(context.Background(), edit.ConfigWrite{Path: t.path, StatsURL: t.statsURL,
					Changes: []edit.ConfigChange{slotChange(e, true)}}); err != nil {
					fmt.Fprintf(stderr, "agentop script install: could not take the slot back out: %v\n", err)
				}
			}
			undoFiles()
			return fail("%s did not load: %s. The Cortex log says why; for a Python script, check that python3 runs.", e.Name, why)
		}
	}
	verb := "Installed"
	if state == stateOutdated {
		verb = "Updated"
	}
	fmt.Fprintf(stdout, "%s %s: %s.\n", verb, e.Name, e.Description)
	return 0
}

func scriptRemove(args []string, stdout, stderr io.Writer) int {
	cfgPath, name, _, code, ok := scriptFlags("remove", args, stdout, stderr, true)
	if !ok {
		return code
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "agentop script remove: "+format+"\n", a...)
		return 1
	}
	e, err := findCatalogEntry(name)
	if err != nil {
		fmt.Fprintf(stderr, "agentop script remove: %v\n", err)
		return 2
	}
	t, err := loadScriptTarget(cfgPath)
	if err != nil {
		return fail("%v", err)
	}
	dir := filepath.Join(t.root, e.Name)
	if stateOf(t, e) == stateNotInstalled {
		fmt.Fprintf(stdout, "%s is not installed.\n", e.Name)
		return 0
	}
	rec, _ := readRecord(dir)
	if extra := unrecorded(dir, rec); len(extra) > 0 {
		return fail("%s holds files install did not write (%s); move them out first", homeTilde(dir), strings.Join(extra, ", "))
	}
	if hasSlot(t.cfg, e.Name) {
		res, err := writePluginConfig(context.Background(), edit.ConfigWrite{Path: t.path, StatsURL: t.statsURL,
			Changes: []edit.ConfigChange{slotChange(e, true)}})
		if err != nil {
			return fail("%v", err)
		}
		if res.Outcome == edit.WriteReloadFailed || res.Outcome == edit.WriteStatusUnreachable {
			return fail("Cortex did not take the change, so %s was put back as it was and nothing was removed:\n  %s", homeTilde(t.path), res.ReloadError)
		}
	}
	if err := removeFiles(dir, rec); err != nil {
		return fail("removing %s: %v", homeTilde(dir), err)
	}
	fmt.Fprintf(stdout, "Removed %s.\n", e.Name)
	return 0
}
