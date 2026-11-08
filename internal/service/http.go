package service

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/kpenfound/scribble/internal/queue"
)

const maxUpload = 25 * 1024 * 1024

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func jsonResponse(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func problem(w http.ResponseWriter, status int, err error) {
	jsonResponse(w, status, map[string]string{"error": err.Error()})
}
func (s *Service) Handler(frontend http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Store.Check(); err != nil {
			problem(w, 503, err)
			return
		}
		jsonResponse(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /thoughts", s.submit)
	mux.HandleFunc("GET /status", s.status)
	mux.HandleFunc("GET /events", s.events)
	mux.HandleFunc("GET /thoughts/{id}", func(w http.ResponseWriter, r *http.Request) {
		j, err := s.Store.Get(r.PathValue("id"))
		if err != nil {
			problem(w, 500, err)
			return
		}
		if j == nil {
			http.NotFound(w, r)
			return
		}
		jsonResponse(w, 200, j)
	})
	mux.HandleFunc("POST /thoughts/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Store.Retry(r.PathValue("id")); err != nil {
			problem(w, 409, err)
			return
		}
		s.Wake()
		jsonResponse(w, 202, map[string]string{"state": "queued"})
	})
	mux.HandleFunc("POST /thoughts/{id}/undo", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Undo(r.Context(), r.PathValue("id")); err != nil {
			problem(w, 409, err)
			return
		}
		jsonResponse(w, 200, map[string]string{"state": "undone"})
	})
	mux.Handle("/", frontend)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; media-src 'self' blob:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		if r.Method != "GET" && r.Method != "HEAD" {
			origin := r.Header.Get("Origin")
			if origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host {
					problem(w, 403, fmt.Errorf("cross-origin writes are not allowed"))
					return
				}
			}
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				problem(w, 403, fmt.Errorf("cross-site writes are not allowed"))
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
func (s *Service) submit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+(1<<20))
	var j queue.Job
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			problem(w, 400, err)
			return
		}
		defer r.MultipartForm.RemoveAll()
		j.ID, j.CapturedAt = r.FormValue("id"), r.FormValue("captured_at")
		file, header, err := r.FormFile("audio")
		if err != nil {
			problem(w, 400, err)
			return
		}
		defer file.Close()
		j.MIME = header.Header.Get("Content-Type")
		if !strings.HasPrefix(j.MIME, "audio/") {
			problem(w, 400, fmt.Errorf("audio content type is required"))
			return
		}
		j.Audio, err = io.ReadAll(io.LimitReader(file, maxUpload+1))
		if err != nil || len(j.Audio) == 0 || len(j.Audio) > maxUpload {
			problem(w, 400, fmt.Errorf("audio must be between 1 byte and 25 MiB"))
			return
		}
	} else {
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			problem(w, 415, fmt.Errorf("use application/json or multipart/form-data"))
			return
		}
		var input struct {
			ID         string `json:"id"`
			CapturedAt string `json:"captured_at"`
			Text       string `json:"text"`
		}
		decoder := json.NewDecoder(io.LimitReader(r.Body, queue.MaxText+4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			problem(w, 400, err)
			return
		}
		if decoder.Decode(new(any)) != io.EOF {
			problem(w, 400, fmt.Errorf("expected one JSON object"))
			return
		}
		j.ID, j.CapturedAt, j.Text = input.ID, input.CapturedAt, queue.NormalizeText(input.Text)
		if j.Text == "" || len(j.Text) > queue.MaxText || strings.ContainsRune(j.Text, 0) {
			problem(w, 400, fmt.Errorf("text must be between 1 and 16384 bytes and contain no NUL"))
			return
		}
	}
	if !uuidRE.MatchString(j.ID) {
		problem(w, 400, fmt.Errorf("id must be a UUID"))
		return
	}
	captured, err := time.Parse(time.RFC3339Nano, j.CapturedAt)
	if err != nil {
		problem(w, 400, fmt.Errorf("captured_at must be an RFC3339 timestamp"))
		return
	}
	j.ID = strings.ToLower(j.ID)
	j.CapturedAt = captured.Format(time.RFC3339Nano)
	saved, err := s.Store.Submit(j)
	if err != nil {
		problem(w, 500, err)
		return
	}
	s.Wake()
	jsonResponse(w, 202, saved)
}
func (s *Service) status(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.Store.Status()
	if err != nil {
		problem(w, 500, err)
		return
	}
	jsonResponse(w, 200, map[string]any{"jobs": jobs, "today": s.Config.Today(s.Now())})
}
func (s *Service) events(w http.ResponseWriter, r *http.Request) {
	if _, ok := w.(http.Flusher); !ok {
		http.Error(w, "streaming unavailable", 500)
		return
	}
	ch, unsubscribe := s.subscribe()
	defer unsubscribe()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	send := func() error {
		jobs, err := s.Store.Status()
		if err != nil {
			return err
		}
		b, err := json.Marshal(map[string]any{"jobs": jobs, "today": s.Config.Today(s.Now())})
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
