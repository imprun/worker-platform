package apppackage

import "encoding/json"

const InvalidInputExitCode = 2

// Invocation is the JSON document passed to the package entrypoint on stdin.
type Invocation struct {
	Input       json.RawMessage `json:"input"`
	RunID       string          `json:"runId,omitempty"`
	Attempt     int             `json:"attempt,omitempty"`
	SiteSession json.RawMessage `json:"siteSession,omitempty"`
}
