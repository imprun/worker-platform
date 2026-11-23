package main

import (
	"encoding/json"
	"os"
	"time"

	"github.com/imprun/worker-platform/internal/apppackage"
)

var version = "v1"
var readyDelay = "0s"

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
		Message *string `json:"message"`
	}
	if json.NewDecoder(os.Stdin).Decode(&request) != nil ||
		json.Unmarshal(request.Input, &input) != nil || input.Message == nil {
		os.Exit(apppackage.InvalidInputExitCode)
	}
	if err := json.NewEncoder(os.Stdout).Encode(struct {
		Message string `json:"message"`
		Version string `json:"version"`
	}{*input.Message, version}); err != nil {
		os.Exit(1)
	}
}
