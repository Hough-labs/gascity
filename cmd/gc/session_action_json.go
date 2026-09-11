package main

import "io"

type sessionActionResult struct {
	SchemaVersion       string `json:"schema_version"`
	OK                  bool   `json:"ok"`
	Command             string `json:"command"`
	Action              string `json:"action"`
	SessionID           string `json:"session_id,omitempty"`
	State               string `json:"state,omitempty"`
	Mode                string `json:"mode,omitempty"`
	Title               string `json:"title,omitempty"`
	Identity            string `json:"identity,omitempty"`
	Before              string `json:"before,omitempty"`
	Cutoff              string `json:"cutoff,omitempty"`
	Count               *int   `json:"count,omitempty"`
	WaitNudgesWithdrawn int    `json:"wait_nudges_withdrawn,omitempty"`
	Pinned              *bool  `json:"pinned,omitempty"`
	MaterializedNamed   bool   `json:"materialized_named,omitempty"`
	// Confirmed reports whether the command observed the outcome it asked for,
	// rather than merely requesting it. Commands that can only ever request
	// leave it nil; gc session reset sets it explicitly either way.
	Confirmed *bool `json:"confirmed,omitempty"`
}

func sessionJSONRequested(values []bool) bool {
	return len(values) > 0 && values[0]
}

func writeSessionActionJSON(stdout io.Writer, result sessionActionResult) error {
	return writeSessionActionJSONWithOK(stdout, result, true)
}

// writeSessionActionJSONWithOK emits a session action result whose ok field is
// the caller's to decide. A command that reports an outcome it could not
// confirm must not claim ok — see gc session reset, where "ok for a restart
// that never happened" is the defect this exists to make impossible.
func writeSessionActionJSONWithOK(stdout io.Writer, result sessionActionResult, ok bool) error {
	result.SchemaVersion = "1"
	result.OK = ok
	if result.Command == "" && result.Action != "" {
		result.Command = commandName("session", result.Action)
	}
	return writeCLIJSONLine(stdout, result)
}
