package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

const base = "version = 1\nvault = \"/srv/vault\"\n"

// parse labels errors with a fixed file name so the error format is exact.
func parse(t *testing.T, text string) (Config, error) {
	t.Helper()
	return Parse("config.toml", []byte(text))
}

func TestParseMinimalConfigKeepsDefaults(t *testing.T) {
	c, err := parse(t, base)
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultConfig()
	want.Vault = "/srv/vault"
	if c.Vault != want.Vault || c.Jev != want.Jev || c.Profiles != want.Profiles || c.Queue != want.Queue || c.Listen != want.Listen {
		t.Fatalf("defaults lost: %+v", c)
	}
	if c.Location().String() != "America/Detroit" {
		t.Fatalf("location %v", c.Location())
	}
}

func TestParseReadsEveryKey(t *testing.T) {
	c, err := parse(t, `version = 1
vault = "/srv/v"
timezone = "UTC"
lookback_days = 3
log_level = "debug"
[daily]
folder = "Notes"
format = "YYYY/MM/DD"
[listen]
web = "localhost:9000"
tailnet = "notes"
tailnet_https = false
[queue]
max_attempts = 5
retry_backoff = "2s"
[jev]
url = "http://jev.example"
model = "m"
api_key_env = "K"
timeout = "3s"
amend_threshold = 0.9
link_threshold = 0
tag_threshold = 1
[profiles.default]
agent = "claude"
model = "claude-x"
timeout = "30s"
[transcribe]
url = "http://whisper.example/v1/audio/transcriptions"
model = "w"
api_key_env = "W"
`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Timezone != "UTC" || c.LookbackDays != 3 || c.Daily.Folder != "Notes" || c.Listen.Web != "localhost:9000" || c.Listen.TailnetHTTPS ||
		c.Queue.MaxAttempts != 5 || c.RetryBackoff() != 2*time.Second || c.JevTimeout() != 3*time.Second || c.WriterTimeout() != 30*time.Second ||
		c.Jev.AmendThreshold != .9 || c.Jev.LinkThreshold != 0 || c.Jev.TagThreshold != 1 || c.Profiles.Default.Model != "claude-x" || c.Transcribe.Model != "w" {
		t.Fatalf("unexpected %+v", c)
	}
}

// Every rejection is "<file>: <field>: <reason>" with the offending field.
func TestParseRejectsWithFileFieldReason(t *testing.T) {
	for name, tc := range map[string]struct{ text, field, reason string }{
		"unknown top-level key":       {base + "bogus = 1\n", "bogus", "unknown key"},
		"unknown nested key":          {base + "[jev]\nbogus = 1\n", "jev.bogus", "unknown key"},
		"YAML-era queue_db":           {base + "queue_db = \"/var/lib/scribble/queue.db\"\n", "queue_db", "unknown key"},
		"YAML-era claude.binary":      {base + "[claude]\nbinary = \"claude\"\n", "claude.binary", "unknown key"},
		"YAML-era amend_confidence":   {base + "[jev]\namend_confidence = 0.5\n", "jev.amend_confidence", "unknown key"},
		"YAML-era link_confidence":    {base + "[jev]\nlink_confidence = 0.5\n", "jev.link_confidence", "unknown key"},
		"YAML-era tag_confidence":     {base + "[jev]\ntag_confidence = 0.5\n", "jev.tag_confidence", "unknown key"},
		"YAML-era listen string":      {base + "listen = \"127.0.0.1:8090\"\n", "listen", "wrong type"},
		"unknown profile":             {base + "[profiles.fast]\nagent = \"claude\"\n", "profiles.fast.agent", "unknown key"},
		"version missing":             {"vault = \"/srv/vault\"\n", "version", "must be 1"},
		"version 2":                   {"version = 2\nvault = \"/srv/vault\"\n", "version", "must be 1"},
		"version 0":                   {"version = 0\nvault = \"/srv/vault\"\n", "version", "must be 1"},
		"vault missing":               {"version = 1\n", "vault", "is required"},
		"unknown timezone":            {base + "timezone = \"Mars/Base\"\n", "timezone", "unknown time zone"},
		"lookback 0":                  {base + "lookback_days = 0\n", "lookback_days", "between 1 and 7"},
		"lookback 8":                  {base + "lookback_days = 8\n", "lookback_days", "between 1 and 7"},
		"lookback negative":           {base + "lookback_days = -1\n", "lookback_days", "between 1 and 7"},
		"log level":                   {base + "log_level = \"loud\"\n", "log_level", "must be debug"},
		"daily folder escapes":        {base + "[daily]\nfolder = \"../x\"\n", "daily.folder", "relative to the vault"},
		"daily format":                {base + "[daily]\nformat = \"dddd\"\n", "daily.format", "unsupported"},
		"public listen.web":           {base + "[listen]\nweb = \"0.0.0.0:8090\"\n", "listen.web", "loopback"},
		"hostname listen.web":         {base + "[listen]\nweb = \"example.com:8090\"\n", "listen.web", "loopback"},
		"listen.web without port":     {base + "[listen]\nweb = \"127.0.0.1\"\n", "listen.web", "host:port"},
		"listen.web without host":     {base + "[listen]\nweb = \":8090\"\n", "listen.web", "host is required"},
		"listen.tailnet not a label":  {base + "[listen]\ntailnet = \"A.b\"\n", "listen.tailnet", "one label"},
		"max_attempts 0":              {base + "[queue]\nmax_attempts = 0\n", "queue.max_attempts", "between 1 and 10"},
		"retry_backoff unparsable":    {base + "[queue]\nretry_backoff = \"soon\"\n", "queue.retry_backoff", "duration string"},
		"retry_backoff not positive":  {base + "[queue]\nretry_backoff = \"0s\"\n", "queue.retry_backoff", "positive"},
		"retry_backoff integer":       {base + "[queue]\nretry_backoff = 5\n", "queue.retry_backoff", "duration string"},
		"jev.timeout integer":         {base + "[jev]\ntimeout = 10\n", "jev.timeout", "duration string"},
		"profile timeout float":       {base + "[profiles.default]\ntimeout = 90.5\n", "profiles.default.timeout", "duration string"},
		"profile timeout unparsable":  {base + "[profiles.default]\ntimeout = \"90\"\n", "profiles.default.timeout", "duration string"},
		"amend threshold above 1":     {base + "[jev]\namend_threshold = 1.01\n", "jev.amend_threshold", "between 0 and 1"},
		"link threshold below 0":      {base + "[jev]\nlink_threshold = -0.01\n", "jev.link_threshold", "between 0 and 1"},
		"tag threshold nan":           {base + "[jev]\ntag_threshold = nan\n", "jev.tag_threshold", "between 0 and 1"},
		"tag threshold inf":           {base + "[jev]\ntag_threshold = inf\n", "jev.tag_threshold", "between 0 and 1"},
		"threshold string":            {base + "[jev]\namend_threshold = \"high\"\n", "jev.amend_threshold", "wrong type"},
		"jev.url relative":            {base + "[jev]\nurl = \"api.typesafe.ai\"\n", "jev.url", "http or https"},
		"jev.url empty":               {base + "[jev]\nurl = \"\"\n", "jev.url", "is required"},
		"agent codex":                 {base + "[profiles.default]\nagent = \"codex\"\n", "profiles.default.agent", "claude"},
		"agent empty":                 {base + "[profiles.default]\nagent = \"\"\n", "profiles.default.agent", "claude"},
		"profile model empty":         {base + "[profiles.default]\nmodel = \"\"\n", "profiles.default.model", "is required"},
		"transcribe.url not http":     {base + "[transcribe]\nurl = \"ftp://x/y\"\n", "transcribe.url", "http or https"},
		"invalid TOML":                {base + "lookback_days = \n", "lookback_days", "invalid TOML"},
		"transcribe.url wrong type":   {base + "[transcribe]\nurl = 3\n", "transcribe.url", "wrong type"},
		"tailnet_https wrong type":    {base + "[listen]\ntailnet_https = \"yes\"\n", "listen.tailnet_https", "wrong type"},
		"max_attempts as string":      {base + "[queue]\nmax_attempts = \"3\"\n", "queue.max_attempts", "wrong type"},
		"lookback as float":           {base + "lookback_days = 7.5\n", "lookback_days", "wrong type"},
		"jev.api_key_env empty":       {base + "[jev]\napi_key_env = \"\"\n", "jev.api_key_env", "is required"},
		"transcribe.model empty":      {base + "[transcribe]\nmodel = \"\"\n", "transcribe.model", "is required"},
		"unknown table under profile": {base + "[profiles.default.extra]\nx = 1\n", "profiles.default.extra.x", "unknown key"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parse(t, tc.text)
			if err == nil {
				t.Fatal("accepted")
			}
			var fe *FieldError
			if !errors.As(err, &fe) {
				t.Fatalf("not a FieldError: %T %v", err, err)
			}
			if fe.Path != "config.toml" || fe.Field != tc.field || !strings.Contains(fe.Reason, tc.reason) {
				t.Fatalf("got path %q field %q reason %q; want field %q reason containing %q", fe.Path, fe.Field, fe.Reason, tc.field, tc.reason)
			}
		})
	}
}

func TestErrorFormat(t *testing.T) {
	_, err := parse(t, base+"[jev]\namend_threshold = 2\n")
	if want := "config.toml: jev.amend_threshold: must be between 0 and 1"; err == nil || err.Error() != want {
		t.Fatalf("got %v, want %s", err, want)
	}
	_, err = parse(t, base+"bogus = 1\n")
	if want := "config.toml: bogus: unknown key"; err == nil || err.Error() != want {
		t.Fatalf("got %v, want %s", err, want)
	}
	// A whole-file failure has no field: <file>: <reason>.
	_, err = parse(t, "= =")
	if err == nil || !strings.HasPrefix(err.Error(), "config.toml: ") {
		t.Fatalf("got %v", err)
	}
	// Errors name keys, never quote values from the file.
	_, err = parse(t, base+"[queue]\nretry_backoff = \"s3cret-token\"\n")
	if err == nil || strings.Contains(err.Error(), "s3cret-token") {
		t.Fatalf("value leaked or accepted: %v", err)
	}
}

func TestLoadUsesFilePathInErrors(t *testing.T) {
	dir := t.TempDir()
	p := PathsAt(dir)
	if _, err := Load(p); err == nil || !strings.HasPrefix(err.Error(), p.Config+": cannot be read") {
		t.Fatalf("missing file: %v", err)
	}
	if err := os.WriteFile(p.Config, []byte(base+"queue_db = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || err.Error() != p.Config+": queue_db: unknown key" {
		t.Fatalf("got %v", err)
	}
	if err := os.WriteFile(p.Config, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Paths != p {
		t.Fatalf("paths %+v", c.Paths)
	}
}

func TestValidateOnDefaultsNeedsVault(t *testing.T) {
	c := DefaultConfig()
	if err := c.Validate(); err == nil || err.Error() != "config: vault: is required" {
		t.Fatalf("got %v", err)
	}
	c.Vault = t.TempDir()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Listen.Web = "0.0.0.0:8090"
	if c.Validate() == nil {
		t.Fatal("public listener accepted")
	}
}

func TestLoopbackWebAddresses(t *testing.T) {
	for _, web := range []string{"127.0.0.1:8090", "localhost:8090", "[::1]:8090", "127.0.0.2:0"} {
		if _, err := parse(t, base+"[listen]\nweb = \""+web+"\"\n"); err != nil {
			t.Errorf("%s: %v", web, err)
		}
	}
	for _, web := range []string{"0.0.0.0:8090", "[::]:8090", "192.168.1.5:8090", "100.64.0.1:8090", "scribble.ts.net:8090"} {
		if _, err := parse(t, base+"[listen]\nweb = \""+web+"\"\n"); err == nil {
			t.Errorf("%s accepted", web)
		}
	}
}

// config.example.toml documents the defaults, so it must decode to them. Its
// only addition is a placeholder vault, which has no default.
func TestExampleMatchesDefaultConfig(t *testing.T) {
	var got Config
	md, err := toml.Decode(string(ExampleTOML), &got)
	if err != nil {
		t.Fatal(err)
	}
	if u := md.Undecoded(); len(u) > 0 {
		t.Fatalf("example has unknown keys: %v", u)
	}
	if got.Vault == "" {
		t.Fatal("example must show a vault")
	}
	got.Vault = ""
	if want := DefaultConfig(); !reflect.DeepEqual(got, want) {
		t.Fatalf("example and DefaultConfig differ:\nexample %+v\ndefault %+v", got, want)
	}
	if _, err := Parse("config.example.toml", ExampleTOML); err != nil {
		t.Fatalf("example does not load: %v", err)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestNoYAMLConfigShips(t *testing.T) {
	if _, err := os.Stat("../../config.example.yaml"); !os.IsNotExist(err) {
		t.Fatalf("config.example.yaml still exists: %v", err)
	}
	mod := string(mustRead(t, "../../go.mod"))
	if strings.Contains(mod, "yaml") {
		t.Fatal("go.mod still mentions yaml")
	}
}

func TestDefaultsAreTheDocumentedValues(t *testing.T) {
	c := DefaultConfig()
	if c.Version != 1 || c.Timezone != "America/Detroit" || c.LookbackDays != 7 || c.LogLevel != "info" ||
		c.Daily != (Daily{"Daily", "YYYY-MM-DD"}) || c.Listen != (Listen{Web: "", Tailnet: "scribble", TailnetHTTPS: true}) ||
		c.Queue != (Queue{3, "5s"}) ||
		c.Jev != (Jev{"https://api.typesafe.ai", "jev-latest", "TYPESAFE_API_KEY", "10s", .75, .75, .6}) ||
		c.Profiles.Default != (Profile{"claude", "claude-haiku-5-5", "90s"}) ||
		c.Transcribe != (Transcribe{"", "large-v3-turbo", "WHISPER_API_KEY"}) {
		t.Fatalf("defaults drifted: %+v", c)
	}
}

func TestConfigJSONHasFileKeys(t *testing.T) {
	c := DefaultConfig()
	c.Paths = PathsAt("/r")
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err = json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	jev := m["jev"].(map[string]any)
	if jev["amend_threshold"] != .75 || m["paths"].(map[string]any)["socket"] != "/r/scribble.sock" {
		t.Fatalf("json shape %s", b)
	}
	if _, ok := jev["amend_confidence"]; ok {
		t.Fatal("YAML-era key in output")
	}
}

func TestPathsAtLayout(t *testing.T) {
	p := PathsAt("/r")
	want := Paths{Root: "/r", Config: "/r/config.toml", DB: "/r/scribble.db", DBLock: "/r/scribble.db.lock", Socket: "/r/scribble.sock",
		Sessions: "/r/sessions", WriterDir: "/r", Tailnet: "/r/tailnet", Log: "/r/service.log"}
	if p != want {
		t.Fatalf("got %+v", p)
	}
}

func TestServiceStateDoesNotUseRelativeRoot(t *testing.T) {
	t.Chdir(t.TempDir())
	p, err := ResolvePaths("rel")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(p.Root) || !filepath.IsAbs(p.Socket) {
		t.Fatalf("relative paths %+v", p)
	}
}
