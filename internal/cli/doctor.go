package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/kpenfound/scribble/internal/api"
	"github.com/kpenfound/scribble/internal/config"
	"github.com/kpenfound/scribble/internal/service"
	"github.com/kpenfound/scribble/internal/vault"
)

// Where a doctor report was produced.
const (
	// SourceService: the checks ran inside the running service, with its PATH,
	// environment and network.
	SourceService = "service"
	// SourceLocal: no service answered, so the checks ran in the CLI process.
	SourceLocal = "local"
)

// Check statuses.
const (
	StatusOK   = "ok"
	StatusWarn = "warn"
	StatusFail = "fail"
)

// Check is the result of one doctor check.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// DoctorReport is the body of the doctor control and the --json output of
// `scribble doctor`.
type DoctorReport struct {
	// Source is service or local.
	Source string `json:"source"`
	// Socket is the socket the CLI asked, for a local report.
	Socket string `json:"socket,omitempty"`
	// OK is false when any check failed.
	OK     bool    `json:"ok"`
	Checks []Check `json:"checks"`
}

// runDoctor runs every check against c in env. The service calls it in its own
// process; the CLI calls it for the local fallback, so both report the same
// checks. tail is the service's tailnet node, nil outside the service.
func runDoctor(ctx context.Context, source string, c config.Config, env environment, tail service.TailnetStatuser) DoctorReport {
	checks := []Check{
		checkVault(c),
		checkDailyNotes(c),
		checkClaude(env),
		checkJevKey(c, env),
		checkWhisper(ctx, c, env),
		checkTailnet(c, env, tail),
	}
	r := DoctorReport{Source: source, OK: true, Checks: checks}
	for _, ch := range checks {
		if ch.Status == StatusFail {
			r.OK = false
		}
	}
	return r
}

func checkVault(c config.Config) Check {
	ch := Check{Name: "vault"}
	info, err := os.Stat(c.Vault)
	switch {
	case err != nil:
		return fail(ch, "%s: %v", c.Vault, cause(err))
	case !info.IsDir():
		return fail(ch, "%s is not a directory", c.Vault)
	}
	if _, err = os.ReadDir(c.Vault); err != nil {
		return fail(ch, "%s cannot be read: %v", c.Vault, cause(err))
	}
	if err = unix.Access(c.Vault, unix.W_OK); err != nil {
		return fail(ch, "%s is not writable by this account: %v", c.Vault, err)
	}
	return ok(ch, "%s is a readable, writable directory", c.Vault)
}

func checkDailyNotes(c config.Config) Check {
	ch := Check{Name: "daily-notes.json"}
	path := filepath.Join(c.Vault, ".obsidian", "daily-notes.json")
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ok(ch, "absent; using [daily] folder %q, format %q", c.Daily.Folder, c.Daily.Format)
	}
	if err != nil {
		return fail(ch, "%s cannot be read: %v", path, cause(err))
	}
	var settings struct {
		Folder *string `json:"folder"`
		Format *string `json:"format"`
	}
	if err = json.Unmarshal(b, &settings); err != nil {
		return fail(ch, "%s is not valid JSON", path)
	}
	folder, format := c.Daily.Folder, c.Daily.Format
	if settings.Folder != nil {
		folder = *settings.Folder
	}
	if folder != "" && !filepath.IsLocal(folder) {
		return fail(ch, "folder %q escapes the vault", folder)
	}
	if settings.Format != nil && *settings.Format != "" {
		format = *settings.Format
	}
	if _, err = vault.MomentLayout(format); err != nil {
		return fail(ch, "format %q: %v", format, err)
	}
	return ok(ch, "%s sets folder %q, format %q", path, folder, format)
}

func checkClaude(env environment) Check {
	ch := Check{Name: "claude"}
	path, err := env.LookPath("claude")
	if err != nil {
		return fail(ch, "`claude` is not on PATH (%s); amending a thought and creating tags need it", env.Getenv("PATH"))
	}
	return ok(ch, "found at %s", path)
}

func checkJevKey(c config.Config, env environment) Check {
	ch := Check{Name: "jev-key"}
	if env.Getenv(c.Jev.APIKeyEnv) == "" {
		return fail(ch, "$%s is not set; routing cannot call Jev", c.Jev.APIKeyEnv)
	}
	return ok(ch, "$%s is set", c.Jev.APIKeyEnv)
}

func checkWhisper(ctx context.Context, c config.Config, env environment) Check {
	ch := Check{Name: "whisper"}
	if c.Transcribe.URL == "" {
		return warn(ch, "transcribe.url is not set; voice captures fail and retry, text capture works")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.Transcribe.URL, nil)
	if err != nil {
		return fail(ch, "transcribe.url is not a usable URL")
	}
	resp, err := env.HTTP.Do(req)
	if err != nil {
		return fail(ch, "%s is not reachable: %v", c.Transcribe.URL, unwrapURL(err))
	}
	resp.Body.Close()
	// Any HTTP answer shows the host network reaches the server; a transcription
	// endpoint may well refuse HEAD.
	return ok(ch, "%s answered HTTP %d", c.Transcribe.URL, resp.StatusCode)
}

func checkTailnet(c config.Config, env environment, tail service.TailnetStatuser) Check {
	ch := Check{Name: "tailnet"}
	if c.Listen.Tailnet == "" {
		return ok(ch, "disabled (listen.tailnet is empty)")
	}
	if tail == nil {
		// The node lives in the service; the CLI can only look at its inputs.
		detail := fmt.Sprintf("listen.tailnet = %q, which only a running service can check", c.Listen.Tailnet)
		if entries, err := os.ReadDir(c.Paths.Tailnet); err == nil && len(entries) > 0 {
			return warn(ch, "%s; node state exists in %s", detail, c.Paths.Tailnet)
		}
		if env.Getenv("TS_AUTHKEY") == "" {
			return warn(ch, "%s; no node state in %s and TS_AUTHKEY is not set for the first login", detail, c.Paths.Tailnet)
		}
		return warn(ch, "%s; TS_AUTHKEY is set for the first login", detail)
	}
	st := tail.TailnetStatus()
	switch st.State {
	case api.TailnetRunning:
		return ok(ch, "running at %s", st.URL)
	case api.TailnetDisabled:
		return ok(ch, "disabled")
	case api.TailnetDegraded:
		return fail(ch, "degraded: %s", st.Reason)
	}
	return warn(ch, "%s", st.State)
}

func ok(c Check, format string, args ...any) Check   { return result(c, StatusOK, format, args...) }
func warn(c Check, format string, args ...any) Check { return result(c, StatusWarn, format, args...) }
func fail(c Check, format string, args ...any) Check { return result(c, StatusFail, format, args...) }
func result(c Check, status, format string, args ...any) Check {
	c.Status, c.Detail = status, fmt.Sprintf(format, args...)
	return c
}

// doctor implements `scribble doctor`.
func (a *app) doctor(args []string) error {
	f, _, c, paths, err := a.open("doctor", args, 0)
	if err != nil {
		return err
	}
	var rep DoctorReport
	err = c.do(a.ctx, http.MethodGet, ControlDoctor, &rep)
	var down *unavailableError
	if errors.As(err, &down) {
		cfg, lerr := config.Load(paths)
		if lerr != nil {
			return f.fail(fmt.Errorf("no service answers on %s and the local checks need the config: %w", paths.Socket, lerr))
		}
		rep = runDoctor(a.ctx, SourceLocal, cfg, a.env, nil)
		rep.Socket = paths.Socket
	} else if err != nil {
		return f.fail(err)
	}
	if f.json {
		a.writeJSON(a.stdout, rep)
	} else {
		a.printDoctor(rep, c.socket)
	}
	if !rep.OK {
		return &exitError{code: ExitFailure}
	}
	return nil
}

func (a *app) printDoctor(r DoctorReport, socket string) {
	if r.Source == SourceLocal {
		fmt.Fprintf(a.stdout, "Doctor (local): no service answers on %s.\nThese checks ran in this process, with its PATH, environment and network, not in a running service.\n", socket)
	} else {
		fmt.Fprintf(a.stdout, "Doctor (service): checks ran inside the service on %s, with its PATH, environment and network.\n", socket)
	}
	for _, ch := range r.Checks {
		fmt.Fprintf(a.stdout, "  %-4s  %-17s %s\n", ch.Status, ch.Name, strings.TrimSpace(ch.Detail))
	}
	if r.OK {
		fmt.Fprintln(a.stdout, "All checks passed.")
	} else {
		fmt.Fprintln(a.stdout, "Some checks failed.")
	}
}

// cause strips the operation and path from a file error, which the detail
// already names.
func cause(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
