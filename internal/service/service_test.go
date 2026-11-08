package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/agent/agenttest"
	"github.com/kpenfound/agentcore/jev"
	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"

	"github.com/kpenfound/scribble/internal/api"
	"github.com/kpenfound/scribble/internal/buildinfo"
	"github.com/kpenfound/scribble/internal/config"
	"github.com/kpenfound/scribble/internal/queue"
	"github.com/kpenfound/scribble/internal/routing"
	"github.com/kpenfound/scribble/internal/vault"
	"github.com/kpenfound/scribble/internal/writer"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	c := config.DefaultConfig()
	c.Vault = t.TempDir()
	c.Paths = config.PathsAt(t.TempDir())
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c
}
func writeFixture(t *testing.T, root, path, text string) {
	t.Helper()
	path = filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0640); err != nil {
		t.Fatal(err)
	}
}
func readFixture(t *testing.T, root, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func testService(t *testing.T) *Service {
	t.Helper()
	s, err := NewService(testConfig(t), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func scripted(target string, p, newTag float64, tags map[string]float64, others ...string) jevtest.Result {
	answers := map[string]systemone.Answer{"target": jevtest.Choice(target, p, .01, others...), "new_tag": jevtest.Noul(newTag)}
	for k, v := range tags {
		answers[k] = jevtest.Noul(v)
	}
	return jevtest.Result{Response: systemone.Response{Model: "fixture", Answers: answers, Usage: systemone.Usage{InputTokens: 10}}}
}
func inject(s *Service, results ...jevtest.Result) *jevtest.Provider {
	p := &jevtest.Provider{Results: results}
	settings := jev.DefaultSettings()
	settings.Enabled = true
	s.Router = &routing.Jev{Judge: &jev.Judge{Config: func() jev.Settings { return settings }, Provider: p.Factory(), Getenv: func(string) string { return "fake" }}}
	return p
}

// claude is the service's real writer, so tests can point its runner at a fake CLI.
func claude(s *Service) *writer.Claude { return s.Writer.(*writer.Claude) }
func TestNewEntryRawInputAndTagProbabilities(t *testing.T) {
	s := testService(t)
	writeFixture(t, s.Config.Vault, vault.TagsPath, vault.TagsHeader+"| #idea/talk | Talks | 2 | 2026-11-04 |\n| #idea/post | Posts | 0 | |\n")
	inject(s, scripted("none", .99, .599, map[string]float64{"#idea/talk": .6, "#idea/post": .599}, "last"))
	j, err := s.Store.Submit(queue.Job{ID: "one", CapturedAt: time.Now().Format(time.RFC3339), Text: "raw spelling & punctuation!"})
	if err != nil {
		t.Fatal(err)
	}
	// No CLI exists at this path: the ordinary route must never call it.
	claude(s).Runner.ClaudeBin = "/no/real/claude"
	if err = s.process(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	j, _ = s.Store.Get(j.ID)
	if j.State != "written" || !strings.HasPrefix(j.Line, "- raw spelling & punctuation! #idea/talk ^scr-") || strings.Contains(j.Line, "#idea/post") {
		t.Fatalf("bad write: %+v", j)
	}
	if err = s.process(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	raw := readFixture(t, s.Config.Vault, j.Plan.Path)
	if strings.Count(raw, j.Line) != 1 {
		t.Fatal("duplicate replay")
	}
}
func TestAmendNewTagUndoUsingAgentcore(t *testing.T) {
	s := testService(t)
	date := s.Config.Today(s.Now())
	path, _ := s.Vault.DailyPath(s.Now())
	writeFixture(t, s.Config.Vault, path, "# Daily\n## Scribbles\n- original #idea/talk ^scr-ab12\n## Journal\nkeep\n")
	writeFixture(t, s.Config.Vault, vault.TagsPath, vault.TagsHeader+"| #idea/talk | Talk ideas | 1 | "+date+" |\n")
	// Test the HTTP Jev client and calibrated probability, not confidence (.01).
	endpoint := jevtest.NewServer(t, scripted(path+"#^scr-ab12", .75, .8, map[string]float64{"#idea/talk": .8}, "none", "last"))
	s.Router = &routing.Jev{Judge: &jev.Judge{Config: endpoint.Settings}}
	fake := agenttest.Session(t, agent.AgentClaude, agenttest.Turn{ResultText: `{"text":"original plus details","tags":["#project/demo"],"new_tags":[{"name":"#project/demo","meaning":"Demo project"}]}`})
	t.Setenv("PATH", fake.Dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	j, _ := s.Store.Submit(queue.Job{ID: "amend", CapturedAt: time.Now().Format(time.RFC3339), Text: "plus details"})
	if err := s.process(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	j, _ = s.Store.Get(j.ID)
	if j.Action != "amend" || j.Previous != "- original #idea/talk ^scr-ab12" || strings.Count(j.Line, "#project/demo") != 1 || strings.Count(j.Line, "#idea/talk") != 1 {
		t.Fatalf("bad amend: %+v", j)
	}
	if fake.Record(t).Starts != 1 || len(endpoint.Requests()) != 1 {
		t.Fatal("wrong provider calls")
	}
	if err := s.performUndo(j.ID); err != nil {
		t.Fatal(err)
	}
	if got := readFixture(t, s.Config.Vault, path); got != "# Daily\n## Scribbles\n- original #idea/talk ^scr-ab12\n## Journal\nkeep\n" {
		t.Fatalf("undo changed bytes: %q", got)
	}
	if err := s.performUndo(j.ID); err != nil {
		t.Fatal("undo must be idempotent", err)
	}
}
func TestEarlierDayLinkNeverAmends(t *testing.T) {
	s := testService(t)
	past := s.Now().In(s.Config.Location()).AddDate(0, 0, -1)
	path, _ := s.Vault.DailyPath(past)
	original := "## Scribbles\n- earlier thought ^scr-past\n"
	writeFixture(t, s.Config.Vault, path, original)
	inject(s, scripted(path+"#^scr-past", .9, 0, nil, "none", "last"))
	j, _ := s.Store.Submit(queue.Job{ID: "link", Text: "follow up", CapturedAt: past.Format(time.RFC3339)})
	if err := s.process(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	j, _ = s.Store.Get(j.ID)
	if j.Action != "link" || !strings.Contains(j.Line, "[["+strings.TrimSuffix(filepath.Base(path), ".md")+"#^scr-past]]") {
		t.Fatalf("missing link: %+v", j)
	}
	if readFixture(t, s.Config.Vault, path) != original {
		t.Fatal("edited a past note")
	}
}
func TestPreparedWriteRecovery(t *testing.T) {
	s := testService(t)
	date := s.Config.Today(s.Now())
	path, _ := s.Vault.DailyPath(s.Now())
	p := vault.Plan{Date: date, Path: path, Block: "scr-test", After: "- durable thought ^scr-test", Action: "new"}
	j, _ := s.Store.Submit(queue.Job{ID: "recovery", Text: "durable thought"})
	if err := s.Store.Prepare(j.ID, p); err != nil {
		t.Fatal(err)
	}
	// Simulate death between note rename and completion in SQLite.
	if err := s.Vault.Apply(p); err != nil {
		t.Fatal(err)
	}
	j, _ = s.Store.Get(j.ID)
	s.Router = nil
	if err := s.process(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	j, _ = s.Store.Get(j.ID)
	if j.State != "written" || strings.Count(readFixture(t, s.Config.Vault, path), p.After) != 1 {
		t.Fatal("recovery duplicated write")
	}
}
func TestSubmissionIsDurableIdempotentAndOriginChecked(t *testing.T) {
	s := testService(t)
	handler := s.WebHandler(http.NotFoundHandler(), []string{"scribble"})
	payload := `{"id":"00000000-0000-4000-8000-000000000001","captured_at":"2026-11-05T12:00:00Z","text":"  first\nsecond  "}`
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest("POST", "http://scribble/v1/thoughts", strings.NewReader(payload))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 202 {
			t.Fatal(w.Code, w.Body.String())
		}
		var j api.Job
		if err := json.Unmarshal(w.Body.Bytes(), &j); err != nil {
			t.Fatal(err)
		}
		if j.State != "queued" {
			t.Fatal("submission waited on processing")
		}
	}
	j, _ := s.Store.Next()
	if j.Text != "first second" {
		t.Fatal("incorrect normalization")
	}
	jobs, _ := s.Store.Status()
	if len(jobs) != 1 {
		t.Fatal("duplicate jobs")
	}
	r := httptest.NewRequest("POST", "http://scribble/v1/thoughts", bytes.NewBufferString(payload))
	r.Header.Set("Origin", "https://attacker.example")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("accepted cross-origin write")
	}
}

// do sends one request through handler and returns the recorded response.
func do(handler http.Handler, method, target, body string, header map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

var jsonType = map[string]string{"Content-Type": "application/json"}

func submitBody(id string) string {
	return `{"id":"` + id + `","captured_at":"2026-11-05T12:00:00Z","text":"hello"}`
}

const (
	idA = "00000000-0000-4000-8000-00000000000a"
	idB = "00000000-0000-4000-8000-00000000000b"
)

func TestWebHandlerServesExactlyTheV1Routes(t *testing.T) {
	var got []string
	for _, r := range Routes {
		got = append(got, r.Method+" "+r.Path)
	}
	want := []string{"POST /v1/thoughts", "GET /v1/thoughts/{id}", "POST /v1/thoughts/{id}/retry", "POST /v1/thoughts/{id}/undo", "GET /v1/status", "GET /v1/events", "GET /v1/health"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("routes:\n%s", strings.Join(got, "\n"))
	}
	s := testService(t)
	handler := s.WebHandler(http.NotFoundHandler(), []string{"scribble"})
	if w := do(handler, "POST", "http://scribble/v1/thoughts", submitBody(idA), jsonType); w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, route := range []string{"GET /v1/thoughts/" + idA, "GET /v1/status", "GET /v1/health"} {
		method, path, _ := strings.Cut(route, " ")
		if w := do(handler, method, "http://scribble"+path, "", nil); w.Code != 200 {
			t.Fatal(route, w.Code, w.Body.String())
		}
	}
	// The frontend answers everything outside /v1; unversioned API paths and
	// the old health probe are not routes at all.
	for _, route := range []string{"POST /thoughts", "GET /thoughts/" + idA, "POST /thoughts/" + idA + "/retry", "POST /thoughts/" + idA + "/undo", "GET /status", "GET /events", "GET /healthz"} {
		method, path, _ := strings.Cut(route, " ")
		if w := do(handler, method, "http://scribble"+path, submitBody(idB), jsonType); w.Code != 404 {
			t.Fatalf("%s answered %d", route, w.Code)
		}
	}
	// A path under /v1 that is not in Routes is not found, in the envelope.
	w := do(handler, "GET", "http://scribble/v1/nothing", "", nil)
	expectError(t, w, 404, api.NotFound)
}

func expectError(t *testing.T, w *httptest.ResponseRecorder, status int, code api.Code) api.Error {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d: %s", w.Code, status, w.Body.String())
	}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("content type %q", w.Header().Get("Content-Type"))
	}
	var raw map[string]map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err, w.Body.String())
	}
	if len(raw) != 1 || len(raw["error"]) != 2 {
		t.Fatalf("not the {error:{code,message}} envelope: %s", w.Body.String())
	}
	var env api.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != code || env.Error.Message == "" {
		t.Fatalf("error %+v, want code %s with a message", env.Error, code)
	}
	return env.Error
}

func TestEveryFailureUsesTheErrorEnvelope(t *testing.T) {
	s := testService(t)
	handler := s.SocketHandler()
	// A queued job refuses retry; an unrelated queued job refuses undo.
	if w := do(handler, "POST", "http://x/v1/thoughts", submitBody(idA), jsonType); w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	park(t, s, idA)
	runWorker(t, s)
	tooLong := `{"id":"` + idA + `","captured_at":"2026-11-05T12:00:00Z","text":"` + strings.Repeat("x", queue.MaxText+1) + `"}`
	multipartNoAudio := "--b\r\nContent-Disposition: form-data; name=\"id\"\r\n\r\n" + idB + "\r\n--b--\r\n"
	for _, tc := range []struct {
		name, method, path, body string
		header                   map[string]string
		status                   int
		code                     api.Code
	}{
		{"invalid JSON", "POST", "/v1/thoughts", `{`, jsonType, 400, api.Validation},
		{"unknown field", "POST", "/v1/thoughts", `{"id":"` + idB + `","captured_at":"2026-11-05T12:00:00Z","text":"x","extra":1}`, jsonType, 400, api.Validation},
		{"second JSON value", "POST", "/v1/thoughts", submitBody(idB) + submitBody(idB), jsonType, 400, api.Validation},
		{"empty text", "POST", "/v1/thoughts", `{"id":"` + idB + `","captured_at":"2026-11-05T12:00:00Z","text":"  "}`, jsonType, 400, api.Validation},
		{"text too long", "POST", "/v1/thoughts", tooLong, jsonType, 400, api.Validation},
		{"bad id", "POST", "/v1/thoughts", `{"id":"nope","captured_at":"2026-11-05T12:00:00Z","text":"x"}`, jsonType, 400, api.Validation},
		{"bad timestamp", "POST", "/v1/thoughts", `{"id":"` + idB + `","captured_at":"yesterday","text":"x"}`, jsonType, 400, api.Validation},
		{"multipart without audio", "POST", "/v1/thoughts", multipartNoAudio, map[string]string{"Content-Type": "multipart/form-data; boundary=b"}, 400, api.Validation},
		{"wrong content type", "POST", "/v1/thoughts", "hello", map[string]string{"Content-Type": "text/plain"}, 415, api.UnsupportedMedia},
		{"unknown thought", "GET", "/v1/thoughts/" + idB, "", nil, 404, api.NotFound},
		{"retry unknown", "POST", "/v1/thoughts/" + idB + "/retry", "", nil, 404, api.NotFound},
		{"undo unknown", "POST", "/v1/thoughts/" + idB + "/undo", "", nil, 404, api.NotFound},
		{"unknown route", "GET", "/v1/elsewhere", "", nil, 404, api.NotFound},
		{"retry of a queued job", "POST", "/v1/thoughts/" + idA + "/retry", "", nil, 409, api.Conflict},
		{"undo of a job that was never written", "POST", "/v1/thoughts/" + idA + "/undo", "", nil, 409, api.Conflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expectError(t, do(handler, tc.method, "http://x"+tc.path, tc.body, tc.header), tc.status, tc.code)
		})
	}
}

// runWorker starts the worker goroutine, which performs undo, until the test ends.
func runWorker(t *testing.T, s *Service) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = s.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

// park fails job id once with an hour of backoff: it stays queued but the worker leaves it alone.
func park(t *testing.T, s *Service, id string) {
	t.Helper()
	if err := s.Store.Fail(&queue.Job{ID: id}, queue.RetryPolicy{MaxAttempts: 5, Backoff: time.Hour}, errors.New("parked"), s.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestRetrySuccessBody(t *testing.T) {
	s := testService(t)
	handler := s.SocketHandler()
	do(handler, "POST", "http://x/v1/thoughts", submitBody(idB), jsonType)
	if err := s.Store.Fail(&queue.Job{ID: idB, Attempts: 99}, queue.RetryPolicy{MaxAttempts: 1}, errors.New("boom"), s.Now()); err != nil {
		t.Fatal(err)
	}
	w := do(handler, "POST", "http://x/v1/thoughts/"+idB+"/retry", "", nil)
	if w.Code != 202 || strings.TrimSpace(w.Body.String()) != `{"state":"queued"}` {
		t.Fatal(w.Code, w.Body.String())
	}
	if j, _ := s.Store.Get(idB); j.State != "queued" {
		t.Fatal("not queued again:", j.State)
	}
}

func TestUndoSuccessBodyAndRefusalOfAnOlderWrite(t *testing.T) {
	s := testService(t)
	handler := s.SocketHandler()
	inject(s, scripted("none", 1, 0, nil, "last"), scripted("none", 1, 0, nil, "last"))
	for _, id := range []string{idA, idB} {
		do(handler, "POST", "http://x/v1/thoughts", submitBody(id), jsonType)
		j, _ := s.Store.Next()
		if err := s.process(context.Background(), j); err != nil {
			t.Fatal(err)
		}
	}
	runWorker(t, s)
	expectError(t, do(handler, "POST", "http://x/v1/thoughts/"+idA+"/undo", "", nil), 409, api.Conflict)
	w := do(handler, "POST", "http://x/v1/thoughts/"+idB+"/undo", "", nil)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"state":"undone"}` {
		t.Fatal(w.Code, w.Body.String())
	}
}

type fakeTailnet api.TailnetStatus

func (f fakeTailnet) TailnetStatus() api.TailnetStatus { return api.TailnetStatus(f) }

func TestHealthReportsBuildAndTailnetState(t *testing.T) {
	version, commit, date := buildinfo.Version, buildinfo.Commit, buildinfo.Date
	t.Cleanup(func() { buildinfo.Version, buildinfo.Commit, buildinfo.Date = version, commit, date })
	buildinfo.Version, buildinfo.Commit, buildinfo.Date = "v9.9.9", "abc123", "2026-11-06T00:00:00Z"
	for _, tc := range []struct {
		name    string
		tailnet TailnetStatuser
		want    api.TailnetStatus
	}{
		{"healthy", fakeTailnet{State: api.TailnetRunning, URL: "https://scribble.example.ts.net"}, api.TailnetStatus{State: "running", URL: "https://scribble.example.ts.net"}},
		{"degraded", fakeTailnet{State: api.TailnetDegraded, Reason: "login required"}, api.TailnetStatus{State: "degraded", Reason: "login required"}},
		{"not configured", nil, api.TailnetStatus{State: "disabled"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testService(t)
			s.Tailnet = tc.tailnet
			for name, handler := range map[string]http.Handler{"socket": s.SocketHandler(), "web": s.WebHandler(nil, []string{"scribble"})} {
				w := do(handler, "GET", "http://scribble/v1/health", "", nil)
				if w.Code != 200 {
					t.Fatal(name, w.Code, w.Body.String())
				}
				var h api.Health
				if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
					t.Fatal(err)
				}
				if h.Status != "ok" || h.Build != (api.Build{Version: "v9.9.9", Commit: "abc123", Date: "2026-11-06T00:00:00Z"}) || h.Tailnet != tc.want {
					t.Fatalf("%s: %+v", name, h)
				}
			}
		})
	}
}

func TestBrowserChecksApplyToWebHandlerOnly(t *testing.T) {
	s := testService(t)
	hosts := []string{"127.0.0.1:8090", "scribble.example.ts.net"}
	web := s.WebHandler(http.NotFoundHandler(), hosts)
	socket := s.SocketHandler()
	ok := map[string]string{"Content-Type": "application/json"}
	with := func(extra map[string]string) map[string]string {
		h := map[string]string{}
		for k, v := range ok {
			h[k] = v
		}
		for k, v := range extra {
			h[k] = v
		}
		return h
	}
	post := func(handler http.Handler, host string, header map[string]string) *httptest.ResponseRecorder {
		return do(handler, "POST", "http://"+host+"/v1/thoughts", submitBody(idA), header)
	}
	// The listener's own names pass, same-origin uploads included.
	for _, host := range hosts {
		if w := post(web, host, with(map[string]string{"Origin": "http://" + host, "Sec-Fetch-Site": "same-origin"})); w.Code != 202 {
			t.Fatal(host, w.Code, w.Body.String())
		}
	}
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	_ = mw.WriteField("id", idB)
	_ = mw.WriteField("captured_at", "2026-11-05T12:00:00Z")
	part, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="audio"; filename="capture"`}, "Content-Type": {"audio/webm"}})
	_, _ = part.Write([]byte("audio bytes"))
	_ = mw.Close()
	upload := do(web, "POST", "http://127.0.0.1:8090/v1/thoughts", form.String(), map[string]string{"Content-Type": mw.FormDataContentType(), "Origin": "http://127.0.0.1:8090"})
	if upload.Code != 202 {
		t.Fatal("same-origin audio upload refused:", upload.Code, upload.Body.String())
	}
	// Cross-site writes are refused on the web handler and ignored on the socket.
	cross := map[string]map[string]string{
		"cross-origin Origin":    {"Origin": "https://attacker.example"},
		"cross-site Fetch-Site":  {"Sec-Fetch-Site": "cross-site"},
		"Origin of another port": {"Origin": "http://127.0.0.1:9999"},
	}
	for name, extra := range cross {
		t.Run(name, func(t *testing.T) {
			expectError(t, post(web, "127.0.0.1:8090", with(extra)), 403, api.Forbidden)
			if w := post(socket, "socket", with(extra)); w.Code != 202 {
				t.Fatal("socket applied a browser check:", w.Code, w.Body.String())
			}
		})
	}
	// Reads with a foreign Origin are not writes.
	if w := do(web, "GET", "http://127.0.0.1:8090/v1/status", "", map[string]string{"Origin": "https://attacker.example", "Sec-Fetch-Site": "cross-site"}); w.Code != 200 {
		t.Fatal("refused a cross-site read:", w.Code)
	}
	// An unexpected Host is refused for every method and path, DNS rebinding
	// included; the socket serves whatever Host it is given.
	for _, host := range []string{"attacker.example", "attacker.example:8090", "127.0.0.1:9999", "127.0.0.1", "scribble.example.ts.net:8090", ""} {
		for _, method := range []string{"GET", "POST"} {
			for _, path := range []string{"/v1/status", "/v1/health", "/", "/app.js"} {
				r := httptest.NewRequest(method, "http://placeholder"+path, strings.NewReader(submitBody(idA)))
				r.Host = host
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				web.ServeHTTP(w, r)
				expectError(t, w, 403, api.Forbidden)
			}
		}
	}
	for _, host := range []string{"attacker.example", "anything:1"} {
		if w := do(socket, "GET", "http://"+host+"/v1/status", "", nil); w.Code != 200 {
			t.Fatal("socket refused Host", host, w.Code)
		}
	}
	// No host list refuses everything.
	if w := do(s.WebHandler(nil, nil), "GET", "http://127.0.0.1:8090/v1/health", "", nil); w.Code != 403 {
		t.Fatal(w.Code)
	}
	// Security headers, with the CSP exactly as it was.
	const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; media-src 'self' blob:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'"
	for _, w := range []*httptest.ResponseRecorder{do(web, "GET", "http://127.0.0.1:8090/v1/health", "", nil), do(web, "GET", "http://127.0.0.1:8090/", "", nil), post(web, "127.0.0.1:8090", with(cross["cross-origin Origin"]))} {
		if got := w.Header().Get("Content-Security-Policy"); got != csp {
			t.Fatalf("CSP changed: %q", got)
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Referrer-Policy") != "same-origin" {
			t.Fatal("security headers missing")
		}
	}
}

func TestLoopbackHosts(t *testing.T) {
	got := LoopbackHosts("127.0.0.1:8090")
	if strings.Join(got, ",") != "127.0.0.1:8090,localhost:8090,[::1]:8090" {
		t.Fatal(got)
	}
	if LoopbackHosts("not an address") != nil {
		t.Fatal("accepted a bad address")
	}
}

func TestSocketHandlerMountsControlsOutsideTheWebHandler(t *testing.T) {
	s := testService(t)
	control := Control{Pattern: "GET /v1/control/ping", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { api.WriteJSON(w, 200, map[string]string{"pong": "yes"}) })}
	socket := s.SocketHandler(control)
	if w := do(socket, "GET", "http://x/v1/control/ping", "", nil); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := do(socket, "GET", "http://x/v1/health", "", nil); w.Code != 200 {
		t.Fatal("public routes missing on the socket:", w.Code)
	}
	expectError(t, do(socket, "GET", "http://x/", "", nil), 404, api.NotFound)
	web := s.WebHandler(nil, []string{"x"})
	expectError(t, do(web, "GET", "http://x/v1/control/ping", "", nil), 404, api.NotFound)
	for _, r := range Routes {
		if strings.Contains(r.Path, "control") {
			t.Fatal("control in the public routes:", r.Path)
		}
	}
}

func TestUndoRefusesExternalEdit(t *testing.T) {
	s := testService(t)
	inject(s, scripted("none", 1, 0, nil, "last"))
	j, _ := s.Store.Submit(queue.Job{ID: "undo", Text: "hello"})
	if err := s.process(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	j, _ = s.Store.Get(j.ID)
	raw := readFixture(t, s.Config.Vault, j.Plan.Path)
	writeFixture(t, s.Config.Vault, j.Plan.Path, strings.Replace(raw, "hello", "my edit", 1))
	if err := s.performUndo(j.ID); !errors.Is(err, vault.ErrConflict) {
		t.Fatal("overwrote external edit", err)
	}
	intent, _ := s.Store.UndoIntent()
	if intent != nil {
		t.Fatal("invalid undo was journaled")
	}
}
func TestUnwrittenIntentReroutesAfterMidnight(t *testing.T) {
	s := testService(t)
	today := s.Config.Today(s.Now())
	past := s.Now().In(s.Config.Location()).AddDate(0, 0, -1)
	path, _ := s.Vault.DailyPath(past)
	original := "# Yesterday\n## Scribbles\n- existing ^scr-orig\n"
	writeFixture(t, s.Config.Vault, path, original)
	j, _ := s.Store.Submit(queue.Job{ID: "midnight", Text: "captured late", CapturedAt: past.Format(time.RFC3339)})
	p := vault.Plan{Date: s.Config.Today(past), Path: path, Block: "scr-test", After: "- captured late ^scr-test", Action: "new"}
	if err := s.Store.Prepare(j.ID, p); err != nil {
		t.Fatal(err)
	}
	j, _ = s.Store.Get(j.ID)
	inject(s, scripted("none", .99, 0, nil, "last", path+"#^scr-orig"))
	if err := s.process(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	j, _ = s.Store.Get(j.ID)
	if j.Date != today || j.State != "written" || j.Plan.Path == path {
		t.Fatalf("wrong processing date: %+v", j)
	}
	if readFixture(t, s.Config.Vault, path) != original {
		t.Fatal("changed past note")
	}
}
