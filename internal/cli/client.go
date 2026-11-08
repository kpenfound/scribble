package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/kpenfound/scribble/internal/api"
)

// unavailableError means nothing answered on the service's socket.
type unavailableError struct {
	socket string
	err    error
}

func (e *unavailableError) Error() string {
	return fmt.Sprintf("no scribble service answers on %s (start one with `scribble serve`): %v", e.socket, e.err)
}
func (e *unavailableError) Unwrap() error { return e.err }

// client talks to one service over its Unix socket.
type client struct {
	socket string
	http   *http.Client
}

func newClient(socket string, timeout time.Duration) *client {
	return &client{socket: socket, http: &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
			DisableKeepAlives: true,
		},
	}}
}

// do sends one request and decodes a 2xx JSON body into out (when non-nil).
// A refused request becomes the *api.Error the service sent.
func (c *client) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://scribble"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &unavailableError{socket: c.socket, err: unwrapURL(err)}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var envelope api.ErrorResponse
		if json.Unmarshal(body, &envelope) == nil && envelope.Error.Code != "" {
			e := envelope.Error
			return &e
		}
		return api.Errorf(api.Internal, "%s %s: unexpected %s", method, path, resp.Status)
	}
	if out == nil {
		return nil
	}
	if err = json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s %s: unreadable response: %w", method, path, err)
	}
	return nil
}

func unwrapURL(err error) error {
	var u *url.Error
	if errors.As(err, &u) {
		return u.Err
	}
	return err
}

// waitGone returns once the service's socket file has been removed, which the
// service does last, after its listeners are closed, its worker has finished
// and its database is closed.
func (c *client) waitGone(ctx context.Context, limit time.Duration) error {
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Lstat(c.socket); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("the service on %s did not exit within %s", c.socket, limit)
		case <-tick.C:
		}
	}
}
