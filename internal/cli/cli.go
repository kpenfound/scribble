// Package cli implements the scribble command: serve runs the service, and
// stop, status, retry, undo, config and doctor talk to a running service over
// its Unix socket <root>/scribble.sock.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"time"
	_ "time/tzdata" // the configured time zone must resolve on minimal hosts

	"github.com/kpenfound/scribble/internal/api"
	"github.com/kpenfound/scribble/internal/buildinfo"
	"github.com/kpenfound/scribble/internal/config"
	"github.com/kpenfound/scribble/internal/service"
	"github.com/kpenfound/scribble/internal/tailnet"
)

// Exit codes, as osmia's CLI: usage errors are 2, a request the service
// refused as invalid or about something that does not exist is 4, and a
// conflict, an internal failure or no service to talk to is 5.
const (
	ExitOK      = 0
	ExitUsage   = 2
	ExitInvalid = 4
	ExitFailure = 5
)

const usage = `usage: scribble <command> [flags]

commands:
  serve [--detach] [--root PATH]   run the service (--detach: in the background, logging to <root>/service.log)
  stop [--root PATH] [--json]      shut the running service down and wait until it has gone
  status [--root PATH] [--json]    health, tailnet, today, pending and failed thoughts, the last write
  retry <id> [--root PATH] [--json] queue a failed thought again
  undo <id> [--root PATH] [--json]  undo the most recent write
  config [--root PATH] [--json]    the configuration the running service loaded
  doctor [--root PATH] [--json]    check the vault, claude, keys, Whisper and the tailnet
  --version                        print the build information

Every command except serve and --version reaches the service on
<root>/scribble.sock. Without --root that is the default root,
$XDG_DATA_HOME/scribble (~/.local/share/scribble).
`

// Run executes one scribble command line, args without the program name, and
// returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	return newApp(context.Background(), stdout, stderr).run(args)
}

// environment is the process environment the doctor checks read. The service
// runs them with its own; the local fallback runs them with the CLI's. Tests
// give each a different one.
type environment struct {
	Getenv   func(string) string
	LookPath func(string) (string, error)
	HTTP     *http.Client
}

func hostEnvironment() environment {
	return environment{Getenv: os.Getenv, LookPath: exec.LookPath, HTTP: &http.Client{Timeout: 5 * time.Second}}
}

type app struct {
	ctx            context.Context
	stdout, stderr io.Writer
	// logOut receives the serve command's JSON log. It is stdout, which
	// systemd captures and a detached service points at service.log.
	logOut io.Writer
	env    environment

	// join starts the tailnet node. Tests substitute a fake.
	join func(*slog.Logger) tailnet.Join
	// tune runs on the new service before it starts. Tests use it to replace
	// the model clients with fakes.
	tune func(*service.Service)
	// executable is the binary --detach starts.
	executable func() (string, error)
	// tailnetRetry is the tailnet node's restart interval; zero keeps its default.
	tailnetRetry time.Duration
	// stopTimeout bounds how long stop waits for the service to go.
	stopTimeout time.Duration
	// startTimeout bounds how long --detach waits for the service to answer.
	startTimeout time.Duration
}

func newApp(ctx context.Context, stdout, stderr io.Writer) *app {
	return &app{
		ctx: ctx, stdout: stdout, stderr: stderr, logOut: stdout,
		env:          hostEnvironment(),
		join:         tailnet.JoinTailscale,
		executable:   os.Executable,
		stopTimeout:  30 * time.Second,
		startTimeout: 20 * time.Second,
	}
}

// usageError is a command line the CLI cannot act on.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usageErrorf(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// errHelp is returned by flag parsing for -h and --help.
var errHelp = errors.New("help requested")

func (a *app) run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(a.stderr, usage)
		return ExitUsage
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "--version", "-version", "version":
		fmt.Fprintf(a.stdout, "scribble %s\n", buildinfo.String())
		return ExitOK
	case "-h", "--help", "-help", "help":
		fmt.Fprint(a.stdout, usage)
		return ExitOK
	case "serve":
		err = a.serve(rest)
	case "stop":
		err = a.stop(rest)
	case "status":
		err = a.status(rest)
	case "retry":
		err = a.mutate(rest, "retry")
	case "undo":
		err = a.mutate(rest, "undo")
	case "config":
		err = a.config(rest)
	case "doctor":
		err = a.doctor(rest)
	default:
		err = usageErrorf("unknown command %q", cmd)
	}
	return a.finish(err)
}

// finish reports err and maps it to an exit code.
func (a *app) finish(err error) int {
	if err == nil {
		return ExitOK
	}
	if errors.Is(err, errHelp) {
		return ExitOK
	}
	code := exitCode(err)
	var quiet *exitError
	if errors.As(err, &quiet) {
		return code
	}
	var jsonErr *jsonError
	if errors.As(err, &jsonErr) {
		a.writeJSON(a.stderr, jsonErr.envelope)
		return code
	}
	fmt.Fprintf(a.stderr, "scribble: %v\n", err)
	var u *usageError
	if errors.As(err, &u) {
		fmt.Fprintln(a.stderr, "run `scribble help` for usage")
	}
	return code
}

// exitError ends the command with a code after it has already printed its
// result, such as a doctor report with failed checks.
type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

// jsonError marks an error to be printed as the API error envelope, for
// commands run with --json.
type jsonError struct {
	err      error
	envelope api.ErrorResponse
}

func (e *jsonError) Error() string { return e.err.Error() }
func (e *jsonError) Unwrap() error { return e.err }

func asJSONError(err error) error {
	var apiErr *api.Error
	envelope := api.ErrorResponse{Error: api.Error{Code: api.Internal, Message: err.Error()}}
	switch {
	case errors.As(err, &apiErr):
		envelope.Error = *apiErr
	case exitCode(err) == ExitInvalid:
		envelope.Error.Code = api.Validation
	case exitCode(err) == ExitUsage:
		envelope.Error.Code = api.Validation
	case errors.As(err, new(*unavailableError)):
		envelope.Error.Code = api.Unavailable
	}
	return &jsonError{err: err, envelope: envelope}
}

// exitCode maps err to a process exit code.
func exitCode(err error) int {
	var u *usageError
	var apiErr *api.Error
	var field *config.FieldError
	var quiet *exitError
	switch {
	case err == nil:
		return ExitOK
	case errors.As(err, &quiet):
		return quiet.code
	case errors.As(err, &u):
		return ExitUsage
	case errors.As(err, &apiErr):
		return codeExit(apiErr.Code)
	case errors.As(err, &field):
		return ExitInvalid
	}
	return ExitFailure
}

func codeExit(c api.Code) int {
	switch c {
	case api.Validation, api.NotFound, api.Forbidden, api.UnsupportedMedia:
		return ExitInvalid
	}
	return ExitFailure
}

// flags are the options every service-facing command accepts.
type flags struct {
	root string
	json bool
	fs   *flag.FlagSet
}

func (a *app) newFlags(name string, withJSON bool) *flags {
	f := &flags{fs: flag.NewFlagSet("scribble "+name, flag.ContinueOnError)}
	f.fs.SetOutput(io.Discard)
	f.fs.StringVar(&f.root, "root", "", "state root `PATH` holding the service's socket (default: the XDG state root)")
	if withJSON {
		f.fs.BoolVar(&f.json, "json", false, "print JSON")
	}
	f.fs.Usage = func() {}
	return f
}

// parse reads flags anywhere among the arguments and returns the positional
// ones, so `retry <id> --json` and `retry --json <id>` both work.
func (a *app) parse(f *flags, args []string) ([]string, error) {
	var positional []string
	for {
		if err := f.fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Fprintf(a.stdout, "usage: %s [flags]\n", f.fs.Name())
				f.fs.SetOutput(a.stdout)
				f.fs.PrintDefaults()
				return nil, errHelp
			}
			return nil, usageErrorf("%v", err)
		}
		args = f.fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

// resolve returns the paths the command works under, the same way serve does.
func (f *flags) resolve() (config.Paths, error) {
	paths, err := config.ResolvePaths(f.root)
	if err != nil {
		return config.Paths{}, usageErrorf("%v", err)
	}
	return paths, nil
}
