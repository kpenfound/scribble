// Package tailnet runs scribble's embedded Tailscale node. The node is used
// only to listen: it accepts the PWA's connections from the owner's tailnet.
// Nothing in scribble dials out through it; Jev, Claude and Whisper traffic
// uses the host's network stack.
//
// A tailnet failure never reaches the caller as an error. Node.Run reports
// it as a degraded state with a reason, which the service's /v1/health and
// `scribble status` show, and keeps retrying while the rest of the service
// runs.
package tailnet

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/scribble/internal/api"
)

// Status is what the backend knows about the embedded node.
type Status struct {
	// Backend is Tailscale's backend state: Running, NeedsLogin,
	// NeedsMachineAuth, Stopped, Starting and so on.
	Backend string
	// AuthURL is the login URL shown while the node waits for approval.
	AuthURL string
	// DNSName is the node's full MagicDNS name, such as
	// scribble.tail1234.ts.net., once it has one.
	DNSName string
	// CertDomains lists the names the tailnet issues HTTPS certificates for.
	CertDomains []string
}

// Backend is one joined tailnet node. It only listens.
type Backend interface {
	// Listen accepts connections on addr, such as ":443", from the tailnet.
	Listen(addr string) (net.Listener, error)
	// Status reports the node's state.
	Status(context.Context) (Status, error)
	// TLSConfig fetches the certificate for name ahead of serving, so an
	// issuance failure shows as an error rather than a failed handshake, and
	// returns a TLS configuration that serves it.
	TLSConfig(ctx context.Context, name string) (*tls.Config, error)
	// Close leaves the tailnet and ends every open connection.
	Close() error
}

// Join joins the tailnet as hostname, keeping the node's state in dir.
type Join func(hostname, dir string) (Backend, error)

// Handler builds the HTTP handler for the listener. hosts lists the Host
// header values the node answers to, each as a browser sends it.
type Handler func(hosts []string) http.Handler

// Config configures a Node.
type Config struct {
	// Hostname is listen.tailnet. Empty disables the tailnet.
	Hostname string
	// Dir is the node's state directory, <root>/tailnet.
	Dir string
	// HTTPS serves on 443 with Tailscale certificates when true, and plain
	// HTTP on 80 otherwise (listen.tailnet_https).
	HTTPS bool
	// Join starts the node; JoinTailscale for the real one.
	Join Join
	// Logger receives the node's lifecycle messages. Nil discards them.
	Logger *slog.Logger
	// RetryInterval is how long Run waits before it starts a failed node
	// again. Zero means 30 seconds.
	RetryInterval time.Duration
	// PollInterval is how often Run checks the node's state. Zero means
	// 2 seconds.
	PollInterval time.Duration
}

// Node is the service's hold on its tailnet node. It reports the node's
// state, which comes and goes while the service runs, and it implements the
// service's tailnet status interface.
type Node struct {
	cfg Config

	mu     sync.Mutex
	status api.TailnetStatus
}

// New returns a Node. With an empty hostname it is disabled and Run returns
// at once.
func New(cfg Config) *Node {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.RetryInterval <= 0 {
		cfg.RetryInterval = 30 * time.Second
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	n := &Node{cfg: cfg}
	if cfg.Hostname == "" {
		n.status = api.TailnetStatus{State: api.TailnetDisabled}
	} else {
		n.status = api.TailnetStatus{State: api.TailnetStarting}
	}
	return n
}

// TailnetStatus reports the listener's state. A nil Node is disabled.
func (n *Node) TailnetStatus() api.TailnetStatus {
	if n == nil {
		return api.TailnetStatus{State: api.TailnetDisabled}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.status
}

func (n *Node) set(s api.TailnetStatus) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.status = s
}

func (n *Node) degraded(reason string) {
	n.cfg.Logger.Warn("tailnet degraded", "component", "tailnet", "reason", reason)
	n.set(api.TailnetStatus{State: api.TailnetDegraded, Reason: reason})
}

// Run joins the tailnet and serves handler until ctx ends. It returns nothing:
// a failure to start, to get a certificate or to stay connected is reported
// through TailnetStatus as degraded with a reason, and Run starts the node
// again after RetryInterval. Run is meant to run in its own goroutine next to
// the service's other listeners, which a tailnet failure must not stop.
func (n *Node) Run(ctx context.Context, handler Handler) {
	if n == nil || n.cfg.Hostname == "" {
		return
	}
	for ctx.Err() == nil {
		if err := n.runOnce(ctx, handler); err != nil && ctx.Err() == nil {
			n.degraded(err.Error())
		}
		select {
		case <-ctx.Done():
		case <-time.After(n.cfg.RetryInterval):
		}
	}
}

// runOnce joins the tailnet, serves while the node holds, and returns why
// it stopped: nil when ctx ended.
func (n *Node) runOnce(ctx context.Context, handler Handler) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("tailnet panic: %v", r)
		}
	}()
	if n.cfg.Join == nil {
		return errors.New("no tailnet implementation configured")
	}
	n.set(api.TailnetStatus{State: api.TailnetStarting})
	if err := os.MkdirAll(n.cfg.Dir, 0o700); err != nil {
		return fmt.Errorf("create tailnet state directory: %w", err)
	}
	backend, err := n.cfg.Join(n.cfg.Hostname, n.cfg.Dir)
	if err != nil {
		return fmt.Errorf("join the tailnet as %s: %w", n.cfg.Hostname, err)
	}
	defer backend.Close()

	st, err := n.waitRunning(ctx, backend)
	if err != nil || st == nil {
		return err
	}
	name := strings.TrimSuffix(st.DNSName, ".")
	hosts := n.hosts(name)

	var ln net.Listener
	scheme := "http"
	if n.cfg.HTTPS {
		scheme = "https"
		ln, err = n.listenHTTPS(ctx, backend, st, name)
	} else {
		ln, err = backend.Listen(":80")
		if err != nil {
			err = fmt.Errorf("listen on the tailnet: %w", err)
		}
	}
	if err != nil {
		return err
	}

	srv := &http.Server{Handler: handler(hosts), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if srv.Shutdown(shutdown) != nil {
			srv.Close()
		}
	}()

	url := scheme + "://" + name
	if name == "" {
		url = scheme + "://" + n.cfg.Hostname
	}
	n.cfg.Logger.Info("tailnet serving", "component", "tailnet", "url", url)
	n.set(api.TailnetStatus{State: api.TailnetRunning, URL: url})

	tick := time.NewTicker(n.cfg.PollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-served:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return fmt.Errorf("tailnet listener stopped: %w", err)
		case <-tick.C:
			// A node that drops stays degraded while it keeps its
			// listener, and reports running again when it recovers.
			st, err := currentStatus(ctx, backend)
			switch {
			case ctx.Err() != nil:
				return nil
			case err != nil:
				n.degradedKeep(url, err.Error())
			case st.Backend != "Running":
				n.degradedKeep(url, backendReason(st))
			default:
				n.set(api.TailnetStatus{State: api.TailnetRunning, URL: url})
			}
		}
	}
}

// degradedKeep records a degraded state for a node that is serving, keeping
// the app's URL so the status still says where it was.
func (n *Node) degradedKeep(url, reason string) {
	n.mu.Lock()
	changed := n.status.State != api.TailnetDegraded || n.status.Reason != reason
	n.status = api.TailnetStatus{State: api.TailnetDegraded, Reason: reason, URL: url}
	n.mu.Unlock()
	if changed {
		n.cfg.Logger.Warn("tailnet degraded", "component", "tailnet", "reason", reason)
	}
}

func currentStatus(ctx context.Context, b Backend) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return b.Status(ctx)
}

// waitRunning polls until the node is up. While it waits for a login or an
// approval the state is degraded with the reason, including the login URL,
// so the owner can act on it; the wait ends when ctx does, and then returns
// a nil status.
func (n *Node) waitRunning(ctx context.Context, b Backend) (*Status, error) {
	tick := time.NewTicker(n.cfg.PollInterval)
	defer tick.Stop()
	for {
		st, err := currentStatus(ctx, b)
		switch {
		case ctx.Err() != nil:
			return nil, nil
		case err != nil:
			return nil, fmt.Errorf("query the tailnet node: %w", err)
		case st.Backend == "Running" && st.DNSName != "":
			return &st, nil
		case st.Backend == "Running", st.Backend == "":
			n.set(api.TailnetStatus{State: api.TailnetStarting})
		case reasonKnown(st.Backend):
			n.degradedKeep("", backendReason(st))
		default:
			n.set(api.TailnetStatus{State: api.TailnetStarting})
		}
		select {
		case <-ctx.Done():
			return nil, nil
		case <-tick.C:
		}
	}
}

// listenHTTPS opens TLS on port 443 with a certificate for the node's full
// MagicDNS name. Anything that stops that is an error, so the node reports
// degraded with the reason rather than serving the PWA in the clear.
func (n *Node) listenHTTPS(ctx context.Context, b Backend, st *Status, name string) (net.Listener, error) {
	if !strings.HasSuffix(name, ".ts.net") {
		return nil, errors.New("the node has no full MagicDNS name for HTTPS")
	}
	if !containsFold(st.CertDomains, name) {
		return nil, errors.New("enable HTTPS certificates in the tailnet admin console for " + name)
	}
	// Certificate issuance can take a while on first use.
	certCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cfg, err := b.TLSConfig(certCtx, name)
	if err != nil {
		return nil, fmt.Errorf("certificate for %s: %w", name, err)
	}
	ln, err := b.Listen(":443")
	if err != nil {
		return nil, fmt.Errorf("listen on the tailnet: %w", err)
	}
	return tls.NewListener(ln, cfg), nil
}

// hosts lists the Host header values the node serves: its MagicDNS name, the
// short name and the configured hostname.
func (n *Node) hosts(name string) []string {
	var hosts []string
	add := func(h string) {
		h = strings.ToLower(h)
		if h != "" && !containsFold(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	add(name)
	short, _, _ := strings.Cut(name, ".")
	add(short)
	add(n.cfg.Hostname)
	return hosts
}

// reasonKnown reports whether a backend state is one that needs the owner's
// action or is stopped, which is degraded, rather than a node still
// connecting.
func reasonKnown(backend string) bool {
	switch backend {
	case "NeedsLogin", "NeedsMachineAuth", "Stopped":
		return true
	}
	return false
}

// backendReason says why a node that is not running is degraded.
func backendReason(st Status) string {
	switch st.Backend {
	case "NeedsLogin":
		if st.AuthURL != "" {
			return "the node needs login: open " + st.AuthURL
		}
		return "the node needs login: set TS_AUTHKEY and restart"
	case "NeedsMachineAuth":
		return "the node awaits approval on the tailnet"
	case "Stopped":
		return "the node is stopped"
	}
	if st.Backend == "" {
		return "the node reports no state"
	}
	return "the node is " + strings.ToLower(st.Backend)
}

func containsFold(names []string, host string) bool {
	for _, name := range names {
		if strings.EqualFold(strings.TrimSuffix(name, "."), strings.TrimSuffix(host, ".")) {
			return true
		}
	}
	return false
}
