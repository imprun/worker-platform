package jobs

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"
	"unicode/utf8"

	strictjson "sigs.k8s.io/json"
)

var (
	runIDPattern      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	workerNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
	idempotencyKey    = regexp.MustCompile(`^[!-~]{1,128}$`)
)

type ResolvePublication func(context.Context, string) (Publication, error)

type httpAPI struct {
	store   *Store
	resolve ResolvePublication
}

func Handler(store *Store, resolve ResolvePublication, token string) http.Handler {
	api := httpAPI{store: store, resolve: resolve}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/workers/{worker}/runs", api.submit)
	mux.HandleFunc("GET /v1/runs/{id}", api.status)
	mux.HandleFunc("GET /v1/runs/{id}/result", api.result)
	digest := sha256.Sum256([]byte("Bearer " + token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		presented := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		if subtle.ConstantTimeCompare(presented[:], digest[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (api httpAPI) submit(w http.ResponseWriter, r *http.Request) {
	worker := r.PathValue("worker")
	key := r.Header.Values("Idempotency-Key")
	if !workerNamePattern.MatchString(worker) {
		writeError(w, ErrNotFound)
		return
	}
	if len(key) != 1 || !idempotencyKey.MatchString(key[0]) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_idempotency_key"})
		return
	}
	input, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPayload))
	var sizeErr *http.MaxBytesError
	if errors.As(err, &sizeErr) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "input_too_large"})
		return
	}
	if err != nil || !validJSON(input) {
		writeError(w, ErrInvalidJSON)
		return
	}
	publication, err := api.resolve(r.Context(), worker)
	if err != nil {
		writeError(w, err)
		return
	}
	run, err := api.store.Admit(r.Context(), worker, publication, key[0], input)
	if err != nil {
		writeError(w, err)
		return
	}
	location := "/v1/runs/" + run.ID
	w.Header().Set("Location", location)
	writeJSON(w, http.StatusAccepted, map[string]string{"runId": run.ID, "revision": run.Revision, "statusUrl": location})
}

func (api httpAPI) status(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !runIDPattern.MatchString(id) {
		writeError(w, ErrNotFound)
		return
	}
	run, err := api.store.Get(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (api httpAPI) result(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !runIDPattern.MatchString(id) {
		writeError(w, ErrNotFound)
		return
	}
	state, result, err := api.store.Result(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	if state != succeeded {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "result_unavailable", "state": state})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(result)
}

func validJSON(data []byte) bool {
	var value any
	if !utf8.Valid(data) {
		return false
	}
	violations, err := strictjson.UnmarshalStrict(data, &value, strictjson.DisallowDuplicateFields)
	return err == nil && len(violations) == 0
}

func writeError(w http.ResponseWriter, err error) {
	code, reason := http.StatusServiceUnavailable, "service_unavailable"
	switch {
	case errors.Is(err, ErrNotFound):
		code, reason = http.StatusNotFound, "not_found"
	case errors.Is(err, ErrNotPublished):
		code, reason = http.StatusConflict, "revision_unavailable"
	case errors.Is(err, ErrInputConflict):
		code, reason = http.StatusConflict, "idempotency_conflict"
	case errors.Is(err, ErrInvalidJSON):
		code, reason = http.StatusBadRequest, "invalid_json"
	default:
		// Database errors can include a constraint detail containing input.
		slog.Warn("Job API unavailable", "errorType", fmt.Sprintf("%T", err))
	}
	writeJSON(w, code, map[string]string{"error": reason})
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(value)
}
