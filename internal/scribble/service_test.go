package scribble

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/agent/agenttest"
	"github.com/kpenfound/agentcore/jev"
	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
)

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
	s.Models.Judge = &jev.Judge{Config: func() jev.Settings { return settings }, Provider: p.Factory(), Getenv: func(string) string { return "fake" }}
	return p
}
func TestThresholdBoundaries(t *testing.T) {
	c := DefaultConfig().Jev
	today := "2026-11-05"
	for _, test := range []struct {
		date string
		p    float64
		want string
	}{{today, .75, "amend"}, {today, .7499, "link"}, {"2026-11-04", .75, "link"}, {"2026-11-04", .7499, "new"}} {
		if got := decide(&Entry{Date: test.date}, test.p, today, c); got != test.want {
			t.Fatalf("%+v got %s", test, got)
		}
	}
	if decide(nil, 1, today, c) != "new" {
		t.Fatal("none must always be new")
	}
}
func TestNewEntryRawInputAndTagProbabilities(t *testing.T) {
	s := testService(t)
	writeFixture(t, s.Config.Vault, tagsPath, tagsHeader+"| #idea/talk | Talks | 2 | 2026-11-04 |\n| #idea/post | Posts | 0 | |\n")
	inject(s, scripted("none", .99, .599, map[string]float64{"#idea/talk": .6, "#idea/post": .599}, "last"))
	j, err := s.Store.Submit(Job{ID: "one", CapturedAt: time.Now().Format(time.RFC3339), Text: "raw spelling & punctuation!"})
	if err != nil {
		t.Fatal(err)
	}
	// No CLI exists at this path: the ordinary route must never call it.
	s.Models.Runner.ClaudeBin = "/no/real/claude"
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
	date := s.Config.today(s.Now())
	path, _ := s.Vault.dailyPath(s.Now())
	writeFixture(t, s.Config.Vault, path, "# Daily\n## Scribbles\n- original #idea/talk ^scr-ab12\n## Journal\nkeep\n")
	writeFixture(t, s.Config.Vault, tagsPath, tagsHeader+"| #idea/talk | Talk ideas | 1 | "+date+" |\n")
	// Test the HTTP Jev client and calibrated probability, not confidence (.01).
	endpoint := jevtest.NewServer(t, scripted(path+"#^scr-ab12", .75, .8, map[string]float64{"#idea/talk": .8}, "none", "last"))
	s.Models.Judge = &jev.Judge{Config: endpoint.Settings}
	fake := agenttest.Session(t, agent.AgentClaude, agenttest.Turn{ResultText: `{"text":"original plus details","tags":["#project/demo"],"new_tags":[{"name":"#project/demo","meaning":"Demo project"}]}`})
	s.Models.Runner.ClaudeBin = fake.Bin
	j, _ := s.Store.Submit(Job{ID: "amend", CapturedAt: time.Now().Format(time.RFC3339), Text: "plus details"})
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
	past := s.Now().In(s.Config.location).AddDate(0, 0, -1)
	path, _ := s.Vault.dailyPath(past)
	original := "## Scribbles\n- earlier thought ^scr-past\n"
	writeFixture(t, s.Config.Vault, path, original)
	inject(s, scripted(path+"#^scr-past", .9, 0, nil, "none", "last"))
	j, _ := s.Store.Submit(Job{ID: "link", Text: "follow up", CapturedAt: past.Format(time.RFC3339)})
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
func TestDurableQueueFIFOAndRecovery(t *testing.T) {
	c := testConfig(t)
	s, err := OpenStore(c.QueueDB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(c.QueueDB); err == nil {
		t.Fatal("second writer acquired queue")
	}
	a, _ := s.Submit(Job{ID: "a", Text: "first", Audio: []byte{1, 2}, MIME: "audio/webm"})
	_, _ = s.Submit(Job{ID: "b", Text: "second"})
	duplicate, _ := s.Submit(Job{ID: "a", Text: "changed"})
	if duplicate.Text != "first" {
		t.Fatal("retry overwrote payload")
	}
	if err = s.State(a.ID, "routing"); err != nil {
		t.Fatal(err)
	}
	if err = s.Fail(a, c, errors.New("temporary"), time.Now()); err != nil {
		t.Fatal(err)
	}
	next, _ := s.Next()
	if next.ID != "a" || next.NextAt <= time.Now().UnixMilli() {
		t.Fatal("FIFO skipped retry backoff")
	}
	_ = s.Close()
	s, err = OpenStore(c.QueueDB)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	next, _ = s.Next()
	if next.ID != "a" || len(next.Audio) != 2 || next.Attempts != 1 {
		t.Fatalf("lost persisted queue: %+v", next)
	}
	for i := 1; i < c.MaxAttempts; i++ {
		if err = s.Fail(next, c, errors.New("failed"), time.Now()); err != nil {
			t.Fatal(err)
		}
		next, _ = s.Get("a")
	}
	next, _ = s.Next()
	if next.ID != "b" {
		t.Fatal("terminal failure blocks queue")
	}
	if err = s.Retry("a"); err != nil {
		t.Fatal(err)
	}
	next, _ = s.Next()
	if next.ID != "b" {
		t.Fatal("manual retry jumped the queue")
	}
}
func TestPreparedWriteRecovery(t *testing.T) {
	s := testService(t)
	date := s.Config.today(s.Now())
	path, _ := s.Vault.dailyPath(s.Now())
	p := Plan{Date: date, Path: path, Block: "scr-test", After: "- durable thought ^scr-test", Action: "new"}
	j, _ := s.Store.Submit(Job{ID: "recovery", Text: "durable thought"})
	if err := s.Store.Prepare(j.ID, p); err != nil {
		t.Fatal(err)
	}
	// Simulate death between note rename and completion in SQLite.
	if err := s.Vault.Apply(p); err != nil {
		t.Fatal(err)
	}
	j, _ = s.Store.Get(j.ID)
	s.Models.Judge = nil
	if err := s.process(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	j, _ = s.Store.Get(j.ID)
	if j.State != "written" || strings.Count(readFixture(t, s.Config.Vault, path), p.After) != 1 {
		t.Fatal("recovery duplicated write")
	}
}
func TestTranscriptionHTTPAndMissingConfiguration(t *testing.T) {
	s := testService(t)
	j := &Job{Audio: []byte("audio fixture"), MIME: "audio/webm"}
	if _, err := s.Models.Transcribe(context.Background(), j); err == nil {
		t.Fatal("missing endpoint did not fail")
	}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			t.Error("wrong path")
		}
		if err := r.ParseMultipartForm(1024); err != nil {
			t.Error(err)
		}
		defer r.MultipartForm.RemoveAll()
		f, h, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			http.Error(w, "missing file", 400)
			return
		}
		defer f.Close()
		b, _ := io.ReadAll(f)
		if string(b) != "audio fixture" || h.Header.Get("Content-Type") != "audio/webm" || r.FormValue("model") != "large-v3-turbo" {
			t.Error("invalid transcription request")
		}
		_, _ = io.WriteString(w, `{"text":"spoken\nthought"}`)
	}))
	defer endpoint.Close()
	s.Models.Config.Transcribe.URL = endpoint.URL + "/v1/audio/transcriptions"
	text, err := s.Models.Transcribe(context.Background(), j)
	if err != nil || text != "spoken thought" {
		t.Fatalf("transcription %q %v", text, err)
	}
}
func TestSubmissionIsDurableIdempotentAndOriginChecked(t *testing.T) {
	s := testService(t)
	handler := s.Handler(http.NotFoundHandler())
	payload := `{"id":"00000000-0000-4000-8000-000000000001","captured_at":"2026-11-05T12:00:00Z","text":"  first\nsecond  "}`
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest("POST", "http://scribble/thoughts", strings.NewReader(payload))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 202 {
			t.Fatal(w.Code, w.Body.String())
		}
		var j Job
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
	r := httptest.NewRequest("POST", "http://scribble/thoughts", bytes.NewBufferString(payload))
	r.Header.Set("Origin", "https://attacker.example")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("accepted cross-origin write")
	}
}
func TestUndoRefusesExternalEdit(t *testing.T) {
	s := testService(t)
	inject(s, scripted("none", 1, 0, nil, "last"))
	j, _ := s.Store.Submit(Job{ID: "undo", Text: "hello"})
	if err := s.process(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	j, _ = s.Store.Get(j.ID)
	raw := readFixture(t, s.Config.Vault, j.Plan.Path)
	writeFixture(t, s.Config.Vault, j.Plan.Path, strings.Replace(raw, "hello", "my edit", 1))
	if err := s.performUndo(j.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("overwrote external edit", err)
	}
	intent, _ := s.Store.UndoIntent()
	if intent != nil {
		t.Fatal("invalid undo was journaled")
	}
}
func TestConfigValidation(t *testing.T) {
	for _, format := range []string{"../YYYY-MM-DD", "YYYY-ww-DD", "YYYY-MM", "[bad"} {
		if _, err := momentLayout(format); err == nil {
			t.Fatalf("accepted %s", format)
		}
	}
	c := testConfig(t)
	c.Listen = "0.0.0.0:8090"
	if c.Validate() == nil {
		t.Fatal("public listener accepted")
	}
	writeFixture(t, c.Vault, ".obsidian/daily-notes.json", `{"folder":"../escape"}`)
	v, err := OpenVault(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if _, err = v.dailyPath(time.Now()); err == nil {
		t.Fatal("escaping config accepted")
	}
}

func TestUnwrittenIntentReroutesAfterMidnight(t *testing.T) {
	s := testService(t)
	today := s.Config.today(s.Now())
	past := s.Now().In(s.Config.location).AddDate(0, 0, -1)
	path, _ := s.Vault.dailyPath(past)
	original := "# Yesterday\n## Scribbles\n- existing ^scr-orig\n"
	writeFixture(t, s.Config.Vault, path, original)
	j, _ := s.Store.Submit(Job{ID: "midnight", Text: "captured late", CapturedAt: past.Format(time.RFC3339)})
	p := Plan{Date: s.Config.today(past), Path: path, Block: "scr-test", After: "- captured late ^scr-test", Action: "new"}
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

func TestRestartRestoresInFlightVoiceAndUndoJournal(t *testing.T) {
	c := testConfig(t)
	store, err := OpenStore(c.QueueDB)
	if err != nil {
		t.Fatal(err)
	}
	j, _ := store.Submit(Job{ID: "inflight", Audio: []byte("voice"), MIME: "audio/webm"})
	if err = store.State(j.ID, "transcribing"); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(c.QueueDB)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	j, _ = store.Next()
	if j.State != "queued" || string(j.Audio) != "voice" {
		t.Fatalf("lost in-flight voice: %+v", j)
	}
	now := time.Now()
	v, err := OpenVault(c, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	path, _ := v.dailyPath(now)
	p := Plan{Date: c.today(now), Path: path, Block: "scr-test", After: "- crash test ^scr-test", Action: "new"}
	if err = store.Prepare(j.ID, p); err != nil {
		t.Fatal(err)
	}
	if err = v.Apply(p); err != nil {
		t.Fatal(err)
	}
	if err = store.Complete(j.ID, p, now); err != nil {
		t.Fatal(err)
	}
	p.Before, p.After = p.After, ""
	p.Undo = true
	intent := UndoIntent{JobID: j.ID, Plan: p}
	if err = store.PrepareUndo(intent); err != nil {
		t.Fatal(err)
	}
	if err = v.Apply(p); err != nil {
		t.Fatal(err)
	}
	// Death after the inverse rename but before recording undone must be safe.
	recovered, err := store.UndoIntent()
	if err != nil {
		t.Fatal(err)
	}
	if err = v.Apply(recovered.Plan); err != nil {
		t.Fatal(err)
	}
	if err = store.CompleteUndo(j.ID); err != nil {
		t.Fatal(err)
	}
	j, _ = store.Get(j.ID)
	if !j.Undone || strings.Contains(readFixture(t, c.Vault, path), "crash test") {
		t.Fatal("undo recovery failed")
	}
}
