package watcher

import (
	"testing"

	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/api/watch"
	"github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/require"
)

// TestStopBeforeFirstQueryDoesNotPanic covers a race in consul/api/watch: when Plan.Stop runs
// after the run loop's stop check but before the first query has registered its cancel func,
// the query defers a nil cancel func and panics, crashing ATC on shutdown or watcher restart.
func TestStopBeforeFirstQueryDoesNotPanic(t *testing.T) {
	client, err := api.NewClient(&api.Config{Address: "127.0.0.1:1"})
	require.NoError(t, err)
	for i := 0; i < 3000; i++ {
		plan, err := watch.Parse(map[string]any{"type": "services"})
		require.NoError(t, err)
		guardStoppedPlan(plan)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = plan.RunWithClientAndHclog(client, hclog.NewNullLogger())
		}()
		plan.Stop()
		<-done
	}
}
