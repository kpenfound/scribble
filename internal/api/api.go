// Package api defines the wire types of scribble's local HTTP API: the
// version prefix, the error envelope and its codes, and the request and
// response bodies shared by the service and its clients.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Prefix is the path every public route lives under.
const Prefix = "/v1"

// Code names the kind of failure an error response reports.
type Code string

// Error codes. Each maps to one HTTP status, see Status.
const (
	// Validation: the request is not acceptable as sent.
	Validation Code = "validation"
	// Forbidden: a browser check refused the request.
	Forbidden Code = "forbidden"
	// NotFound: the route or the thought does not exist.
	NotFound Code = "not_found"
	// Conflict: the request is valid but the current state refuses it.
	Conflict Code = "conflict"
	// UnsupportedMedia: the request's content type is not accepted.
	UnsupportedMedia Code = "unsupported_media_type"
	// Unavailable: the service cannot do the work right now.
	Unavailable Code = "unavailable"
	// Internal: an unexpected failure inside the service.
	Internal Code = "internal"
)

// Status is the HTTP status that goes with the code.
func (c Code) Status() int {
	switch c {
	case Validation:
		return http.StatusBadRequest
	case Forbidden:
		return http.StatusForbidden
	case NotFound:
		return http.StatusNotFound
	case Conflict:
		return http.StatusConflict
	case UnsupportedMedia:
		return http.StatusUnsupportedMediaType
	case Unavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// Error is the body of every failed API response, inside ErrorResponse.
type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// ErrorResponse is the envelope {"error":{"code","message"}}.
type ErrorResponse struct {
	Error Error `json:"error"`
}

// Errorf builds an Error.
func Errorf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WriteJSON writes value as an uncacheable JSON response.
func WriteJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// WriteError writes the error envelope with the status that matches code.
func WriteError(w http.ResponseWriter, code Code, message string) {
	WriteJSON(w, code.Status(), ErrorResponse{Error: Error{Code: code, Message: message}})
}

// Job is a captured thought as the API reports it.
type Job struct {
	ID         string `json:"id"`
	CapturedAt string `json:"captured_at"`
	// State is queued, transcribing, routing, written or failed.
	State    string `json:"state"`
	Attempts int    `json:"attempts"`
	// Error is the last failure, for a failed or retrying job.
	Error string `json:"error,omitempty"`
	// Line is the vault line written for the thought.
	Line string `json:"line,omitempty"`
	// Previous is the line an amendment replaced.
	Previous string `json:"previous,omitempty"`
	// Action is new, amend or link, once the thought is written.
	Action    string `json:"action,omitempty"`
	WrittenAt string `json:"written_at,omitempty"`
	// Date is the day of the note the thought was written to.
	Date   string `json:"date,omitempty"`
	Undone bool   `json:"undone"`
	// Sequence orders jobs by their place in the queue.
	Sequence int64 `json:"sequence"`
}

// SubmitRequest is the JSON body of POST /v1/thoughts.
type SubmitRequest struct {
	// ID is a client-chosen UUID; submitting it again is idempotent.
	ID string `json:"id"`
	// CapturedAt is when the thought was captured, in RFC 3339 form.
	CapturedAt string `json:"captured_at"`
	// Text is 1 to 16384 bytes with no NUL; newlines collapse to spaces.
	Text string `json:"text"`
}

// StatusResponse is the body of GET /v1/status and the data of each
// "status" event on /v1/events.
type StatusResponse struct {
	// Jobs holds every job that is not written plus the latest written one.
	Jobs []Job `json:"jobs"`
	// Today is the current day in the configured timezone, YYYY-MM-DD.
	Today string `json:"today"`
}

// State is the body of a successful retry or undo.
type State struct {
	// State is queued after a retry and undone after an undo.
	State string `json:"state"`
}

// Build identifies the binary that serves the API.
type Build struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

// Tailnet listener states.
const (
	TailnetStarting = "starting"
	TailnetRunning  = "running"
	TailnetDegraded = "degraded"
	TailnetDisabled = "disabled"
)

// TailnetStatus is the state of the tailnet listener.
type TailnetStatus struct {
	// State is starting, running, degraded or disabled.
	State string `json:"state"`
	// Reason says why the listener is degraded.
	Reason string `json:"reason,omitempty"`
	// URL is where the app is reachable on the tailnet, once it is running.
	URL string `json:"url,omitempty"`
}

// Health is the body of GET /v1/health.
type Health struct {
	// Status is ok while the service can read its queue. A degraded tailnet
	// does not change it: the service, socket and web listener keep running.
	Status  string        `json:"status"`
	Build   Build         `json:"build"`
	Tailnet TailnetStatus `json:"tailnet"`
}
