package routing

import (
	"testing"

	"github.com/kpenfound/scribble/internal/config"
	"github.com/kpenfound/scribble/internal/vault"
)

func TestThresholdBoundaries(t *testing.T) {
	d := config.DefaultConfig().Jev // the shipped *_threshold defaults
	c := Thresholds{Amend: d.AmendThreshold, Link: d.LinkThreshold, Tag: d.TagThreshold}
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
