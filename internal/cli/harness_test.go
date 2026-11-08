package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/kpenfound/scribble/internal/api"
	"github.com/kpenfound/scribble/internal/routing"
	"github.com/kpenfound/scribble/internal/service"
	"github.com/kpenfound/scribble/internal/tailnet"
	"github.com/kpenfound/scribble/internal/vault"
)

// TestMain lets the --detach test start this test binary as the scribble
// command: the detached child is a re-execution of the running executable.
func TestMain(m *testing.M) {
	if os.Getenv("SCRIBBLE_CLI_TEST_CHILD") == "1" {
		os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// fakeRouter stands in for Jev. While ok is false every route fails, so a
// submitted thought fails and, with max_attempts = 1, lands in failed.
type fakeRouter struct{ ok atomic.Bool }

func (r *fakeRouter) Route(context.Context, string, []vault.Entry, []vault.Tag, *routing.LastWrite) (routing.Result, error) {
	if !r.ok.Load() {
		return routing.Result{}, errors.New("fake jev is down")
	}
	return routing.Result{Target: "none", Probability: .99, Tags: map[string]float64{}}, nil
}

// testEnv is an environment with the given variables and executables.
func testEnv(vars map[string]string, executables ...string) environment {
	return environment{
		Getenv: func(k string) string { return vars[k] },
		LookPath: func(name string) (string, error) {
			for _, e := range executables {
				if e == name {
					return "/service/bin/" + name, nil
				}
			}
			return "", errors.New("not found")
		},
		HTTP: &http.Client{Timeout: 2 * time.Second},
	}
}

// shortDir makes a directory short enough for a Unix socket path.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "scr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// instance is one running service in the test process.
type instance struct {
	t      *testing.T
	root   string
	vault  string
	web    string
	app    *app
	router *fakeRouter
	cancel context.CancelFunc
	done   chan int
	exited chan struct{} // closed once serve has returned, however done was read
}

type options struct {
	web     bool
	join    tailnet.Join // non-nil turns the tailnet on with this fake
	env     environment
	transcr string
}

func defaultOptions() options {
	return options{env: testEnv(map[string]string{"TYPESAFE_API_KEY": "fake"}, "claude")}
}

func writeConfig(t *testing.T, root string, cfg map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.toml"), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func baseConfig(vaultDir, web string, tailnetName string) map[string]any {
	return map[string]any{
		"version": 1, "vault": vaultDir,
		"listen": map[string]any{"web": web, "tailnet": tailnetName},
		"queue":  map[string]any{"max_attempts": 1, "retry_backoff": "1ms"},
	}
}

func startService(t *testing.T, o options) *instance {
	t.Helper()
	in := &instance{t: t, root: shortDir(t), vault: t.TempDir(), router: &fakeRouter{}, done: make(chan int, 1), exited: make(chan struct{})}
	cfg := baseConfig(in.vault, "", "")
	if o.web {
		in.web = freePort(t)
		cfg["listen"].(map[string]any)["web"] = in.web
	}
	if o.join != nil {
		cfg["listen"].(map[string]any)["tailnet"] = "scribble"
	}
	if o.transcr != "" {
		cfg["transcribe"] = map[string]any{"url": o.transcr}
	}
	writeConfig(t, in.root, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	in.cancel = cancel
	a := newApp(ctx, io.Discard, io.Discard)
	a.logOut = io.Discard
	a.env = o.env
	a.tailnetRetry = time.Hour
	if o.join != nil {
		a.join = func(*slog.Logger) tailnet.Join { return o.join }
	}
	a.tune = func(s *service.Service) { s.Router = in.router }
	in.app = a
	go func() {
		code := a.run([]string{"serve", "--root", in.root})
		in.done <- code
		close(in.exited)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-in.exited:
		case <-time.After(15 * time.Second):
			t.Error("service did not stop at cleanup")
		}
	})
	in.waitAnswering()
	return in
}

func (in *instance) sock() string { return filepath.Join(in.root, "scribble.sock") }

func (in *instance) waitAnswering() {
	in.t.Helper()
	c := newClient(in.sock(), time.Second)
	waitFor(in.t, "service to answer", func() bool {
		select {
		case code := <-in.done:
			in.t.Fatalf("service exited with %d while starting", code)
		default:
		}
		return c.do(context.Background(), http.MethodGet, api.Prefix+"/health", nil) == nil
	})
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// cli runs one scribble command as a separate process image would: its own
// app, its own environment, nothing shared with the service but the socket.
func cli(t *testing.T, env environment, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	a := newApp(context.Background(), &out, &errb)
	a.env = env
	a.stopTimeout = 10 * time.Second
	code = a.run(args)
	return code, out.String(), errb.String()
}

var noClaudeEnv = testEnv(map[string]string{})

func (in *instance) cli(args ...string) (int, string, string) {
	in.t.Helper()
	return cli(in.t, noClaudeEnv, append(args, "--root", in.root)...)
}

// rawClient sends request bodies over the service's socket.
func (in *instance) submit(id, text string) {
	in.t.Helper()
	body := fmt.Sprintf(`{"id":%q,"captured_at":%q,"text":%q}`, id, time.Now().Format(time.RFC3339), text)
	req, _ := http.NewRequest(http.MethodPost, "http://scribble"+api.Prefix+"/thoughts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := newClient(in.sock(), 5*time.Second).http.Do(req)
	if err != nil {
		in.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		in.t.Fatalf("submit: %s %s", resp.Status, b)
	}
}

func (in *instance) job(id string) api.Job {
	in.t.Helper()
	var j api.Job
	if err := newClient(in.sock(), 5*time.Second).do(context.Background(), http.MethodGet, api.Prefix+"/thoughts/"+id, &j); err != nil {
		in.t.Fatal(err)
	}
	return j
}

func (in *instance) waitState(id, state string) api.Job {
	in.t.Helper()
	var j api.Job
	waitFor(in.t, id+" to be "+state, func() bool { j = in.job(id); return j.State == state })
	return j
}

func decode[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, s)
	}
	return v
}

const (
	id1 = "00000000-0000-4000-8000-000000000001"
	id2 = "00000000-0000-4000-8000-000000000002"
)

func writeConfigAt(t *testing.T, path string, cfg map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func decodeBody(resp *http.Response, v any) error {
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}
