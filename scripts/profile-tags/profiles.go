package main

import (
	"fmt"
	"sort"
)

// PluginName is a plugin's build-tag suffix, which may differ from the plugin's
// registered name (e.g. `litellm_budgettrack` vs `litellm-budget-track`).
type PluginName string

// ProfileName names one shipped artifact's plugin set.
type ProfileName string

// The profiles that exist. Kept as an explicit list rather than derived from
// membership below, so a typo in a membership entry is a missing profile the
// guards can report rather than a silently empty tag list.
const (
	ProfileLocal ProfileName = "local"
	ProfileFull  ProfileName = "full"
	ProfileLite  ProfileName = "lite"
	ProfileEnvoy ProfileName = "envoy"
	ProfileCpex  ProfileName = "cpex"
)

var allProfiles = []ProfileName{
	ProfileLocal, ProfileFull, ProfileLite, ProfileEnvoy, ProfileCpex,
}

// membership is the single source of truth for plugin packaging: every plugin has
// exactly one entry listing the profiles that carry it. Keyed by plugin rather
// than by profile so that adding a plugin is one edit in one place, and so that
// "carried by a profile" and "deliberately carried by none" cannot disagree — an
// entry with no profiles IS the optional case, instead of a second list that
// could name the same plugin twice.
//
// Artifact contents:
//
//	local  desktop cortex — the three parsers + tool-prune
//	full   authbridge image, Kubernetes proxy-sidecar, release tarballs,
//	       make dev-install — all fifteen
//	lite   authbridge-lite image — sidecar minimum
//	envoy  authbridge-envoy image
//	cpex   authbridge-cpex image
var membership = map[PluginName][]ProfileName{
	"a2aparser":       {ProfileLocal, ProfileFull, ProfileEnvoy, ProfileCpex},
	"inferenceparser": {ProfileLocal, ProfileFull, ProfileEnvoy, ProfileCpex},
	"mcpparser":       {ProfileLocal, ProfileFull, ProfileEnvoy, ProfileCpex},
	"toolprune":       {ProfileLocal, ProfileFull, ProfileEnvoy},

	"ibac":        {ProfileFull, ProfileEnvoy, ProfileCpex},
	"sparc":       {ProfileFull, ProfileEnvoy, ProfileCpex},
	"tokenbroker": {ProfileFull, ProfileEnvoy, ProfileCpex},
	"lineage":     {ProfileFull, ProfileEnvoy},
	"opa":         {ProfileFull, ProfileEnvoy},

	// Full only. Its redirect needs the forward proxy, so cortex-envoy would refuse
	// it at build time, and only cmd/cortex registers it. Full is what the release
	// tarballs and make dev-install build, so it reaches laptops; it changes no
	// traffic until an agent is routed.
	"inferencerouter": {ProfileFull},

	// The credential and identity plugins lite exists for, plus budget tracking.
	"jwtvalidation": {ProfileFull, ProfileLite, ProfileEnvoy, ProfileCpex},
	"tokenexchange": {ProfileFull, ProfileLite, ProfileEnvoy, ProfileCpex},
	// The route-free alternative to tokenexchange, so it ships wherever that does.
	// Linked is not active: it changes no traffic until a pipeline names it.
	"discoveryexchange":   {ProfileFull, ProfileLite, ProfileEnvoy, ProfileCpex},
	"litellm_budgettrack": {ProfileFull, ProfileLite},
	"staticinject":        {ProfileFull, ProfileLite},

	// Only cortex-cpex carries the cpex plugin; it is registered nowhere else.
	"cpex": {ProfileCpex},

	// No profile: both are heavy and linked only when a caller asks. context-guru
	// pulls bifrost/core, tiktoken-go, tree-sitter grammars and starlark (~16 MiB);
	// session-budget pulls go-redis (~6 MiB).
	"contextguru":   {},
	"sessionbudget": {},
}

// Tags returns the include_plugin_* build tags for a named profile, sorted so the
// output is stable across runs and diffable in CI logs.
func Tags(profile ProfileName) ([]string, error) {
	if !isKnownProfile(profile) {
		return nil, fmt.Errorf("unknown profile %q (known: %v)", profile, known())
	}
	var tags []string
	for plugin, carriedBy := range membership {
		for _, p := range carriedBy {
			if p == profile {
				tags = append(tags, "include_plugin_"+string(plugin))
				break
			}
		}
	}
	// Fail closed on an empty profile: an empty CSV becomes `go build -tags ""`,
	// which under an all-opt-in convention links no plugins at all and would ship
	// a proxy that refuses every config it is handed.
	if len(tags) == 0 {
		return nil, fmt.Errorf("profile %q carries no plugins", profile)
	}
	sort.Strings(tags)
	return tags, nil
}

// isOptional reports whether a plugin deliberately belongs to no profile.
func isOptional(plugin PluginName) bool {
	carriedBy, ok := membership[plugin]
	return ok && len(carriedBy) == 0
}

func isKnownProfile(profile ProfileName) bool {
	for _, p := range allProfiles {
		if p == profile {
			return true
		}
	}
	return false
}

// known lists the defined profile names, for error messages.
func known() []string {
	out := make([]string, 0, len(allProfiles))
	for _, p := range allProfiles {
		out = append(out, string(p))
	}
	sort.Strings(out)
	return out
}
