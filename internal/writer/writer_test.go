package writer

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/agent/agenttest"

	"github.com/kpenfound/scribble/internal/vault"
)

func testWriter(t *testing.T, turns ...agenttest.Turn) (*Claude, *agenttest.SessionFixture) {
	t.Helper()
	fake := agenttest.Session(t, agent.AgentClaude, turns...)
	// The writer has no configurable binary: the fake is "claude" on PATH.
	t.Setenv("PATH", fake.Dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := t.TempDir()
	w := New(Settings{Model: "claude-haiku-5-5", Timeout: 90 * time.Second, Dir: dir, SessionsDir: filepath.Join(dir, "sessions")})
	return w, fake
}

func TestWriterAcceptsValidAmend(t *testing.T) {
	w, fake := testWriter(t, agenttest.Turn{ResultText: `{"text":"original plus details","tags":["#idea/talk"],"new_tags":[]}`})
	target := &vault.Entry{Line: "- original #idea/talk ^scr-ab12"}
	got, _, err := w.Write(context.Background(), "plus details", target, []string{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "original plus details" || fake.Record(t).Starts != 1 {
		t.Fatalf("unexpected result %+v", got)
	}
}

func TestWriterRejectsInvalidOutput(t *testing.T) {
	for name, result := range map[string]string{
		"multiline":    `{"text":"a\nb","tags":[],"new_tags":[]}`,
		"block id":     `{"text":"a ^scr-ab12","tags":[],"new_tags":[]}`,
		"unknown tag":  `{"text":"a","tags":["#nope"],"new_tags":[]}`,
		"unwanted tag": `{"text":"a","tags":[],"new_tags":[{"name":"#a/b","meaning":"m"}]}`,
		"flat tag":     `{"text":"a","tags":["#a"],"new_tags":[{"name":"#a","meaning":"m"}]}`,
		"extra field":  `{"text":"a","tags":[],"new_tags":[],"x":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			w, _ := testWriter(t, agenttest.Turn{ResultText: result})
			newTag := strings.Contains(name, "flat")
			if _, _, err := w.Write(context.Background(), "in", &vault.Entry{Line: "- t ^scr-ab12"}, nil, nil, newTag); err == nil {
				t.Fatal("accepted invalid output")
			}
		})
	}
}

func TestWriterRunsClaudeFromPathWithProfileModel(t *testing.T) {
	w, fake := testWriter(t, agenttest.Turn{ResultText: `{"text":"a","tags":[],"new_tags":[]}`})
	w.Model = "claude-profile-model"
	if w.Runner.ClaudeBin != "" {
		t.Fatalf("writer configures a binary: %q", w.Runner.ClaudeBin)
	}
	if _, _, err := w.Write(context.Background(), "in", &vault.Entry{Line: "- t ^scr-ab12"}, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	rec := fake.Record(t)
	if rec.Starts != 1 {
		t.Fatalf("PATH claude started %d times", rec.Starts)
	}
	args := rec.Sessions[0].Args
	i := slices.Index(args, "--model")
	if i < 0 || i+1 >= len(args) || args[i+1] != "claude-profile-model" {
		t.Fatalf("model not passed: %v", args)
	}
}

func TestWriterTakesProfileTimeout(t *testing.T) {
	w := New(Settings{Model: "m", Timeout: 7 * time.Second, Dir: t.TempDir()})
	if w.Timeout != 7*time.Second {
		t.Fatalf("timeout %v", w.Timeout)
	}
}
