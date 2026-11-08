package routing

import (
	"testing"

	"github.com/kpenfound/scribble/internal/vault"
)

func TestThresholdBoundaries(t *testing.T) {
	c := Thresholds{Amend: .75, Link: .75, Tag: .6} // the shipped defaults
	today := "2026-11-05"
	for _, test := range []struct {
		date string
		p    float64
		want string
	}{{today, .75, "amend"}, {today, .7499, "link"}, {"2026-11-04", .75, "link"}, {"2026-11-04", .7499, "new"}} {
		if got := Decide(&vault.Entry{Date: test.date}, test.p, today, c); got != test.want {
			t.Fatalf("%+v got %s", test, got)
		}
	}
	if Decide(nil, 1, today, c) != "new" {
		t.Fatal("none must always be new")
	}
}
