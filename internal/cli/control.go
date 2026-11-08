package cli

import (
	"net/http"

	"github.com/kpenfound/scribble/internal/api"
	"github.com/kpenfound/scribble/internal/config"
	"github.com/kpenfound/scribble/internal/service"
)

// The socket-only controls. They are mounted on the Unix socket handler only:
// they are not in service.Routes or docs/openapi.json, and the web and tailnet
// handlers answer these paths with not_found.
const (
	// ControlPrefix is the path prefix of every control.
	ControlPrefix = api.Prefix + "/control"
	// ControlConfig returns the configuration the service loaded at startup.
	ControlConfig = ControlPrefix + "/config"
	// ControlDoctor runs the doctor checks in the service's environment.
	ControlDoctor = ControlPrefix + "/doctor"
	// ControlShutdown asks the service to shut down as it does on a signal.
	ControlShutdown = ControlPrefix + "/shutdown"
)

// controls returns the socket-only operations for a running service. stop
// feeds the same shutdown path as a signal.
func (a *app) controls(c config.Config, svc *service.Service, stop func()) []service.Control {
	return []service.Control{
		{Pattern: "GET " + ControlConfig, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			// c is the configuration the service started with; the file on
			// disk is never read again.
			api.WriteJSON(w, http.StatusOK, c)
		})},
		{Pattern: "GET " + ControlDoctor, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			api.WriteJSON(w, http.StatusOK, runDoctor(r.Context(), SourceService, c, a.env, svc.Tailnet))
		})},
		{Pattern: "POST " + ControlShutdown, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			api.WriteJSON(w, http.StatusAccepted, api.State{State: "stopping"})
			_ = http.NewResponseController(w).Flush()
			stop()
		})},
	}
}
