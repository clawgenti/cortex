package session

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
