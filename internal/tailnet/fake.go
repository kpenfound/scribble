package tailnet

import (
	"context"
	"crypto/tls"
	"net"
	"sync"

	"github.com/kpenfound/scribble/internal/api"
)

// Fake reports a tailnet state set by the test. It implements the same
// status interface as Node (service.TailnetStatuser), so health, status and
// doctor tests need no tailnet.
type Fake struct {
	mu     sync.Mutex
	status api.TailnetStatus
}

// NewFake returns a Fake that reports s.
func NewFake(s api.TailnetStatus) *Fake { return &Fake{status: s} }

// Set changes the state the Fake reports.
func (f *Fake) Set(s api.TailnetStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = s
}

// TailnetStatus reports the current state.
func (f *Fake) TailnetStatus() api.TailnetStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

// FakeBackend is a Backend that touches no tailnet. Its listeners are on the
// loopback interface, so a test can serve and call a handler through it.
type FakeBackend struct {
	mu sync.Mutex

	// State is what Status reports; StatusErr, ListenErr and TLSErr make the
	// matching call fail.
	State     Status
	StatusErr error
	ListenErr error
	TLSErr    error
	// TLS is returned by TLSConfig. Nil returns an empty configuration.
	TLS *tls.Config

	// Recorded calls.
	Listened []string
	CertFor  []string
	Closed   bool

	listeners []net.Listener
}

// SetState changes what Status reports, as a node that drops or recovers.
func (b *FakeBackend) SetState(s Status, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.State, b.StatusErr = s, err
}

// Addr returns the loopback address of the most recent listener, or "".
func (b *FakeBackend) Addr() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.listeners) == 0 {
		return ""
	}
	return b.listeners[len(b.listeners)-1].Addr().String()
}

func (b *FakeBackend) Listen(addr string) (net.Listener, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.Listened = append(b.Listened, addr)
	if b.ListenErr != nil {
		return nil, b.ListenErr
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b.listeners = append(b.listeners, ln)
	return ln, nil
}

func (b *FakeBackend) Status(context.Context) (Status, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.State, b.StatusErr
}

func (b *FakeBackend) TLSConfig(_ context.Context, name string) (*tls.Config, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.CertFor = append(b.CertFor, name)
	if b.TLSErr != nil {
		return nil, b.TLSErr
	}
	if b.TLS != nil {
		return b.TLS, nil
	}
	return &tls.Config{}, nil
}

func (b *FakeBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.Closed = true
	for _, ln := range b.listeners {
		ln.Close()
	}
	return nil
}

// FakeJoin returns a Join that hands out b, or fails with err when it is
// non-nil. It records the hostname and state directory it was asked for in
// the returned *JoinCall.
func FakeJoin(b Backend, err error) (Join, *JoinCall) {
	call := &JoinCall{}
	return func(hostname, dir string) (Backend, error) {
		call.mu.Lock()
		defer call.mu.Unlock()
		call.Count++
		call.Hostname, call.Dir = hostname, dir
		if err != nil {
			return nil, err
		}
		return b, nil
	}, call
}

// JoinCall records what a FakeJoin was asked for.
type JoinCall struct {
	mu       sync.Mutex
	Count    int
	Hostname string
	Dir      string
}

// Get returns a consistent copy of the recorded call.
func (c *JoinCall) Get() (count int, hostname, dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Count, c.Hostname, c.Dir
}
