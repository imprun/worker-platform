package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"unicode/utf8"

	"github.com/imprun/worker-platform/internal/apppackage"
)

const (
	recordFile   = "requests.jsonl"
	maxEchoBytes = 64 << 10
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--check" {
		if err := record(nil); err != nil {
			os.Exit(1)
		}
		return
	}

	var request apppackage.Invocation
	if err := json.NewDecoder(os.Stdin).Decode(&request); err != nil || !utf8.Valid(request.Input) {
		os.Exit(apppackage.InvalidInputExitCode)
	}
	var payload bytes.Buffer
	if err := json.Compact(&payload, request.Input); err != nil || payload.Len() > maxEchoBytes {
		os.Exit(apppackage.InvalidInputExitCode)
	}
	if err := record(payload.Bytes()); err != nil {
		os.Exit(1)
	}
	if _, err := os.Stdout.Write(payload.Bytes()); err != nil {
		os.Exit(1)
	}
}

func record(payload []byte) error {
	file, err := os.OpenFile(recordFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	if len(payload) != 0 {
		// Append one complete JSON line per invocation.
		_, err = file.Write(append(payload, '\n'))
	}
	return errors.Join(err, file.Close())
}
