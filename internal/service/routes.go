package service

import (
	"net/http"

	"github.com/kpenfound/scribble/internal/api"
)

// Route describes one public endpoint. Routes drives both the HTTP mux and
// the OpenAPI description in docs/openapi.json.
//
// Path is an OpenAPI path template such as /v1/thoughts/{id}. Request and
// Response hold zero values of the JSON body types; Request is nil for an
// endpoint that reads no body. Status is the success status, 200 when zero.
// Stream marks the text/event-stream endpoint, whose Response is one event.
type Route struct {
	Method      string
	Path        string
	Summary     string
	Description string
	Request     any
	Response    any
	Status      int
	Stream      bool

	handle func(*Service, http.ResponseWriter, *http.Request)
}

// Routes lists every public endpoint. The web, tailnet and socket handlers
// all serve exactly these; socket-only controls are not part of this table.
var Routes = []Route{
	{
		Method: http.MethodPost, Path: api.Prefix + "/thoughts", Status: http.StatusAccepted,
		Summary: "Submit a thought",
		Description: `Accepts the capture durably before any processing and answers 202 with the queued job.
Submitting the same id again is idempotent. The body is either application/json
(id, captured_at, text) or multipart/form-data with the fields id and captured_at
and an audio file part whose content type is audio/*, up to 25 MiB, for voice
captures that the service transcribes.`,
		Request: api.SubmitRequest{}, Response: api.Job{}, handle: (*Service).submit,
	},
	{
		Method: http.MethodGet, Path: api.Prefix + "/thoughts/{id}",
		Summary:  "Read one thought",
		Response: api.Job{}, handle: (*Service).thought,
	},
	{
		Method: http.MethodPost, Path: api.Prefix + "/thoughts/{id}/retry", Status: http.StatusAccepted,
		Summary:  "Queue a failed thought again, behind the thoughts already queued",
		Response: api.State{}, handle: (*Service).retry,
	},
	{
		Method: http.MethodPost, Path: api.Prefix + "/thoughts/{id}/undo",
		Summary:     "Undo the most recent write",
		Description: "Refused with conflict unless the thought is the latest written one, was written today and its line is unchanged in the note.",
		Response:    api.State{}, handle: (*Service).undoThought,
	},
	{
		Method: http.MethodGet, Path: api.Prefix + "/status",
		Summary:  "List pending, failed and the latest written thoughts",
		Response: api.StatusResponse{}, handle: (*Service).status,
	},
	{
		Method: http.MethodGet, Path: api.Prefix + "/events", Stream: true,
		Summary:  "Follow status changes",
		Response: api.StatusResponse{}, handle: (*Service).events,
	},
	{
		Method: http.MethodGet, Path: api.Prefix + "/health",
		Summary:  "Report service health, build information and tailnet state",
		Response: api.Health{}, handle: (*Service).health,
	},
}
