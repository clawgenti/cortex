package session

import "strings"

// PendingID is the pending bucket for agent's calls from one process, proc being that
// process as "<pid>.<start>", or the agent-wide bucket when proc is "" — the process
// could not be looked up. It is a recording id only: nothing renames a pending bucket,
// and its calls are linked to the session the process names when the list is read (see
// Link).
func PendingID(agent, proc string) string {
	if proc == "" {
		return PendingSessionID(agent)
	}
	return PendingSessionID(agent) + "@" + proc
}

// IsConversation reports whether id is one conversation's session: false for "", the
// default bucket, and any pending bucket, each of which holds many conversations' calls or
// none. Plugins that key state on a session (the inference-router's pins, cpex's taint)
// skip the others.
func IsConversation(id string) bool {
	return id != "" && id != DefaultSessionID && !strings.HasPrefix(id, PendingPrefix)
}
