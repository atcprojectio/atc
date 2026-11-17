package redirector

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

// TestReturningServiceCancelsQueuedRedirect: a service disappears (redirect queued for the
// dampening window) and comes back before it fires. The redirect must not be written. The
// redirector runs next to the forwarder here, as in production.
func TestReturningServiceCancelsQueuedRedirect(t *testing.T) {
	const dampening = 600 * time.Millisecond
	fc := newFakeConsul(t)
	atcEntry := `{"Kind":"service-resolver","Name":"payments","Meta":{"created-by":"atc","failover-strategy":""},` +
		`"Failover":{"*":{"Targets":[{"Service":"payments","Datacenter":"dc2"}]}}}`
	fc.resolvers.Store("[" + atcEntry + "]")
	fc.current.Store(atcEntry)

	r, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)), fc.addr(), "", "dc1", "", "", true, nil, dampening.String(), "", false, false)
	require.NoError(t, err)
	client, err := api.NewClient(&api.Config{Address: fc.addr()})
	require.NoError(t, err)

	fc.catalog.Store(`{}`) // service gone: redirect queued
	require.NoError(t, r.reconcile(context.Background(), client))
	fc.catalog.Store(`{"payments":["atc.enabled=true"]}`) // back before the redirect fires
	require.NoError(t, r.reconcile(context.Background(), client))

	time.Sleep(dampening + 400*time.Millisecond)
	assert.Zero(t, fc.puts.Load(), "a redirect for a service that came back must not fire")
}

// TestGoneServiceStillGetsItsRedirect is the control: a queued redirect fires when the service
// stays gone.
func TestGoneServiceStillGetsItsRedirect(t *testing.T) {
	const dampening = 300 * time.Millisecond
	fc := newFakeConsul(t)
	atcEntry := `{"Kind":"service-resolver","Name":"payments","Meta":{"created-by":"atc"}}`
	fc.resolvers.Store("[" + atcEntry + "]")
	fc.current.Store(atcEntry)
	fc.catalog.Store(`{}`)

	r, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)), fc.addr(), "", "dc1", "", "", true, nil, dampening.String(), "", false, false)
	require.NoError(t, err)
	client, err := api.NewClient(&api.Config{Address: fc.addr()})
	require.NoError(t, err)
	require.NoError(t, r.reconcile(context.Background(), client))
	require.NoError(t, r.reconcile(context.Background(), client)) // unrelated event

	require.Eventually(t, func() bool { return fc.puts.Load() == 1 }, 3*dampening, 10*time.Millisecond)
}
