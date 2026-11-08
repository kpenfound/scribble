package writer

import (
	"context"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/agent/agenttest"

	"github.com/kpenfound/scribble/internal/vault"
)

func testWriter(t *testing.T, turns ...agenttest.Turn) (*Claude, *agenttest.SessionFixture) {
	t.Helper()
	fake := agenttest.Session(t, agent.AgentClaude, turns...)
	w := New(fake.Bin, "claude-haiku-5-5", t.TempDir())
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
