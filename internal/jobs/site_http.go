package jobs

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

func (api httpAPI) createSiteSession(w http.ResponseWriter, r *http.Request) {
	worker, ref := r.PathValue("worker"), r.PathValue("ref")
	if !workerNamePattern.MatchString(worker) || !siteRefPattern.MatchString(ref) {
		writeError(w, ErrNotFound)
		return
	}
	update, ok := readSiteUpdate(w, r)
	if !ok {
		return
	}
	publication, err := api.resolve(r.Context(), worker)
	if err != nil {
		writeError(w, err)
		return
	}
	receipt, err := api.store.CreateSiteSession(r.Context(), publication.WorkerUID, ref, update)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, receipt)
}

func (api httpAPI) startReauth(w http.ResponseWriter, r *http.Request) {
	runID, id := r.PathValue("id"), r.PathValue("request")
	if !runIDPattern.MatchString(runID) || !runIDPattern.MatchString(id) {
		writeError(w, ErrNotFound)
		return
	}
	receipt, err := api.store.StartReauth(r.Context(), runID, id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, receipt)
}

func (api httpAPI) completeReauth(w http.ResponseWriter, r *http.Request) {
	runID, id := r.PathValue("id"), r.PathValue("request")
	if !runIDPattern.MatchString(runID) || !runIDPattern.MatchString(id) {
		writeError(w, ErrNotFound)
		return
	}
	update, ok := readSiteUpdate(w, r)
	if !ok {
		return
	}
	receipt, err := api.store.CompleteReauth(r.Context(), runID, id, update)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

func readSiteUpdate(w http.ResponseWriter, r *http.Request) (SiteStateUpdate, bool) {
	var update SiteStateUpdate
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSiteState+1024))
	var sizeErr *http.MaxBytesError
	switch {
	case errors.As(err, &sizeErr):
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "site_session_too_large"})
	case err != nil || !validJSON(body) || json.Unmarshal(body, &update) != nil || !validJSON(update.State):
		writeError(w, ErrInvalidJSON)
	case len(update.State) > maxSiteState:
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "site_session_too_large"})
	case update.ExpiresAt.IsZero():
		writeError(w, ErrInvalidExpiry)
	default:
		return update, true
	}
	return SiteStateUpdate{}, false
}
