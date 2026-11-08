package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/kpenfound/scribble/internal/api"
	"github.com/kpenfound/scribble/internal/config"
)

// requestTimeout bounds one control request.
const requestTimeout = 30 * time.Second

// fail wraps err for --json: the error then prints as the API envelope.
func (f *flags) fail(err error) error {
	if err == nil || !f.json {
		return err
	}
	return asJSONError(err)
}

func (a *app) writeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// open parses a command line that takes no positional arguments, or exactly
// want of them, and returns the client for the service it names.
func (a *app) open(name string, args []string, want int) (*flags, []string, *client, config.Paths, error) {
	f := a.newFlags(name, true)
	pos, err := a.parse(f, args)
	if err != nil {
		return f, nil, nil, config.Paths{}, err
	}
	if len(pos) != want {
		return f, nil, nil, config.Paths{}, f.fail(usageErrorf("%s takes %d argument(s), got %d", name, want, len(pos)))
	}
	paths, err := f.resolve()
	if err != nil {
		return f, nil, nil, paths, f.fail(err)
	}
	return f, pos, newClient(paths.Socket, requestTimeout), paths, nil
}

func (a *app) stop(args []string) error {
	f, _, c, _, err := a.open("stop", args, 0)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(a.ctx, a.stopTimeout+requestTimeout)
	defer cancel()
	var body api.State
	if err = c.do(ctx, http.MethodPost, ControlShutdown, &body); err != nil {
		return f.fail(err)
	}
	if err = c.waitGone(ctx, a.stopTimeout); err != nil {
		return f.fail(err)
	}
	if f.json {
		a.writeJSON(a.stdout, api.State{State: "stopped"})
	} else {
		fmt.Fprintf(a.stdout, "scribble service on %s stopped\n", c.socket)
	}
	return nil
}

// statusOutput is the --json shape of `scribble status`.
type statusOutput struct {
	Health api.Health         `json:"health"`
	Status api.StatusResponse `json:"status"`
}

func (a *app) status(args []string) error {
	f, _, c, _, err := a.open("status", args, 0)
	if err != nil {
		return err
	}
	var out statusOutput
	if err = c.do(a.ctx, http.MethodGet, api.Prefix+"/health", &out.Health); err != nil {
		return f.fail(err)
	}
	if err = c.do(a.ctx, http.MethodGet, api.Prefix+"/status", &out.Status); err != nil {
		return f.fail(err)
	}
	if f.json {
		a.writeJSON(a.stdout, out)
		return nil
	}
	a.printStatus(out)
	return nil
}

func (a *app) printStatus(s statusOutput) {
	w := a.stdout
	fmt.Fprintf(w, "Health: %s\n", s.Health.Status)
	fmt.Fprintf(w, "Version: %s\n", s.Health.Build.Version)
	fmt.Fprintf(w, "Tailnet: %s\n", tailnetLine(s.Health.Tailnet))
	fmt.Fprintf(w, "Today: %s\n", s.Status.Today)
	var pending, failed []api.Job
	var last *api.Job
	for i, j := range s.Status.Jobs {
		switch j.State {
		case "failed":
			failed = append(failed, j)
		case "written":
			last = &s.Status.Jobs[i]
		default:
			pending = append(pending, j)
		}
	}
	fmt.Fprintf(w, "Pending: %d\n", len(pending))
	for _, j := range pending {
		fmt.Fprintf(w, "  %s  %s\n", j.ID, j.State)
	}
	fmt.Fprintf(w, "Failed: %d\n", len(failed))
	for _, j := range failed {
		fmt.Fprintf(w, "  %s  attempts %d: %s\n", j.ID, j.Attempts, j.Error)
	}
	if last == nil {
		fmt.Fprintln(w, "Last write: none")
		return
	}
	undone := ""
	if last.Undone {
		undone = " (undone)"
	}
	fmt.Fprintf(w, "Last write: %s %s %s%s\n  %s\n", last.WrittenAt, last.Action, last.ID, undone, last.Line)
}

func tailnetLine(t api.TailnetStatus) string {
	line := t.State
	if t.Reason != "" {
		line += ": " + t.Reason
	}
	if t.URL != "" {
		line += " " + t.URL
	}
	return line
}

// mutate implements retry and undo, which call the public routes the PWA uses.
func (a *app) mutate(args []string, action string) error {
	f, pos, c, _, err := a.open(action, args, 1)
	if err != nil {
		return err
	}
	id := pos[0]
	if strings.TrimSpace(id) == "" {
		return f.fail(usageErrorf("%s needs a job id", action))
	}
	var body api.State
	path := api.Prefix + "/thoughts/" + url.PathEscape(id) + "/" + action
	if err = c.do(a.ctx, http.MethodPost, path, &body); err != nil {
		return f.fail(err)
	}
	if f.json {
		a.writeJSON(a.stdout, struct {
			ID    string `json:"id"`
			State string `json:"state"`
		}{id, body.State})
		return nil
	}
	fmt.Fprintf(a.stdout, "%s: %s\n", id, body.State)
	return nil
}

func (a *app) config(args []string) error {
	f, _, c, _, err := a.open("config", args, 0)
	if err != nil {
		return err
	}
	var loaded config.Config
	var raw json.RawMessage
	if err = c.do(a.ctx, http.MethodGet, ControlConfig, &raw); err != nil {
		return f.fail(err)
	}
	if f.json {
		var out bytes.Buffer
		if err = json.Indent(&out, raw, "", "  "); err != nil {
			return f.fail(err)
		}
		fmt.Fprintln(a.stdout, out.String())
		return nil
	}
	if err = json.Unmarshal(raw, &loaded); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "# loaded by the running service from %s\n# state root: %s\n# editing the file changes nothing until the service restarts\n", loaded.Paths.Config, loaded.Paths.Root)
	return toml.NewEncoder(a.stdout).Encode(loaded)
}
