// Package buildinfo holds the release version, commit and build date stamped
// into the scribble binary at link time:
//
//	go build -ldflags "-X github.com/kpenfound/scribble/internal/buildinfo.Version=v0.1.0 -X github.com/kpenfound/scribble/internal/buildinfo.Commit=abc123 -X github.com/kpenfound/scribble/internal/buildinfo.Date=2026-11-06T00:00:00Z" ./cmd/scribble
//
// An unstamped build reports version dev with an empty commit and date.
package buildinfo

import "strings"

var (
	// Version is the release version, or dev for an unstamped build.
	Version = "dev"
	// Commit is the source commit the release was built from, or empty.
	Commit = ""
	// Date is the build time in RFC 3339 form, or empty.
	Date = ""
)

// String describes the build on one line, such as "v0.1.0 (abc123, 2026-11-06T00:00:00Z)".
func String() string {
	var detail []string
	if Commit != "" {
		detail = append(detail, Commit)
	}
	if Date != "" {
		detail = append(detail, Date)
	}
	if len(detail) == 0 {
		return Version
	}
	return Version + " (" + strings.Join(detail, ", ") + ")"
}
