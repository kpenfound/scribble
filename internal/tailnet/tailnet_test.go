package tailnet

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"log/slog"
	"math/big"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/scribble/internal/api"
)

const (
	fullName = "scribble.tail1234.ts.net."
	appURL   = "scribble.tail1234.ts.net"
)

func runningState() Status {
	return Status{Backend: "Running", DNSName: fullName, CertDomains: []string{appURL}}
}

// hostsCapture is a Handler that records the hosts it was built with and
// answers every request with "scribble".
type hostsCapture struct{ hosts []string }

func (c *hostsCapture) handler(hosts []string) http.Handler {
	c.hosts = hosts
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "scribble") })
}

// runNode starts a Node on a fast clock and stops it when the test ends.
func runNode(t *testing.T, cfg Config, handler Handler) *Node {
	t.Helper()
	cfg.PollInterval = 5 * time.Millisecond
	if cfg.RetryInterval == 0 {
		cfg.RetryInterval = 5 * time.Millisecond
	}
	if cfg.Dir == "" {
		cfg.Dir = filepath.Join(t.TempDir(), "tailnet")
	}
	n := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { n.Run(ctx, handler); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return n
}

func waitFor(t *testing.T, n *Node, ok func(api.TailnetStatus) bool) api.TailnetStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := n.TailnetStatus(); ok(s) {
			return s
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("state never matched: %+v", n.TailnetStatus())
	return api.TailnetStatus{}
}

func inState(want string) func(api.TailnetStatus) bool {
	return func(s api.TailnetStatus) bool { return s.State == want }
}

func get(t *testing.T, client *http.Client, url string) string {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// A node without a hostname is disabled, and Run returns without joining.
func TestDisabledNodeReportsDisabledAndNeverJoins(t *testing.T) {
	t.Parallel()
	join, call := FakeJoin(&FakeBackend{}, nil)
	n := New(Config{Join: join})
	n.Run(context.Background(), (&hostsCapture{}).handler)
	if s := n.TailnetStatus(); s != (api.TailnetStatus{State: api.TailnetDisabled}) {
		t.Fatalf("state %+v", s)
	}
	if count, _, _ := call.Get(); count != 0 {
		t.Fatalf("joined %d times", count)
	}
	var nilNode *Node
	if s := nilNode.TailnetStatus(); s.State != api.TailnetDisabled {
		t.Fatalf("nil node state %+v", s)
	}
}

// A running node joins with the configured hostname and state directory,
// serves plain HTTP on port 80 when tailnet_https is false and reports
// running with the app's URL and the Host values it serves.
func TestNodeJoinsWithConfiguredHostnameAndServesHTTP(t *testing.T) {
	t.Parallel()
	b := &FakeBackend{State: runningState()}
	join, call := FakeJoin(b, nil)
	dir := filepath.Join(t.TempDir(), "tailnet")
	var hosts hostsCapture
	n := runNode(t, Config{Hostname: "scribble", Dir: dir, Join: join}, hosts.handler)

	s := waitFor(t, n, inState(api.TailnetRunning))
	if s.URL != "http://"+appURL || s.Reason != "" {
		t.Fatalf("status %+v", s)
	}
	if _, hostname, gotDir := call.Get(); hostname != "scribble" || gotDir != dir {
		t.Fatalf("joined as %q in %q", hostname, gotDir)
	}
	if len(b.Listened) != 1 || b.Listened[0] != ":80" || len(b.CertFor) != 0 {
		t.Fatalf("listened %v, certs %v", b.Listened, b.CertFor)
	}
	want := []string{appURL, "scribble"}
	if strings.Join(hosts.hosts, ",") != strings.Join(want, ",") {
		t.Fatalf("hosts %v, want %v", hosts.hosts, want)
	}
	if body := get(t, http.DefaultClient, "http://"+b.Addr()+"/"); body != "scribble" {
		t.Fatalf("body %q", body)
	}
}

func selfSigned(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: appURL},
		DNSNames:     []string{appURL},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

// With tailnet_https the node asks for a certificate for its full name, listens
// on 443 and serves TLS, with an https URL in its status.
func TestNodeServesHTTPSWithTailscaleCertificates(t *testing.T) {
	t.Parallel()
	b := &FakeBackend{State: runningState(), TLS: selfSigned(t)}
	join, _ := FakeJoin(b, nil)
	var hosts hostsCapture
	n := runNode(t, Config{Hostname: "scribble", HTTPS: true, Join: join}, hosts.handler)

	s := waitFor(t, n, inState(api.TailnetRunning))
	if s.URL != "https://"+appURL {
		t.Fatalf("status %+v", s)
	}
	if len(b.Listened) != 1 || b.Listened[0] != ":443" || len(b.CertFor) != 1 || b.CertFor[0] != appURL {
		t.Fatalf("listened %v, certs %v", b.Listened, b.CertFor)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	if body := get(t, client, "https://"+b.Addr()+"/"); body != "scribble" {
		t.Fatalf("body %q", body)
	}
	// The listener speaks TLS only: plain HTTP is not served.
	if resp, err := http.Get("http://" + b.Addr() + "/"); err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatal("plain HTTP answered on the HTTPS listener")
		}
	}
}

// A start or runtime failure is state with a reason, never a fatal error
// to the caller: Run has no error to return, and the node keeps trying.
func TestStartupFailuresAreDegradedWithAReason(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		https   bool
		join    error
		backend *FakeBackend
		reason  string
	}{
		{"join fails", false, errors.New("boom"), &FakeBackend{}, "join the tailnet as scribble: boom"},
		{"status fails", false, nil, &FakeBackend{StatusErr: errors.New("no local api")}, "query the tailnet node: no local api"},
		{"listen fails", false, nil, &FakeBackend{State: runningState(), ListenErr: errors.New("port taken")}, "listen on the tailnet: port taken"},
		{"needs login", false, nil, &FakeBackend{State: Status{Backend: "NeedsLogin", AuthURL: "https://login.example/a/1"}}, "the node needs login: open https://login.example/a/1"},
		{"needs login without a URL", false, nil, &FakeBackend{State: Status{Backend: "NeedsLogin"}}, "the node needs login: set TS_AUTHKEY and restart"},
		{"awaits approval", false, nil, &FakeBackend{State: Status{Backend: "NeedsMachineAuth"}}, "the node awaits approval on the tailnet"},
		{"https disabled in admin console", true, nil, &FakeBackend{State: Status{Backend: "Running", DNSName: fullName}}, "enable HTTPS certificates in the tailnet admin console for " + appURL},
		{"no full name", true, nil, &FakeBackend{State: Status{Backend: "Running", DNSName: "scribble.", CertDomains: []string{"scribble"}}}, "the node has no full MagicDNS name for HTTPS"},
		{"certificate fails", true, nil, &FakeBackend{State: runningState(), TLSErr: errors.New("acme rate limit")}, "certificate for " + appURL + ": acme rate limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			join, _ := FakeJoin(tc.backend, tc.join)
			var hosts hostsCapture
			n := runNode(t, Config{Hostname: "scribble", HTTPS: tc.https, Join: join}, hosts.handler)
			s := waitFor(t, n, func(s api.TailnetStatus) bool { return s.State == api.TailnetDegraded && s.Reason == tc.reason })
			if s.URL != "" {
				t.Fatalf("degraded start reports a URL: %+v", s)
			}
		})
	}
}

// A node that fails to start is started again, and reports running once it
// does.
func TestFailedStartRetriesAndRecovers(t *testing.T) {
	t.Parallel()
	b := &FakeBackend{State: runningState(), ListenErr: errors.New("port taken")}
	join, call := FakeJoin(b, nil)
	var hosts hostsCapture
	n := runNode(t, Config{Hostname: "scribble", Join: join}, hosts.handler)
	waitFor(t, n, inState(api.TailnetDegraded))

	b.mu.Lock()
	b.ListenErr = nil
	b.mu.Unlock()
	waitFor(t, n, inState(api.TailnetRunning))
	if count, _, _ := call.Get(); count < 2 {
		t.Fatalf("joined %d times", count)
	}
}

// A node that drops while serving is degraded with the reason and its
// last URL, keeps its listener, and returns to running when it recovers.
func TestRuntimeDropIsDegradedThenRecovers(t *testing.T) {
	t.Parallel()
	b := &FakeBackend{State: runningState()}
	join, call := FakeJoin(b, nil)
	var hosts hostsCapture
	n := runNode(t, Config{Hostname: "scribble", Join: join}, hosts.handler)
	waitFor(t, n, inState(api.TailnetRunning))

	b.SetState(Status{Backend: "Stopped"}, nil)
	s := waitFor(t, n, inState(api.TailnetDegraded))
	for s.Reason != "the node is stopped" {
		s = waitFor(t, n, inState(api.TailnetDegraded))
	}
	if s.URL != "http://"+appURL {
		t.Fatalf("degraded status lost the URL: %+v", s)
	}

	b.SetState(Status{}, errors.New("local api gone"))
	waitFor(t, n, func(s api.TailnetStatus) bool { return s.State == api.TailnetDegraded && s.Reason == "local api gone" })

	b.SetState(runningState(), nil)
	if s := waitFor(t, n, inState(api.TailnetRunning)); s.Reason != "" {
		t.Fatalf("recovered status keeps a reason: %+v", s)
	}
	if count, _, _ := call.Get(); count != 1 {
		t.Fatalf("a drop restarted the node %d times", count)
	}
}

// A panic in the tailnet implementation is a degraded state, not a crash.
func TestPanicInBackendIsDegraded(t *testing.T) {
	t.Parallel()
	join := func(string, string) (Backend, error) { panic("tsnet exploded") }
	var hosts hostsCapture
	n := runNode(t, Config{Hostname: "scribble", Join: join}, hosts.handler)
	waitFor(t, n, func(s api.TailnetStatus) bool {
		return s.State == api.TailnetDegraded && strings.Contains(s.Reason, "tsnet exploded")
	})
}

// A state directory that cannot be created, or no implementation, is also a
// reason.
func TestUnusableConfigurationIsDegraded(t *testing.T) {
	t.Parallel()
	var hosts hostsCapture
	n := runNode(t, Config{Hostname: "scribble"}, hosts.handler)
	waitFor(t, n, func(s api.TailnetStatus) bool {
		return s.State == api.TailnetDegraded && s.Reason == "no tailnet implementation configured"
	})

	file := filepath.Join(t.TempDir(), "file")
	if err := writeFile(file); err != nil {
		t.Fatal(err)
	}
	join, _ := FakeJoin(&FakeBackend{State: runningState()}, nil)
	n = runNode(t, Config{Hostname: "scribble", Dir: filepath.Join(file, "tailnet"), Join: join}, hosts.handler)
	waitFor(t, n, func(s api.TailnetStatus) bool {
		return s.State == api.TailnetDegraded && strings.HasPrefix(s.Reason, "create tailnet state directory")
	})
}

// A node waiting for login is degraded with the login URL, then starts serving
// once the owner approves it, with no restart.
func TestNodeWaitingForLoginServesOnceApproved(t *testing.T) {
	t.Parallel()
	b := &FakeBackend{State: Status{Backend: "NeedsLogin", AuthURL: "https://login.example/a/1"}}
	join, call := FakeJoin(b, nil)
	var hosts hostsCapture
	n := runNode(t, Config{Hostname: "scribble", Join: join}, hosts.handler)
	waitFor(t, n, inState(api.TailnetDegraded))

	b.SetState(Status{Backend: "Starting"}, nil)
	waitFor(t, n, inState(api.TailnetStarting))
	b.SetState(runningState(), nil)
	waitFor(t, n, inState(api.TailnetRunning))
	if count, _, _ := call.Get(); count != 1 {
		t.Fatalf("joined %d times", count)
	}
}

// Stopping the service stops the listener and leaves the tailnet.
func TestRunStopsServingAndClosesTheNodeWhenContextEnds(t *testing.T) {
	t.Parallel()
	b := &FakeBackend{State: runningState()}
	join, _ := FakeJoin(b, nil)
	var hosts hostsCapture
	n := New(Config{Hostname: "scribble", Dir: filepath.Join(t.TempDir(), "tailnet"), Join: join, PollInterval: 5 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { n.Run(ctx, hosts.handler); close(done) }()
	waitFor(t, n, inState(api.TailnetRunning))
	addr := b.Addr()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	b.mu.Lock()
	closed := b.Closed
	b.mu.Unlock()
	if !closed {
		t.Fatal("node was not closed")
	}
	if resp, err := http.Get("http://" + addr + "/"); err == nil {
		resp.Body.Close()
		t.Fatal("listener still serving")
	}
}

// The Fake reports whatever state a test sets, through the same method the
// service's health reads.
func TestFakeReportsTheStateItIsGiven(t *testing.T) {
	t.Parallel()
	f := NewFake(api.TailnetStatus{State: api.TailnetStarting})
	if f.TailnetStatus().State != api.TailnetStarting {
		t.Fatalf("%+v", f.TailnetStatus())
	}
	want := api.TailnetStatus{State: api.TailnetDegraded, Reason: "login required"}
	f.Set(want)
	if f.TailnetStatus() != want {
		t.Fatalf("%+v", f.TailnetStatus())
	}
}

func TestBackendReasons(t *testing.T) {
	t.Parallel()
	for backend, want := range map[string]string{
		"Stopped":  "the node is stopped",
		"Starting": "the node is starting",
		"":         "the node reports no state",
	} {
		if got := backendReason(Status{Backend: backend}); got != want {
			t.Errorf("%q: %q, want %q", backend, got, want)
		}
	}
}

type tailscaleLogRecord struct {
	Component string `json:"component"`
	Message   string `json:"message"`
	LoginURL  string `json:"login_url"`
}

func parseRecords(t *testing.T, out string) []tailscaleLogRecord {
	t.Helper()
	var records []tailscaleLogRecord
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		var r tailscaleLogRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("line does not parse as JSON: %s: %v", line, err)
		}
		records = append(records, r)
	}
	return records
}

// An embedded Tailscale log line becomes one structured record, the login
// URL travels as its own field and TS_AUTHKEY is redacted.
//
// serial: calls t.Setenv, which fails if run in parallel with another test.
func TestTailscaleLogfRecordsLoginURLAndRedactsTheAuthKey(t *testing.T) {
	t.Setenv("TS_AUTHKEY", "tskey-secret")
	var buf bytes.Buffer
	logf := tailscaleLogf(slog.New(slog.NewJSONHandler(&buf, nil)))
	logf("To start this tsnet server, restart with ts.AuthKey set, or visit:\n\t%s", "https://login.tailscale.com/a/0123456789abcdef")
	logf("")
	logf("tskey-secret leaked")

	records := parseRecords(t, buf.String())
	if len(records) != 2 {
		t.Fatalf("records %+v", records)
	}
	if records[0].Component != "tailnet" || records[0].LoginURL != "https://login.tailscale.com/a/0123456789abcdef" {
		t.Fatalf("login URL did not travel as its own field: %+v", records[0])
	}
	if strings.Contains(buf.String(), "tskey-secret") {
		t.Fatalf("a tailnet message leaked TS_AUTHKEY: %s", buf.String())
	}
}

// Only the backend's certificate lines reach the service log.
func TestCertificateLogfForwardsOnlyCertificateLines(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logf := certificateLogf(slog.New(slog.NewJSONHandler(&buf, nil)))
	logf(`cert("scribble.tail0.ts.net"): did SetDNS for %s`, "_acme-challenge.scribble.tail0.ts.net")
	logf("magicsock: disco: node %s now using %s", "[abc]", "192.0.2.1:41641")
	logf(`cert("scribble.tail0.ts.net"): got cert`)

	var messages []string
	for _, r := range parseRecords(t, buf.String()) {
		messages = append(messages, r.Message)
	}
	want := []string{`cert("scribble.tail0.ts.net"): did SetDNS for _acme-challenge.scribble.tail0.ts.net`, `cert("scribble.tail0.ts.net"): got cert`}
	if strings.Join(messages, "\n") != strings.Join(want, "\n") {
		t.Fatalf("messages %q, want %q", messages, want)
	}
}

// The node is only for listening: no code outside this package imports
// Tailscale, and the package itself never dials through the node. Outbound
// Jev, Claude and Whisper traffic therefore uses the host's network.
func TestNothingDialsThroughTheTailnetNode(t *testing.T) {
	t.Parallel()
	internal := filepath.Join("..")
	err := filepath.WalkDir(internal, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		inTailnet := filepath.Base(filepath.Dir(path)) == "tailnet"
		for _, imp := range file.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if strings.HasPrefix(p, "tailscale.com") && !inTailnet {
				t.Errorf("%s imports %s; only internal/tailnet may use the embedded node", path, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tailnet.go", "tsnet.go"} {
		src, err := readFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{".Dial(", ".DialContext(", "HTTPClient(", "Sys().Dialer"} {
			if strings.Contains(src, banned) {
				t.Errorf("%s uses %s, which would send traffic out through the node", name, banned)
			}
		}
	}
}
