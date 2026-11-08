package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/kpenfound/agentcore/agent"
	"github.com/kpenfound/agentcore/agent/agenttest"
	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
)

// failUntilRetried is how many Jev calls for a "fail until retried" thought are
// rejected: every automatic attempt, so the job ends failed and only a manual
// retry (a later call) succeeds. It is the queue.max_attempts the fixture sets.
const failUntilRetried = 3

// This opt-in fixture runs the production binary as `scribble serve --root`.
// Only external providers are fake: Jev and Whisper are local HTTP servers and
// `claude` is the agentcore agenttest double placed first on the service's
// PATH. The small gateway derives offered random block IDs from each request,
// then lets agentcore's HTTP fixture serialize its scripted typed response.
//
// Playwright owns the lifecycle: it builds bin/scribble, starts this test with
// SCRIBBLE_E2E_DIR naming a scratch directory, and polls the web listener's
// /v1/health. The state root is <dir>/root, so the specs can run the CLI with
// the same --root. The fixture records the service's pid in <dir>/service.pid
// and its exit status in <dir>/service.exit, and after `scribble stop` ends the
// service it keeps running until Playwright interrupts it, so a stopped
// service is not mistaken for a failed web server.
func TestFixtureServer(t *testing.T) {
	if os.Getenv("SCRIBBLE_E2E") != "1" {
		t.Skip("launched by Playwright only")
	}
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := os.Getenv("SCRIBBLE_E2E_DIR")
	if dir == "" {
		t.Fatal("SCRIBBLE_E2E_DIR names the scratch directory shared with the specs")
	}
	if dir, err = filepath.Abs(dir); err != nil {
		t.Fatal(err)
	}
	vault := filepath.Join(dir, "vault")
	if err = os.MkdirAll(filepath.Join(vault, "Daily"), 0750); err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation("America/Detroit")
	if err != nil {
		t.Fatal(err)
	}
	yesterday := time.Now().In(loc).AddDate(0, 0, -1).Format("2006-01-02")
	if err = os.WriteFile(filepath.Join(vault, "Daily", yesterday+".md"), []byte("# Yesterday\n## Scribbles\n- Yesterday's idea ^scr-past\n"), 0640); err != nil {
		t.Fatal(err)
	}
	var rejectedMu sync.Mutex
	rejected := map[string]int{}
	jevGateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var request struct {
			State struct {
				Input   string `json:"input"`
				Entries []struct {
					ID   string `json:"id"`
					Date string `json:"date"`
				} `json:"entries"`
			} `json:"state"`
			Questions map[string]struct {
				Type     string         `json:"type"`
				Criteria map[string]any `json:"criteria"`
			} `json:"questions"`
		}
		if err = json.Unmarshal(body, &request); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		target, p := "none", .99
		if strings.HasPrefix(request.State.Input, "correct") {
			target = "last"
		}
		if strings.HasPrefix(request.State.Input, "weak follow") {
			target = "last"
			p = .7
		}
		if strings.HasPrefix(request.State.Input, "follow yesterday") {
			for _, entry := range request.State.Entries {
				if entry.Date == yesterday {
					target = entry.ID
					break
				}
			}
		}
		var other []string
		for key := range request.Questions["target"].Criteria {
			if key != target {
				other = append(other, key)
			}
		}
		newTag := 0.0
		if strings.HasPrefix(request.State.Input, "new tag") {
			newTag = .99
		}
		answers := map[string]systemone.Answer{"target": jevtest.Choice(target, p, .01, other...), "new_tag": jevtest.Noul(newTag)}
		for key := range request.Questions {
			if strings.HasPrefix(key, "#") {
				answers[key] = jevtest.Noul(.9)
			}
		}
		result := jevtest.Result{Response: systemone.Response{Model: "scripted-e2e", Answers: answers, Usage: systemone.Usage{InputTokens: 12}}}
		if strings.HasPrefix(request.State.Input, "fail routing") {
			result = jevtest.Result{Err: &systemone.Error{Kind: systemone.KindRejected}}
		}
		if strings.HasPrefix(request.State.Input, "slow routing") {
			time.Sleep(1500 * time.Millisecond)
		}
		if strings.HasPrefix(request.State.Input, "fail until retried") {
			rejectedMu.Lock()
			rejected[request.State.Input]++
			failing := rejected[request.State.Input] <= failUntilRetried
			rejectedMu.Unlock()
			if failing {
				result = jevtest.Result{Err: &systemone.Error{Kind: systemone.KindRejected}}
			}
		}
		endpoint := jevtest.NewServer(t, result)
		req, err := http.NewRequest(http.MethodPost, endpoint.Settings().URL+"/v1/systemone", bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer fixture")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer jevGateway.Close()
	whisper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" || r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		defer r.MultipartForm.RemoveAll()
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "missing audio", 400)
			return
		}
		defer file.Close()
		b, _ := io.ReadAll(file)
		if strings.Contains(string(b), "unavailable fixture") {
			http.Error(w, "fixture unavailable", http.StatusServiceUnavailable)
			return
		}
		if len(b) == 0 || r.FormValue("model") != "large-v3-turbo" {
			http.Error(w, "invalid audio", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"Voice fixture thought"}`)
	}))
	defer whisper.Close()
	claude := agenttest.Session(t, agent.AgentClaude,
		agenttest.Turn{ResultText: `{"text":"new tag for a conference talk","tags":["#idea/talk"],"new_tags":[{"name":"#idea/talk","meaning":"Conference talk ideas"}]}`},
		agenttest.Turn{ResultText: `{"text":"Corrected conference talk","tags":["#idea/talk"],"new_tags":[]}`},
	)
	stateRoot := filepath.Join(dir, "root")
	if err = os.MkdirAll(stateRoot, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{"version": 1, "vault": vault,
		"listen":     map[string]any{"web": "127.0.0.1:18090", "tailnet": ""},
		"queue":      map[string]any{"max_attempts": failUntilRetried, "retry_backoff": "1s"},
		"jev":        map[string]any{"url": jevGateway.URL, "api_key_env": "SCRIBBLE_FAKE_JEV_KEY"},
		"transcribe": map[string]any{"url": whisper.URL + "/v1/audio/transcriptions"},
	}
	var encoded bytes.Buffer
	if err = toml.NewEncoder(&encoded).Encode(cfg); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(stateRoot, "config.toml"), encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// The writer runs `claude` from PATH; the scripted fake is that executable.
	// The CLI the specs run does not get it, so a doctor that finds claude and
	// the Jev key proves it ran in the service's environment.
	cmd := exec.Command(filepath.Join(repo, "bin/scribble"), "serve", "--root", stateRoot)
	cmd.Env = append(os.Environ(), "SCRIBBLE_FAKE_JEV_KEY=fixture", "PATH="+claude.Dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	if err = writeAtomic(filepath.Join(dir, "service.pid"), strconv.Itoa(cmd.Process.Pid)); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err = <-exited:
		code := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else if err != nil {
			code = -1
		}
		if werr := writeAtomic(filepath.Join(dir, "service.exit"), strconv.Itoa(code)); werr != nil {
			t.Fatal(werr)
		}
		if code != 0 {
			t.Fatalf("scribble serve exited with status %d", code)
		}
		// A requested stop: wait for Playwright to finish the run.
		<-ctx.Done()
	case <-ctx.Done():
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(15 * time.Second):
		}
	}
}

// writeAtomic replaces path with content, so a reader never sees a partial file.
func writeAtomic(path, content string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
