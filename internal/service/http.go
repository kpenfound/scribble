package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/kpenfound/scribble/internal/api"
	"github.com/kpenfound/scribble/internal/buildinfo"
	"github.com/kpenfound/scribble/internal/queue"
	"github.com/kpenfound/scribble/internal/vault"
)

const maxUpload = 25 * 1024 * 1024

// ContentSecurityPolicy is sent with every web and tailnet response.
const ContentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; media-src 'self' blob:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'"

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// TailnetStatuser reports the tailnet listener's state. /v1/health reads it
// on every request.
type TailnetStatuser interface {
	TailnetStatus() api.TailnetStatus
}

// refusal is a failure of a valid request that the current state forbids.
type refusal string

func (r refusal) Error() string { return string(r) }

// Control is a socket-only operation mounted on SocketHandler. Pattern is a
// net/http ServeMux pattern. Controls are not public routes: they stay out of
// Routes and docs/openapi.json, and the web and tailnet handlers never see them.
type Control struct {
	Pattern string
	Handler http.Handler
}

func fail(w http.ResponseWriter, code api.Code, err error) {
	api.WriteError(w, code, err.Error())
}
func invalid(w http.ResponseWriter, format string, args ...any) {
	api.WriteError(w, api.Validation, fmt.Sprintf(format, args...))
}
func notFound(w http.ResponseWriter, _ *http.Request) {
	api.WriteError(w, api.NotFound, "no such route")
}

// mux serves Routes, then controls, and answers every other path under the
// API prefix with not_found.
func (s *Service) mux(controls []Control) *http.ServeMux {
	mux := http.NewServeMux()
	for _, route := range Routes {
		handle := route.handle
		mux.HandleFunc(route.Method+" "+route.Path, func(w http.ResponseWriter, r *http.Request) { handle(s, w, r) })
	}
	for _, c := range controls {
		mux.Handle(c.Pattern, c.Handler)
	}
	mux.HandleFunc(api.Prefix+"/", notFound)
	return mux
}

// SocketHandler serves Routes for the Unix socket listener. The socket is
// reachable only by the account that owns the root, so it applies no browser
// checks. Controls are the socket-only operations mounted next to the public
// routes; anything else answers not_found.
func (s *Service) SocketHandler(controls ...Control) http.Handler {
	mux := s.mux(controls)
	mux.HandleFunc("/", notFound)
	return mux
}

// WebHandler serves Routes and the PWA frontend for a browser-facing
// listener, web or tailnet. hosts lists the Host header values the listener
// serves, each exactly as a browser sends it, such as "127.0.0.1:8090" or
// "scribble.example.ts.net". Any other Host is refused, which closes DNS
// rebinding; an empty list refuses everything. Requests other than GET and
// HEAD that name a foreign Origin or a cross-site Sec-Fetch-Site are refused.
func (s *Service) WebHandler(frontend http.Handler, hosts []string) http.Handler {
	mux := s.mux(nil)
	if frontend == nil {
		frontend = http.HandlerFunc(notFound)
	}
	mux.Handle("/", frontend)
	allowed := map[string]bool{}
	for _, h := range hosts {
		allowed[strings.ToLower(h)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", ContentSecurityPolicy)
		if !allowed[strings.ToLower(r.Host)] {
			api.WriteError(w, api.Forbidden, "unexpected Host header")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host {
					api.WriteError(w, api.Forbidden, "cross-origin writes are not allowed")
					return
				}
			}
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				api.WriteError(w, api.Forbidden, "cross-site writes are not allowed")
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

// LoopbackHosts returns the Host values a browser sends to reach addr, a
// loopback listen address such as 127.0.0.1:8090: the loopback names with
// addr's port.
func LoopbackHosts(addr string) []string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil
	}
	return []string{net.JoinHostPort("127.0.0.1", port), net.JoinHostPort("localhost", port), net.JoinHostPort("::1", port)}
}

func wireJob(j queue.Job) api.Job {
	return api.Job{ID: j.ID, CapturedAt: j.CapturedAt, State: j.State, Attempts: j.Attempts, Error: j.Error, Line: j.Line, Previous: j.Previous, Action: j.Action, WrittenAt: j.WrittenAt, Date: j.Date, Undone: j.Undone, Sequence: j.Sequence}
}
func (s *Service) statusBody() (api.StatusResponse, error) {
	jobs, err := s.Store.Status()
	if err != nil {
		return api.StatusResponse{}, err
	}
	out := api.StatusResponse{Jobs: make([]api.Job, 0, len(jobs)), Today: s.Config.Today(s.Now())}
	for _, j := range jobs {
		out.Jobs = append(out.Jobs, wireJob(j))
	}
	return out, nil
}

func (s *Service) health(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Check(); err != nil {
		fail(w, api.Unavailable, err)
		return
	}
	tailnet := api.TailnetStatus{State: api.TailnetDisabled}
	if s.Tailnet != nil {
		tailnet = s.Tailnet.TailnetStatus()
	}
	api.WriteJSON(w, http.StatusOK, api.Health{
		Status:  "ok",
		Build:   api.Build{Version: buildinfo.Version, Commit: buildinfo.Commit, Date: buildinfo.Date},
		Tailnet: tailnet,
	})
}
func (s *Service) thought(w http.ResponseWriter, r *http.Request) {
	j, err := s.Store.Get(r.PathValue("id"))
	if err != nil {
		fail(w, api.Internal, err)
		return
	}
	if j == nil {
		api.WriteError(w, api.NotFound, "no such thought")
		return
	}
	api.WriteJSON(w, http.StatusOK, wireJob(*j))
}
func (s *Service) retry(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	j, err := s.Store.Get(id)
	if err != nil {
		fail(w, api.Internal, err)
		return
	}
	if j == nil {
		api.WriteError(w, api.NotFound, "no such thought")
		return
	}
	if err = s.Store.Retry(id); err != nil {
		if errors.Is(err, queue.ErrNotFailed) {
			fail(w, api.Conflict, err)
		} else {
			fail(w, api.Internal, err)
		}
		return
	}
	s.Wake()
	api.WriteJSON(w, http.StatusAccepted, api.State{State: "queued"})
}
func (s *Service) undoThought(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	j, err := s.Store.Get(id)
	if err != nil {
		fail(w, api.Internal, err)
		return
	}
	if j == nil {
		api.WriteError(w, api.NotFound, "no such thought")
		return
	}
	if err = s.Undo(r.Context(), id); err != nil {
		var refused refusal
		switch {
		case errors.As(err, &refused), errors.Is(err, vault.ErrConflict):
			fail(w, api.Conflict, err)
		case r.Context().Err() != nil:
			fail(w, api.Unavailable, err)
		default:
			fail(w, api.Internal, err)
		}
		return
	}
	api.WriteJSON(w, http.StatusOK, api.State{State: "undone"})
}
func (s *Service) submit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+(1<<20))
	var j queue.Job
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			fail(w, api.Validation, err)
			return
		}
		defer r.MultipartForm.RemoveAll()
		j.ID, j.CapturedAt = r.FormValue("id"), r.FormValue("captured_at")
		file, header, err := r.FormFile("audio")
		if err != nil {
			fail(w, api.Validation, err)
			return
		}
		defer file.Close()
		j.MIME = header.Header.Get("Content-Type")
		if !strings.HasPrefix(j.MIME, "audio/") {
			invalid(w, "audio content type is required")
			return
		}
		j.Audio, err = io.ReadAll(io.LimitReader(file, maxUpload+1))
		if err != nil || len(j.Audio) == 0 || len(j.Audio) > maxUpload {
			invalid(w, "audio must be between 1 byte and 25 MiB")
			return
		}
	} else {
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			api.WriteError(w, api.UnsupportedMedia, "use application/json or multipart/form-data")
			return
		}
		var input api.SubmitRequest
		decoder := json.NewDecoder(io.LimitReader(r.Body, queue.MaxText+4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			fail(w, api.Validation, err)
			return
		}
		if decoder.Decode(new(any)) != io.EOF {
			invalid(w, "expected one JSON object")
			return
		}
		j.ID, j.CapturedAt, j.Text = input.ID, input.CapturedAt, queue.NormalizeText(input.Text)
		if j.Text == "" || len(j.Text) > queue.MaxText || strings.ContainsRune(j.Text, 0) {
			invalid(w, "text must be between 1 and 16384 bytes and contain no NUL")
			return
		}
	}
	if !uuidRE.MatchString(j.ID) {
		invalid(w, "id must be a UUID")
		return
	}
	captured, err := time.Parse(time.RFC3339Nano, j.CapturedAt)
	if err != nil {
		invalid(w, "captured_at must be an RFC3339 timestamp")
		return
	}
	j.ID = strings.ToLower(j.ID)
	j.CapturedAt = captured.Format(time.RFC3339Nano)
	saved, err := s.Store.Submit(j)
	if err != nil {
		fail(w, api.Internal, err)
		return
	}
	s.Wake()
	api.WriteJSON(w, http.StatusAccepted, wireJob(*saved))
}
func (s *Service) status(w http.ResponseWriter, r *http.Request) {
	body, err := s.statusBody()
	if err != nil {
		fail(w, api.Internal, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, body)
}
func (s *Service) events(w http.ResponseWriter, r *http.Request) {
	if _, ok := w.(http.Flusher); !ok {
		api.WriteError(w, api.Internal, "streaming unavailable")
		return
	}
	ch, unsubscribe := s.subscribe()
	defer unsubscribe()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	send := func() error {
		body, err := s.statusBody()
		if err != nil {
			return err
		}
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err = fmt.Fprintf(w, "event: status\ndata: %s\n\n", b); err != nil {
			return err
		}
		return controller.Flush()
	}
	if err := send(); err != nil {
		return
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			if err := send(); err != nil {
				return
			}
		case <-ticker.C:
			if err := send(); err != nil {
				return
			}
		}
	}
}
