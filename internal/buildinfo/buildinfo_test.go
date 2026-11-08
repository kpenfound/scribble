package buildinfo

import "testing"

func TestDefaultsAndString(t *testing.T) {
	if Version != "dev" || Commit != "" || Date != "" {
		t.Fatalf("unstamped build: %q %q %q", Version, Commit, Date)
	}
	if String() != "dev" {
		t.Fatal(String())
	}
	v, c, d := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = v, c, d })
	Version, Commit, Date = "v1.2.3", "abc123", "2026-11-06T00:00:00Z"
	if got := String(); got != "v1.2.3 (abc123, 2026-11-06T00:00:00Z)" {
		t.Fatal(got)
	}
}
