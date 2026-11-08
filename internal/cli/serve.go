package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/kpenfound/scribble/internal/api"
	"github.com/kpenfound/scribble/internal/config"
	"github.com/kpenfound/scribble/internal/service"
	"github.com/kpenfound/scribble/internal/tailnet"
	"github.com/kpenfound/scribble/internal/web"
)

// serve implements `scribble serve [--detach] [--root PATH]`.
func (a *app) serve(args []string) error {
	f := a.newFlags("serve", false)
	detach := f.fs.Bool("detach", false, "run in the background, logging to <root>/service.log")
	pos, err := a.parse(f, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErrorf("serve takes no arguments, got %q", pos[0])
	}
	paths, err := f.resolve()
	if err != nil {
		return err
	}
	c, err := config.Load(paths)
	if err != nil {
		return err
	}
	if *detach {
		return a.detach(f.root, c)
	}
	var level slog.Level
	if err = level.UnmarshalText([]byte(c.LogLevel)); err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(a.logOut, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(a.ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return a.runService(ctx, c, log)
}

// runService serves c until ctx ends, the worker fails, a listener fails or
// the shutdown control fires. A signal and the shutdown control take the same
// path: stop the listeners, then wait for Service.Run.
func (a *app) runService(parent context.Context, c config.Config, log *slog.Logger) (result error) {
	svc, err := service.NewService(c, log)
	if err != nil {
		return err
	}
	var closeOnce sync.Once
	closeService := func() { closeOnce.Do(func() { _ = svc.Close() }) }
	defer closeService()

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// The embedded node only listens; a failure degrades it and nothing else.
	node := tailnet.New(tailnet.Config{
		Hostname: c.Listen.Tailnet, Dir: c.Paths.Tailnet, HTTPS: c.Listen.TailnetHTTPS,
		Join: a.join(log), Logger: log, RetryInterval: a.tailnetRetry,
	})
	svc.Tailnet = node
	if a.tune != nil {
		a.tune(svc)
	}

	newServer := func(h http.Handler) *http.Server {
		return &http.Server{
			Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second,
			IdleTimeout: 90 * time.Second, MaxHeaderBytes: 16 << 10,
			// Shutdown ends open event streams through their request contexts.
			BaseContext: func(net.Listener) context.Context { return ctx },
		}
	}
	type listening struct {
		name   string
		server *http.Server
		ln     net.Listener
	}
	var servers []listening
	closeListeners := func() {
		for _, s := range servers {
			_ = s.ln.Close()
		}
	}

	sock, err := listenSocket(c.Paths.Socket)
	if err != nil {
		return err
	}
	servers = append(servers, listening{"socket", newServer(svc.SocketHandler(a.controls(c, svc, cancel)...)), sock})
	if c.Listen.Web != "" {
		ln, err := net.Listen("tcp", c.Listen.Web)
		if err != nil {
			closeListeners()
			_ = os.Remove(c.Paths.Socket)
			return fmt.Errorf("listen.web: %w", err)
		}
		servers = append(servers, listening{"web", newServer(svc.WebHandler(web.Handler(), service.LoopbackHosts(c.Listen.Web))), ln})
	}

	worker := make(chan error, 1)
	go func() { worker <- svc.Run(ctx) }()
	nodeDone := make(chan struct{})
	go func() {
		defer close(nodeDone)
		node.Run(ctx, func(hosts []string) http.Handler { return svc.WebHandler(web.Handler(), hosts) })
	}()
	serveErr := make(chan error, len(servers))
	for _, s := range servers {
		log.Info("service.listening", "listener", s.name, "address", s.ln.Addr().String())
		go func() { serveErr <- s.server.Serve(s.ln) }()
	}

	workerDone := false
	select {
	case <-ctx.Done():
	case result = <-worker:
		workerDone = true
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			result = err
		}
	}
	// The same ordering for a signal, a shutdown request and a failure:
	// stop the worker's context, close the listeners, wait for Service.Run.
	cancel()
	shutdown, stopWaiting := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopWaiting()
	for _, s := range servers {
		if s.server.Shutdown(shutdown) != nil {
			_ = s.server.Close()
		}
	}
	<-nodeDone
	if !workerDone {
		if err := <-worker; result == nil {
			result = err
		}
	}
	closeService()
	// The socket file goes last: `scribble stop` returns when it is gone.
	_ = os.Remove(c.Paths.Socket)
	return result
}

// listenSocket listens on the root's Unix socket, which only the account that
// owns the root may use. The caller holds the database lock, so a socket file
// that is still there is a stale one. The file is not unlinked on close: the
// service removes it as its last act.
func listenSocket(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err = os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return ln, nil
}

// detach starts `scribble serve` again in the background, logging to
// <root>/service.log, and returns once the new service answers on its socket.
func (a *app) detach(explicitRoot string, c config.Config) error {
	exe, err := a.executable()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(c.Paths.Root, 0o700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(c.Paths.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	args := []string{"serve"}
	if explicitRoot != "" {
		args = append(args, "--root", c.Paths.Root)
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	cl := newClient(c.Paths.Socket, 2*time.Second)
	deadline := time.After(a.startTimeout)
	for {
		var health api.Health
		if err = cl.do(a.ctx, http.MethodGet, api.Prefix+"/health", &health); err == nil {
			fmt.Fprintf(a.stdout, "scribble service started (pid %d), logging to %s\n", cmd.Process.Pid, c.Paths.Log)
			return nil
		}
		select {
		case waitErr := <-exited:
			return fmt.Errorf("the service exited while starting (%v); see %s", waitErr, c.Paths.Log)
		case <-deadline:
			return fmt.Errorf("the service did not answer on %s within %s; see %s", c.Paths.Socket, a.startTimeout, c.Paths.Log)
		case <-a.ctx.Done():
			return a.ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
