package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/scribble/internal/api"
	"github.com/kpenfound/scribble/internal/buildinfo"
	"github.com/kpenfound/scribble/internal/config"
	"github.com/kpenfound/scribble/internal/queue"
	"github.com/kpenfound/scribble/internal/service"
	"github.com/kpenfound/scribble/internal/tailnet"
)

func TestVersionPrintsBuildInfo(t *testing.T) {
	version, commit, date := buildinfo.Version, buildinfo.Commit, buildinfo.Date
	t.Cleanup(func() { buildinfo.Version, buildinfo.Commit, buildinfo.Date = version, commit, date })
	buildinfo.Version, buildinfo.Commit, buildinfo.Date = "v1.2.3", "abc1234", "2026-11-06T00:00:00Z"
	code, out, _ := cli(t, noClaudeEnv, "--version")
	if code != ExitOK || strings.TrimSpace(out) != "scribble v1.2.3 (abc1234, 2026-11-06T00:00:00Z)" {
		t.Fatalf("code %d, output %q", code, out)
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	root := shortDir(t)
	for _, args := range [][]string{
		nil,
		{"frobnicate"},
		{"status", "extra"},
		{"retry"},
		{"undo", "a", "b"},
		{"stop", "--nope"},
		{"config", "--root"},
		{"serve", "--root", root, "extra"},
		{"doctor", "--root", root + "\x00"},
	} {
		code, out, errOut := cli(t, noClaudeEnv, args...)
		if code != ExitUsage {
			t.Errorf("%q: exit %d, want 2 (stdout %q, stderr %q)", args, code, out, errOut)
		}
	}
	if code, out, _ := cli(t, noClaudeEnv, "help"); code != ExitOK || !strings.Contains(out, "usage: scribble") {
		t.Errorf("help: exit %d, %q", code, out)
	}
}

func TestExitCodesForAPIFailures(t *testing.T) {
	in := startService(t, defaultOptions())
	in.submit(id1, "first")
	in.waitState(id1, "failed")

	// not_found -> 4, with the envelope under --json.
	code, _, errOut := in.cli("retry", "00000000-0000-4000-8000-0000000000ff")
	if code != ExitInvalid || !strings.Contains(errOut, "no such thought") {
		t.Fatalf("retry unknown: %d %q", code, errOut)
	}
	code, _, errOut = in.cli("undo", id1, "--json")
	if code != ExitFailure {
		t.Fatalf("undo of a failed thought: exit %d, want 5 (conflict): %q", code, errOut)
	}
	if e := decode[api.ErrorResponse](t, errOut); e.Error.Code != api.Conflict || e.Error.Message == "" {
		t.Fatalf("envelope %+v", e)
	}
	code, _, errOut = in.cli("retry", "00000000-0000-4000-8000-0000000000ff", "--json")
	if e := decode[api.ErrorResponse](t, errOut); code != ExitInvalid || e.Error.Code != api.NotFound {
		t.Fatalf("json not_found: %d %+v", code, e)
	}
	// conflict -> 5: a written thought cannot be retried.
	in.router.ok.Store(true)
	in.submit(id2, "second")
	in.waitState(id2, "written")
	if code, _, errOut = in.cli("retry", id2); code != ExitFailure || !strings.Contains(errOut, "only failed jobs") {
		t.Fatalf("retry of a written thought: %d %q", code, errOut)
	}
}

func TestUnreachableServiceExitsFive(t *testing.T) {
	root := shortDir(t)
	for _, args := range [][]string{{"status"}, {"stop"}, {"config"}, {"retry", id1}, {"undo", id1}} {
		code, _, errOut := cli(t, noClaudeEnv, append(args, "--root", root)...)
		if code != ExitFailure || !strings.Contains(errOut, "no scribble service answers") {
			t.Errorf("%v: exit %d, %q", args, code, errOut)
		}
	}
	code, _, errOut := cli(t, noClaudeEnv, "status", "--json", "--root", root)
	if e := decode[api.ErrorResponse](t, errOut); code != ExitFailure || e.Error.Code != api.Unavailable {
		t.Fatalf("json unavailable: %d %+v", code, e)
	}
}

func TestInvalidConfigExitsFour(t *testing.T) {
	root := shortDir(t)
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("version = 1\nvault = \"/v\"\nlookback_days = 9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := cli(t, noClaudeEnv, "serve", "--root", root)
	if code != ExitInvalid || !strings.Contains(errOut, "lookback_days") {
		t.Fatalf("serve: %d %q", code, errOut)
	}
	code, _, errOut = cli(t, noClaudeEnv, "doctor", "--root", root)
	if code != ExitInvalid || !strings.Contains(errOut, "lookback_days") {
		t.Fatalf("doctor: %d %q", code, errOut)
	}
}

func TestServeCreatesRootStateAndStopWaitsForTheService(t *testing.T) {
	in := startService(t, defaultOptions())
	for _, name := range []string{"scribble.sock", "scribble.db", "scribble.db.lock"} {
		if _, err := os.Stat(filepath.Join(in.root, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	info, err := os.Stat(in.sock())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v, %v", info, err)
	}
	code, out, errOut := in.cli("stop")
	if code != ExitOK || !strings.Contains(out, "stopped") {
		t.Fatalf("stop: %d %q %q", code, out, errOut)
	}
	// stop returned: the socket is gone and the database lock is free again,
	// which only happens once Service.Run has ended and the service closed.
	if _, err = os.Lstat(in.sock()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket still there: %v", err)
	}
	store, err := queue.OpenStore(filepath.Join(in.root, "scribble.db"))
	if err != nil {
		t.Fatalf("the service still holds its database: %v", err)
	}
	_ = store.Close()
	select {
	case code = <-in.done:
		if code != ExitOK {
			t.Fatalf("serve exited %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after stop")
	}
	if code, _, _ = in.cli("status"); code != ExitFailure {
		t.Fatalf("status after stop: exit %d", code)
	}
}

func TestStopUsesTheSameShutdownAsASignal(t *testing.T) {
	// A signal reaches serve as cancellation of its context; the shutdown
	// control cancels the very same context. Both end serve with success and
	// close the socket last.
	in := startService(t, defaultOptions())
	in.cancel()
	select {
	case code := <-in.done:
		if code != ExitOK {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
	if _, err := os.Lstat(in.sock()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket: %v", err)
	}
}

func TestSecondServiceOnTheSameRootIsRefused(t *testing.T) {
	in := startService(t, defaultOptions())
	code, _, errOut := cli(t, noClaudeEnv, "serve", "--root", in.root)
	if code != ExitFailure {
		t.Fatalf("exit %d, %q", code, errOut)
	}
	if code, _, _ = in.cli("status"); code != ExitOK {
		t.Fatal("the first service must keep running")
	}
}

func TestControlsAnswerOnTheSocketOnly(t *testing.T) {
	in := startService(t, options{web: true, env: defaultOptions().env})
	sock := newClient(in.sock(), 5*time.Second)
	for _, path := range []string{ControlConfig, ControlDoctor} {
		if err := sock.do(context.Background(), http.MethodGet, path, nil); err != nil {
			t.Errorf("socket %s: %v", path, err)
		}
	}
	web := &http.Client{Timeout: 5 * time.Second}
	for _, tc := range []struct{ method, path string }{
		{"GET", ControlConfig}, {"GET", ControlDoctor}, {"POST", ControlShutdown}, {"GET", ControlPrefix + "/other"},
	} {
		req, _ := http.NewRequest(tc.method, "http://"+in.web+tc.path, nil)
		resp, err := web.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var e api.ErrorResponse
		err = decodeBody(resp, &e)
		if resp.StatusCode != http.StatusNotFound || err != nil || e.Error.Code != api.NotFound {
			t.Errorf("web %s %s: %d %+v %v", tc.method, tc.path, resp.StatusCode, e, err)
		}
	}
	// The web request for shutdown did nothing.
	if err := sock.do(context.Background(), http.MethodGet, api.Prefix+"/health", nil); err != nil {
		t.Fatalf("service stopped: %v", err)
	}
	// The same handler construction the tailnet uses refuses them too.
	svc := &service.Service{}
	handler := svc.WebHandler(nil, []string{"scribble"})
	for _, path := range []string{ControlConfig, ControlDoctor, ControlShutdown} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "http://scribble"+path, nil)
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("web handler %s: %d", path, w.Code)
		}
	}
}

func TestControlsAreNotPublicRoutes(t *testing.T) {
	for _, r := range service.Routes {
		if strings.HasPrefix(r.Path, ControlPrefix) || strings.Contains(r.Path, "/control") {
			t.Errorf("service.Routes lists %s %s", r.Method, r.Path)
		}
	}
}

func TestFailingTailnetDegradesOnlyTheTailnet(t *testing.T) {
	join, call := tailnet.FakeJoin(nil, errors.New("tailnet is unreachable"))
	o := defaultOptions()
	o.web, o.join = true, join
	in := startService(t, o)
	waitFor(t, "degraded tailnet", func() bool {
		_, out, _ := in.cli("status")
		return strings.Contains(out, "Tailnet: degraded")
	})
	if n, host, dir := call.Get(); n == 0 || host != "scribble" || dir != filepath.Join(in.root, "tailnet") {
		t.Fatalf("join called %d times with %q, %q", n, host, dir)
	}
	code, out, _ := in.cli("status")
	if code != ExitOK || !strings.Contains(out, "Tailnet: degraded: join the tailnet as scribble: tailnet is unreachable") || !strings.Contains(out, "Health: ok") {
		t.Fatalf("status: %d\n%s", code, out)
	}
	// /v1/health says degraded on the socket and on listen.web.
	for name, h := range map[string]*http.Client{"socket": newClient(in.sock(), 5*time.Second).http, "web": http.DefaultClient} {
		base := "http://scribble"
		if name == "web" {
			base = "http://" + in.web
		}
		resp, err := h.Get(base + api.Prefix + "/health")
		if err != nil {
			t.Fatal(err)
		}
		var health api.Health
		if err = decodeBody(resp, &health); err != nil || health.Tailnet.State != api.TailnetDegraded || health.Tailnet.Reason == "" || health.Status != "ok" {
			t.Fatalf("%s health: %+v %v", name, health, err)
		}
	}
	// The worker still processes thoughts.
	in.router.ok.Store(true)
	in.submit(id1, "still working")
	in.waitState(id1, "written")
	// And doctor reports the failure.
	_, out, _ = in.cli("doctor")
	if !strings.Contains(out, "degraded: join the tailnet") {
		t.Fatalf("doctor: %s", out)
	}
}

func TestStatusRetryAndUndo(t *testing.T) {
	in := startService(t, defaultOptions())
	in.submit(id1, "first")
	in.waitState(id1, "failed")
	in.submit(id2, "second")
	in.waitState(id2, "failed")
	before, other := in.job(id1), in.job(id2)

	code, out, _ := in.cli("status")
	for _, want := range []string{"Health: ok", "Tailnet: disabled", "Today: ", "Pending: 0", "Failed: 2", id1, "fake jev is down", "Last write: none"} {
		if code != ExitOK || !strings.Contains(out, want) {
			t.Fatalf("status lacks %q (exit %d):\n%s", want, code, out)
		}
	}
	code, out, _ = in.cli("status", "--json")
	st := decode[statusOutput](t, out)
	if code != ExitOK || st.Health.Status != "ok" || st.Health.Tailnet.State != api.TailnetDisabled || len(st.Status.Jobs) != 2 || st.Status.Today == "" {
		t.Fatalf("status --json: %d %+v", code, st)
	}

	// retry queues the job again at the back, as the PWA's action does.
	in.router.ok.Store(true)
	code, out, _ = in.cli("retry", id1, "--json")
	if r := decode[struct{ ID, State string }](t, out); code != ExitOK || r.ID != id1 || r.State != "queued" {
		t.Fatalf("retry: %d %q", code, out)
	}
	after := in.job(id1)
	if after.Sequence <= other.Sequence || after.Sequence <= before.Sequence || after.Attempts > 1 {
		t.Fatalf("not re-queued at the back: before %+v, other %+v, after %+v", before, other, after)
	}
	in.waitState(id1, "written")
	code, out, _ = in.cli("retry", id2)
	if code != ExitOK || strings.TrimSpace(out) != id2+": queued" {
		t.Fatalf("retry text: %d %q", code, out)
	}
	written := in.waitState(id2, "written")
	note := readNote(t, in, written)
	if !strings.Contains(note, written.Line) {
		t.Fatalf("note lacks the line %q:\n%s", written.Line, note)
	}

	code, out, _ = in.cli("status")
	if !strings.Contains(out, "Last write: ") || !strings.Contains(out, id2) || !strings.Contains(out, written.Line) {
		t.Fatalf("status after writes (exit %d):\n%s", code, out)
	}
	// undo only applies to the latest write.
	if code, _, _ = in.cli("undo", id1); code != ExitFailure {
		t.Fatalf("undo of an older write: exit %d", code)
	}
	code, out, _ = in.cli("undo", id2, "--json")
	if r := decode[struct{ ID, State string }](t, out); code != ExitOK || r.State != "undone" {
		t.Fatalf("undo: %d %q", code, out)
	}
	if !in.job(id2).Undone {
		t.Fatal("job not marked undone")
	}
	if note = readNote(t, in, written); strings.Contains(note, written.Line) {
		t.Fatalf("undo left the line in the note:\n%s", note)
	}
	if _, out, _ = in.cli("status"); !strings.Contains(out, "(undone)") {
		t.Fatalf("status does not show the undo:\n%s", out)
	}
}

func readNote(t *testing.T, in *instance, j api.Job) string {
	t.Helper()
	var found string
	_ = filepath.WalkDir(in.vault, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, j.Date+".md") {
			b, _ := os.ReadFile(path)
			found = string(b)
		}
		return nil
	})
	if found == "" {
		t.Fatalf("no daily note for %s under %s", j.Date, in.vault)
	}
	return found
}

func TestConfigShowsTheLoadedConfigurationNotTheFile(t *testing.T) {
	in := startService(t, defaultOptions())
	cfg := baseConfig(in.vault, "", "")
	cfg["lookback_days"] = 2
	cfg["log_level"] = "debug"
	writeConfig(t, in.root, cfg)

	code, out, _ := in.cli("config", "--json")
	got := decode[config.Config](t, out)
	if code != ExitOK || got.LookbackDays != 7 || got.LogLevel != "info" || got.Vault != in.vault || got.Paths.Socket != in.sock() || got.Queue.MaxAttempts != 1 {
		t.Fatalf("config --json after the file changed: %d %+v", code, got)
	}
	code, out, _ = in.cli("config")
	if code != ExitOK || !strings.Contains(out, "lookback_days = 7") || !strings.Contains(out, `log_level = "info"`) || strings.Contains(out, "lookback_days = 2") || !strings.Contains(out, in.vault) {
		t.Fatalf("config text: %d\n%s", code, out)
	}
	// The file on disk does say 2: the service did not reread it.
	if b, _ := os.ReadFile(filepath.Join(in.root, "config.toml")); !strings.Contains(string(b), "lookback_days = 2") {
		t.Fatalf("fixture: %s", b)
	}
}

func TestDoctorRunsInTheServicesEnvironment(t *testing.T) {
	whisper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "post only", http.StatusMethodNotAllowed) }))
	defer whisper.Close()
	// The service has claude on its PATH and the Jev key; the CLI has neither.
	o := options{env: testEnv(map[string]string{"TYPESAFE_API_KEY": "k", "PATH": "/service/bin"}, "claude"), transcr: whisper.URL}
	in := startService(t, o)
	if err := os.MkdirAll(filepath.Join(in.vault, ".obsidian"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(in.vault, ".obsidian", "daily-notes.json"), []byte(`{"folder":"Journal","format":"YYYY/MM/DD"}`), 0o640); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := in.cli("doctor")
	if code != ExitOK {
		t.Fatalf("doctor: %d\n%s%s", code, out, errOut)
	}
	for _, want := range []string{"Doctor (service)", "vault", "daily-notes.json", `folder "Journal"`, "claude", "found at /service/bin/claude", "jev-key", "$TYPESAFE_API_KEY is set", "whisper", "answered HTTP 405", "tailnet", "disabled"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor text lacks %q:\n%s", want, out)
		}
	}
	code, out, _ = in.cli("doctor", "--json")
	rep := decode[DoctorReport](t, out)
	if code != ExitOK || rep.Source != SourceService || !rep.OK {
		t.Fatalf("doctor --json: %d %+v", code, rep)
	}
	got := map[string]string{}
	for _, c := range rep.Checks {
		got[c.Name] = c.Status
	}
	for _, name := range []string{"vault", "daily-notes.json", "claude", "jev-key", "whisper", "tailnet"} {
		if got[name] != StatusOK {
			t.Errorf("check %s: %q", name, got[name])
		}
	}

	// The same CLI environment checked locally finds neither claude nor the key.
	if err := os.Remove(in.sock()); err != nil {
		t.Fatal(err)
	}
	code, out, _ = in.cli("doctor", "--json")
	local := decode[DoctorReport](t, out)
	if code != ExitFailure || local.Source != SourceLocal || local.OK || local.Socket != in.sock() {
		t.Fatalf("local doctor: %d %+v", code, local)
	}
	for _, c := range local.Checks {
		if (c.Name == "claude" || c.Name == "jev-key") && c.Status != StatusFail {
			t.Errorf("local %s: %+v", c.Name, c)
		}
	}
}

func TestDoctorWithoutAServiceRunsLocallyAndSaysSo(t *testing.T) {
	root, vaultDir := shortDir(t), t.TempDir()
	writeConfig(t, root, baseConfig(vaultDir, "", ""))
	env := testEnv(map[string]string{"TYPESAFE_API_KEY": "k"}, "claude")
	code, out, errOut := cli(t, env, "doctor", "--root", root)
	if code != ExitOK || !strings.Contains(out, "Doctor (local)") || !strings.Contains(out, "not in a running service") || !strings.Contains(out, "found at /service/bin/claude") {
		t.Fatalf("local doctor: %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, "warn  whisper") || !strings.Contains(out, "transcribe.url is not set") {
		t.Fatalf("whisper not configured should warn:\n%s", out)
	}
	// A missing vault and an unreachable Whisper URL fail.
	cfg := baseConfig(filepath.Join(vaultDir, "missing"), "", "")
	cfg["transcribe"] = map[string]any{"url": "http://127.0.0.1:1/v1/audio/transcriptions"}
	writeConfig(t, root, cfg)
	code, out, _ = cli(t, env, "doctor", "--root", root)
	if code != ExitFailure || !strings.Contains(out, "fail  vault") || !strings.Contains(out, "fail  whisper") || !strings.Contains(out, "Some checks failed") {
		t.Fatalf("failing doctor: %d\n%s", code, out)
	}
}

func TestDoctorDailyNotesChecks(t *testing.T) {
	c := config.DefaultConfig()
	c.Vault = t.TempDir()
	if got := checkDailyNotes(c); got.Status != StatusOK || !strings.Contains(got.Detail, "absent") {
		t.Fatalf("absent: %+v", got)
	}
	dir := filepath.Join(c.Vault, ".obsidian")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for body, want := range map[string]string{
		`{"folder":"Daily","format":"YYYY-MM-DD"}`: StatusOK,
		`{"folder":"../outside"}`:                  StatusFail,
		`{"format":"dddd"}`:                        StatusFail,
		`{not json`:                                StatusFail,
	} {
		if err := os.WriteFile(filepath.Join(dir, "daily-notes.json"), []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
		if got := checkDailyNotes(c); got.Status != want {
			t.Errorf("%s: %+v, want %s", body, got, want)
		}
	}
}

func TestTwoServicesAreSelectedByRoot(t *testing.T) {
	a := startService(t, options{env: testEnv(map[string]string{"TYPESAFE_API_KEY": "a"}, "claude")})
	b := startService(t, options{env: testEnv(map[string]string{"TYPESAFE_API_KEY": "b"})})
	a.submit(id1, "only in a")
	a.waitState(id1, "failed")

	// status
	if _, out, _ := a.cli("status"); !strings.Contains(out, "Failed: 1") || !strings.Contains(out, id1) {
		t.Fatalf("a status:\n%s", out)
	}
	if _, out, _ := b.cli("status"); !strings.Contains(out, "Failed: 0") || strings.Contains(out, id1) {
		t.Fatalf("b status:\n%s", out)
	}
	// config
	ca := decode[config.Config](t, mustOut(t, a, "config", "--json"))
	cb := decode[config.Config](t, mustOut(t, b, "config", "--json"))
	if ca.Vault != a.vault || cb.Vault != b.vault || ca.Paths.Root == cb.Paths.Root {
		t.Fatalf("configs: %q %q", ca.Vault, cb.Vault)
	}
	// doctor: a has claude and b has not.
	da := decode[DoctorReport](t, mustOut(t, a, "doctor", "--json"))
	_, bout, _ := b.cli("doctor", "--json")
	db := decode[DoctorReport](t, bout)
	if checkStatus(da, "claude") != StatusOK || checkStatus(db, "claude") != StatusFail || da.Source != SourceService || db.Source != SourceService {
		t.Fatalf("doctor a %+v\nb %+v", da, db)
	}
	// retry and undo reach only the named root.
	if code, _, _ := b.cli("retry", id1); code != ExitInvalid {
		t.Fatalf("b knows nothing of %s: exit %d", id1, code)
	}
	a.router.ok.Store(true)
	mustOut(t, a, "retry", id1)
	a.waitState(id1, "written")
	if code, _, _ := b.cli("undo", id1); code != ExitInvalid {
		t.Fatalf("undo on b: exit %d", code)
	}
	mustOut(t, a, "undo", id1)
	if !a.job(id1).Undone {
		t.Fatal("undo did not reach a")
	}
	// stop on b leaves a running.
	if code, _, errOut := b.cli("stop"); code != ExitOK {
		t.Fatalf("stop b: %d %s", code, errOut)
	}
	if _, err := os.Lstat(b.sock()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("b socket: %v", err)
	}
	if code, out, _ := a.cli("status"); code != ExitOK || !strings.Contains(out, "Health: ok") {
		t.Fatalf("a after stopping b: %d %s", code, out)
	}
	if code, _, _ := b.cli("status"); code != ExitFailure {
		t.Fatalf("b is still answering: %d", code)
	}
}

func checkStatus(r DoctorReport, name string) string {
	for _, c := range r.Checks {
		if c.Name == name {
			return c.Status
		}
	}
	return ""
}

func mustOut(t *testing.T, in *instance, args ...string) string {
	t.Helper()
	code, out, errOut := in.cli(args...)
	if code != ExitOK {
		t.Fatalf("%v: exit %d\n%s%s", args, code, out, errOut)
	}
	return out
}

func TestDefaultRootIsUsedWithoutRootFlag(t *testing.T) {
	home := shortDir(t)
	t.Setenv("XDG_DATA_HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "cfg"))
	if err := os.MkdirAll(filepath.Join(home, "cfg", "scribble"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "scribble"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig(t.TempDir(), "", "")
	writeConfigAt(t, filepath.Join(home, "cfg", "scribble", "config.toml"), cfg)

	ctx, cancel := context.WithCancel(context.Background())
	a := newApp(ctx, io.Discard, io.Discard)
	a.logOut = io.Discard
	done := make(chan int, 1)
	go func() { done <- a.run([]string{"serve"}) }()
	t.Cleanup(func() { cancel(); <-done })
	sock := filepath.Join(home, "scribble", "scribble.sock")
	waitFor(t, "default root socket", func() bool {
		return newClient(sock, time.Second).do(context.Background(), http.MethodGet, api.Prefix+"/health", nil) == nil
	})
	if code, out, errOut := cli(t, noClaudeEnv, "status"); code != ExitOK || !strings.Contains(out, "Health: ok") {
		t.Fatalf("status without --root: %d %s %s", code, out, errOut)
	}
	if code, _, _ := cli(t, noClaudeEnv, "stop"); code != ExitOK {
		t.Fatal("stop without --root")
	}
}

func TestDetachRunsInTheBackgroundAndLogsToServiceLog(t *testing.T) {
	root, vaultDir := shortDir(t), t.TempDir()
	writeConfig(t, root, baseConfig(vaultDir, "", ""))
	t.Setenv("SCRIBBLE_CLI_TEST_CHILD", "1")
	code, out, errOut := cli(t, noClaudeEnv, "serve", "--detach", "--root", root)
	if code != ExitOK || !strings.Contains(out, "scribble service started") {
		t.Fatalf("detach: %d %q %q", code, out, errOut)
	}
	t.Cleanup(func() { cli(t, noClaudeEnv, "stop", "--root", root) })
	if code, out, _ = cli(t, noClaudeEnv, "status", "--root", root); code != ExitOK || !strings.Contains(out, "Health: ok") {
		t.Fatalf("status of the detached service: %d %s", code, out)
	}
	logged, err := os.ReadFile(filepath.Join(root, "service.log"))
	if err != nil || !strings.Contains(string(logged), "service.listening") {
		t.Fatalf("service.log: %v\n%s", err, logged)
	}
	if code, _, errOut = cli(t, noClaudeEnv, "stop", "--root", root); code != ExitOK {
		t.Fatalf("stop: %d %s", code, errOut)
	}
	if _, err = os.Lstat(filepath.Join(root, "scribble.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket after stop: %v", err)
	}
}
