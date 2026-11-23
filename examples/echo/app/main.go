package main

import (
	"encoding/json"
	"os"
	"time"

	"github.com/imprun/worker-platform/internal/apppackage"
)

var version = "v1"
var readyDelay = "0s"

const maxWaitSeconds = 75

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--check" {
		delay, err := time.ParseDuration(readyDelay)
		if err != nil || delay < 0 {
			os.Exit(1)
		}
		time.Sleep(delay)
		return
	}
	var request apppackage.Invocation
	var input struct {
		Message     *string         `json:"message"`
		WaitSeconds json.RawMessage `json:"waitSeconds"`
	}
	if json.NewDecoder(os.Stdin).Decode(&request) != nil ||
		json.Unmarshal(request.Input, &input) != nil || input.Message == nil {
		os.Exit(apppackage.InvalidInputExitCode)
	}
	if request.RunID != "" && len(input.WaitSeconds) > 0 {
		var seconds int
		if json.Unmarshal(input.WaitSeconds, &seconds) != nil || seconds < 0 || seconds > maxWaitSeconds {
			os.Exit(apppackage.InvalidInputExitCode)
		}
		// Give the worker-failure example time to interrupt the first Attempt.
		if request.Attempt == 1 {
			time.Sleep(time.Duration(seconds) * time.Second)
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(struct {
		Message string `json:"message"`
		Version string `json:"version"`
		RunID   string `json:"runId,omitempty"`
		Attempt int    `json:"attempt,omitempty"`
	}{*input.Message, version, request.RunID, request.Attempt}); err != nil {
		os.Exit(1)
	}
}
