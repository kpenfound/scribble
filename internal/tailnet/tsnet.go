package tailnet

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"regexp"
	"strings"

	"tailscale.com/tsnet"
)

// loginURLPattern finds the login URL embedded Tailscale prints while the
// node waits for approval, so it can travel as a field of its own.
var loginURLPattern = regexp.MustCompile(`https?://\S+`)

// tailscaleLogf adapts logger to embedded Tailscale's logf signature: every
// line it logs becomes one structured record, with any URL the line mentions,
// such as the login URL shown while the node waits for approval, as its own
// field. The value of TS_AUTHKEY never reaches the log.
func tailscaleLogf(logger *slog.Logger) func(string, ...any) {
	return func(format string, args ...any) {
		line := strings.TrimSpace(fmt.Sprintf(format, args...))
		if line == "" {
			return
		}
		if key := os.Getenv("TS_AUTHKEY"); key != "" {
			line = strings.ReplaceAll(line, key, "[redacted]")
		}
		attrs := []any{"component", "tailnet", "message", line}
		if url := loginURLPattern.FindString(line); url != "" {
			attrs = append(attrs, "login_url", url)
		}
		logger.Info("tailnet message", attrs...)
	}
}

// certificateLogf forwards only the lines embedded Tailscale logs about the
// node's TLS certificate, so a slow or failed issuance shows its progress
// without the backend's other output.
func certificateLogf(logger *slog.Logger) func(string, ...any) {
	logf := tailscaleLogf(logger)
	return func(format string, args ...any) {
		if strings.HasPrefix(format, "cert(") {
			logf(format, args...)
		}
	}
}

// JoinTailscale joins through embedded Tailscale. The node reads its auth key
// from TS_AUTHKEY on first login; later starts use the node state in dir.
// Without a key it logs a login URL, as a field of a structured record, until
// someone opens it. The node only listens: scribble never dials through it.
func JoinTailscale(logger *slog.Logger) Join {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return func(hostname, dir string) (Backend, error) {
		server := &tsnet.Server{
			Dir:      dir,
			Hostname: hostname,
			AuthKey:  os.Getenv("TS_AUTHKEY"),
			UserLogf: tailscaleLogf(logger),
			Logf:     certificateLogf(logger),
		}
		if err := server.Start(); err != nil {
			server.Close()
			return nil, err
		}
		return tsnetBackend{server: server}, nil
	}
}

type tsnetBackend struct {
	server *tsnet.Server
}

func (b tsnetBackend) Listen(addr string) (net.Listener, error) {
	return b.server.Listen("tcp", addr)
}

func (b tsnetBackend) Status(ctx context.Context) (Status, error) {
	client, err := b.server.LocalClient()
	if err != nil {
		return Status{}, err
	}
	st, err := client.StatusWithoutPeers(ctx)
	if err != nil {
		return Status{}, err
	}
	out := Status{Backend: st.BackendState, AuthURL: st.AuthURL, CertDomains: st.CertDomains}
	if st.Self != nil {
		out.DNSName = st.Self.DNSName
	}
	return out, nil
}

func (b tsnetBackend) TLSConfig(ctx context.Context, name string) (*tls.Config, error) {
	client, err := b.server.LocalClient()
	if err != nil {
		return nil, err
	}
	// Fetch ahead of serving so issuance failures are visible without a handshake.
	certPEM, keyPEM, err := client.CertPair(ctx, name)
	if err == nil {
		_, err = tls.X509KeyPair(certPEM, keyPEM)
	}
	if err != nil {
		return nil, err
	}
	return &tls.Config{GetCertificate: client.GetCertificate}, nil
}

func (b tsnetBackend) Close() error {
	if err := b.server.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}
