package jobs

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/imprun/worker-platform/internal/platform"
)

const (
	queued      = "QUEUED"
	starting    = "STARTING"
	running     = "RUNNING"
	releasing   = "RELEASING"
	waitingAuth = "WAITING_AUTH"
	succeeded   = "SUCCEEDED"
	failed      = "FAILED"

	runLifetime    = 5 * time.Minute
	startupTimeout = 60 * time.Second
	callTimeout    = 90 * time.Second
	leaseLifetime  = 30 * time.Second
	maxAttempts    = 2
	maxPayload     = 64 << 10
	maxSiteState   = 16 << 10
)

var (
	ErrNotFound      = errors.New("run or worker not found")
	ErrNotPublished  = errors.New("worker has no published job revision")
	ErrInputConflict = errors.New("idempotency key was used with a different input")
	ErrInvalidJSON   = errors.New("JSON cannot be stored")
	ErrAuthConflict  = errors.New("reauthentication is unavailable or already in progress")
	ErrAuthDisabled  = errors.New("site sessions are disabled")
	ErrInvalidExpiry = errors.New("site session expiry is in the past")
	errLeaseLost     = errors.New("run lease was lost")
)

type Publication struct {
	WorkerUID string
	Revision  string
}

type Run struct {
	ID            string         `json:"runId"`
	Worker        string         `json:"worker"`
	Revision      string         `json:"revision"`
	State         string         `json:"state"`
	Reason        string         `json:"reason,omitempty"`
	Attempts      []Attempt      `json:"attempts"`
	CreatedAt     time.Time      `json:"createdAt"`
	DeadlineAt    time.Time      `json:"deadlineAt"`
	OccupiedSlots int            `json:"occupiedSlots"`
	Reauth        *ReauthRequest `json:"reauth,omitempty"`
}

type Attempt struct {
	Number             int       `json:"number"`
	State              string    `json:"state"`
	ActorSpace         string    `json:"atespace"`
	Actor              string    `json:"actor"`
	ActorUID           string    `json:"actorUID,omitempty"`
	WorkerID           string    `json:"workerID,omitempty"`
	WorkerPodUID       string    `json:"workerPodUID,omitempty"`
	Reason             string    `json:"reason,omitempty"`
	CreatedAt          time.Time `json:"createdAt"`
	SiteSessionVersion int64     `json:"siteSessionVersion,omitempty"`
}

type claim struct {
	id, leaseID, state string
	number             int
	deadline           time.Time
	input              json.RawMessage
	siteSessionID      string
	revision           platform.JobRevision
}

type outcome struct {
	result        json.RawMessage
	reason        string
	retryable     bool
	reauthVersion int64
}
