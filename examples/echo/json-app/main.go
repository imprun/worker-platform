package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"time"
	"unicode/utf8"

	"github.com/imprun/worker-platform/internal/apppackage"
)

const (
	recordFile   = "requests.jsonl"
	maxEchoBytes = 64 << 10
)

// 검증 패키지마다 시작 확인 시간만 바꾸며 Echo 응답 계약은 동일하다.
var verificationDelay = "0s"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--check" {
		delay, err := time.ParseDuration(verificationDelay)
		if err != nil || delay < 0 {
			os.Exit(1)
		}
		time.Sleep(delay)
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
