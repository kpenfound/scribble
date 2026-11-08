package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
		var j queue.Job
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
