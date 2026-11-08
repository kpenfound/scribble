package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/kpenfound/scribble/internal/config"
	"github.com/kpenfound/scribble/internal/service"
	"github.com/kpenfound/scribble/internal/web"
)

func main() {
	if err := run(); err != nil {
		slog.New(slog.NewJSONHandler(os.Stdout, nil)).Error("service.stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	root := flag.String("root", "", "state root holding config.toml (default: XDG directories)")
	flag.Parse()
	paths, err := config.ResolvePaths(*root)
	if err != nil {
		return err
	}
	c, err := config.Load(paths)
	if err != nil {
		return err
	}
	if c.Listen.Web == "" {
		return errors.New("listen.web must be set until the socket listener lands")
	}
	var level slog.Level
	if err = level.UnmarshalText([]byte(c.LogLevel)); err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)
	svc, err := service.NewService(c, log)
	if err != nil {
		return err
	}
	defer svc.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Addr: c.Listen.Web, Handler: svc.WebHandler(web.Handler(), service.LoopbackHosts(c.Listen.Web)), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 16 << 10}
	worker := make(chan error, 1)
	go func() { worker <- svc.Run(ctx) }()
	httpErr := make(chan error, 1)
	go func() { log.Info("service.listening", "address", c.Listen.Web); httpErr <- server.ListenAndServe() }()
	var result error
	workerDone := false
	select {
	case <-ctx.Done():
	case result = <-worker:
		workerDone = true
	case result = <-httpErr:
		if errors.Is(result, http.ErrServerClosed) {
			result = nil
		}
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdown)
	if !workerDone {
		if err := <-worker; result == nil {
			result = err
		}
	}
	return result
}
