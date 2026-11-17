package forwarder

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Races where the service changes its mind while a dampened write is queued. In each case the
// resolver's owner stays the same, so only cancelling the queued write prevents a stale write.

const dampening = 600 * time.Millisecond

const resolverA = `{"Kind":"service-resolver","Name":"payments","ConnectTimeout":"15s",` +
	`"Meta":{"created-by":"atc","failover-strategy":"A"},` +
	`"Failover":{"*":{"Targets":[{"Service":"payments","Datacenter":"dc2"}]}}}`

func newDampenedForwarder(t *testing.T, addr string) (*Forwarder, *api.Client) {
	t.Helper()
	strategies := map[string]FailoverStrategy{
		"A": {Targets: []FailoverTarget{{Datacenter: "dc2"}}},
		"B": {Targets: []FailoverTarget{{Datacenter: "dc3"}}},
	}
	f, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)), addr, "", "dc1", "", "", strategies, dampening.String(), "", false)
	require.NoError(t, err)
	client, err := api.NewClient(&api.Config{Address: addr})
	require.NoError(t, err)
	return f, client
}

func reconcileWithTags(t *testing.T, fc *fakeConsul, f *Forwarder, client *api.Client, tags ...string) {
	t.Helper()
	fc.tags.Store(tags)
	require.NoError(t, f.reconcile(context.Background(), client))
}

func TestFlapBackToCurrentStrategyCancelsQueuedWrite(t *testing.T) {
	fc := newFakeConsul(t)
	fc.datacenters.Store(`["dc1","dc2","dc3"]`)
	fc.resolvers.Store("[" + resolverA + "]")
	fc.current.Store(resolverA)
	f, client := newDampenedForwarder(t, fc.addr())

	reconcileWithTags(t, fc, f, client, "atc.enabled=true", "atc.failover=A")
	require.Zero(t, fc.resolverPuts.Load(), "sanity: strategy A is already in Consul")

	reconcileWithTags(t, fc, f, client, "atc.enabled=true", "atc.failover=B") // queues B
	reconcileWithTags(t, fc, f, client, "atc.enabled=true", "atc.failover=A") // back to A

	time.Sleep(dampening + 400*time.Millisecond)
	assert.Zero(t, fc.resolverPuts.Load(), "the stale write for strategy B must not fire")
}

func TestRemovingEnabledTagDuringFirstWindowCancelsQueuedWrite(t *testing.T) {
	fc := newFakeConsul(t) // no resolver in Consul yet
	f, client := newDampenedForwarder(t, fc.addr())

	reconcileWithTags(t, fc, f, client, "atc.enabled=true", "atc.failover=A") // queues a create
	reconcileWithTags(t, fc, f, client /* atc.enabled removed */)

	time.Sleep(dampening + 400*time.Millisecond)
	assert.Zero(t, fc.resolverPuts.Load(), "a service that opted out must not get a resolver")
}

func TestRemovingAdoptTagDuringWindowCancelsQueuedWrite(t *testing.T) {
	fc := newFakeConsul(t)
	fc.resolvers.Store("[" + foreignEntry + "]")
	fc.current.Store(foreignEntry)
	f, client := newDampenedForwarder(t, fc.addr())

	reconcileWithTags(t, fc, f, client, "atc.enabled=true", "atc.failover=A", AdoptTag) // queues the takeover
	reconcileWithTags(t, fc, f, client, "atc.enabled=true", "atc.failover=A")           // adopt removed

	time.Sleep(dampening + 400*time.Millisecond)
	assert.Zero(t, fc.resolverPuts.Load(), "the foreign resolver must not be taken over")
}

// TestUnchangedIntentKeepsQueuedWriteAndItsTimer is the control: reconciles that still want the
// same write (e.g. caused by unrelated catalog events) neither cancel it nor restart its timer.
func TestUnchangedIntentKeepsQueuedWriteAndItsTimer(t *testing.T) {
	fc := newFakeConsul(t)
	f, client := newDampenedForwarder(t, fc.addr())

	start := time.Now()
	reconcileWithTags(t, fc, f, client, "atc.enabled=true", "atc.failover=A")
	time.Sleep(dampening * 2 / 3)
	reconcileWithTags(t, fc, f, client, "atc.enabled=true", "atc.failover=A")

	require.Eventually(t, func() bool { return fc.resolverPuts.Load() == 1 }, 2*dampening, 10*time.Millisecond)
	assert.Less(t, time.Since(start), dampening+dampening/2, "the timer must not restart on an unchanged reconcile")
}
