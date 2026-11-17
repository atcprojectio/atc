package forwarder

import (
	"context"
	"testing"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/assert"
)

const (
	atcEntry      = `{"Kind":"service-resolver","Name":"payments","Meta":{"created-by":"atc"}}`
	overrideEntry = `{"Kind":"service-resolver","Name":"payments","Meta":{"created-by":"atc-override"}}`
	foreignEntry  = `{"Kind":"service-resolver","Name":"payments","DefaultSubset":"v2"}`
)

// TestScheduledWriteIsDroppedWhenResolverChanged covers the dampening-window races: a write
// queued by the leader must not undo a purge (possibly made through another node), replace a
// manual override, or take over a resolver someone else just wrote.
func TestScheduledWriteIsDroppedWhenResolverChanged(t *testing.T) {
	cases := []struct {
		name          string
		atSchedule    string // resolver JSON when the write was queued ("" = none)
		atFire        string // resolver JSON when the timer fires
		expectWritten bool
	}{
		{"unchanged ATC resolver is updated", atcEntry, atcEntry, true},
		{"no resolver yet is created", "", "", true},
		{"adopting a foreign resolver still works", foreignEntry, foreignEntry, true},
		{"purged during dampening", atcEntry, "", false},
		{"overridden during dampening", atcEntry, overrideEntry, false},
		{"written by someone else during dampening", "", foreignEntry, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFakeConsul(t)
			f := newTestForwarder(t, fc.addr())

			owner := ""
			if tc.atSchedule != "" {
				fc.current.Store(tc.atSchedule)
				client, err := api.NewClient(&api.Config{Address: fc.addr()})
				if err != nil {
					t.Fatal(err)
				}
				owner, err = CurrentOwnerState(context.Background(), client, "payments")
				if err != nil {
					t.Fatal(err)
				}
			}
			entry := &api.ServiceResolverConfigEntry{Kind: api.ServiceResolver, Name: "payments", Meta: map[string]string{"created-by": "atc"}}
			f.scheduleWrite("payments", entry, 30*time.Millisecond, owner)
			fc.current.Store(tc.atFire)

			time.Sleep(200 * time.Millisecond)
			if tc.expectWritten {
				assert.Equal(t, int32(1), fc.resolverPuts.Load())
			} else {
				assert.Zero(t, fc.resolverPuts.Load(), "the stale write must be dropped")
			}
		})
	}
}
