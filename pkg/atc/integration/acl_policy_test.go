//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/sdk/testutil"
	"github.com/stretchr/testify/require"

	"github.com/atcprojectio/atc/pkg/atc"
	"github.com/atcprojectio/atc/pkg/atc/forwarder"
	"github.com/atcprojectio/atc/pkg/atc/redirector"
	atc_server "github.com/atcprojectio/atc/pkg/atc/server"
)

// TestShippedACLPolicyIsSufficient runs ATC against a default-deny Consul with a token that
// has only deploy/consul/atc-policy.hcl, and exercises every Consul operation ATC performs.
func TestShippedACLPolicyIsSufficient(t *testing.T) {
	const root = "8a9a2f0e-5a0c-4f7e-9d43-1b2c3d4e5f60"
	srv, err := testutil.NewTestServerConfigT(silentTestingTB{t}, func(c *testutil.TestServerConfig) {
		c.Stdout, c.Stderr = io.Discard, io.Discard
		c.ACL.Enabled = true
		c.ACL.DefaultPolicy = "deny"
		c.ACL.Tokens.InitialManagement = root
		c.ACL.Tokens.Agent = root
	})
	require.NoError(t, err)
	defer srv.Stop()
	srv.WaitForLeader(t)

	admin, err := api.NewClient(&api.Config{Address: srv.HTTPAddr, Token: root})
	require.NoError(t, err)
	rules, err := os.ReadFile("../../../deploy/consul/atc-policy.hcl")
	require.NoError(t, err)
	// The ACL system needs a moment after the leader is elected.
	var policy *api.ACLPolicy
	require.Eventually(t, func() bool {
		policy, _, err = admin.ACL().PolicyCreate(&api.ACLPolicy{Name: "atc", Rules: string(rules)}, nil)
		return err == nil
	}, 15*time.Second, 200*time.Millisecond, "create the ATC policy")
	token, _, err := admin.ACL().TokenCreate(&api.ACLToken{Policies: []*api.ACLTokenPolicyLink{{ID: policy.ID}}}, nil)
	require.NoError(t, err)

	register := func(id, name string, tags ...string) {
		require.NoError(t, admin.Agent().ServiceRegister(&api.AgentServiceRegistration{ID: id, Name: name, Tags: tags, Port: 8080}))
	}
	register("payments-1", "payments", "atc.enabled=true", "atc.failover=standard-failover", "atc.redirect=standard-redirect")
	register("orders-1", "orders", "atc.enabled=true")

	cfg := atc.Config{
		ConsulAddr:      srv.HTTPAddr,
		ConsulToken:     token.SecretID, // ATC's token has only the shipped policy
		Target:          []string{"all"},
		WriteRateLimit:  "100ms",
		DampeningPeriod: "0s",
		HA:              atc.HaConfig{Enabled: true, LockKey: "atc/leader/lock", SessionTTL: "15s"},
		Server: atc_server.Config{
			ListenAddress:     "127.0.0.1",
			HTTPListenPort:    getFreePort(t),
			MetricsListenPort: getFreePort(t),
		},
		Strategies: atc.StrategiesConfig{
			Failover: map[string]forwarder.FailoverStrategy{"standard-failover": {Targets: []forwarder.FailoverTarget{{Datacenter: "dc2"}}}},
			Redirect: map[string]redirector.RedirectStrategy{"standard-redirect": {Datacenter: "dc2"}},
		},
	}
	a, err := atc.New(cfg)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() { cancel(); <-done }()

	resolver := func(name string) *api.ServiceResolverConfigEntry {
		e, _, err := admin.ConfigEntries().Get(api.ServiceResolver, name, nil)
		if err != nil {
			return nil
		}
		return e.(*api.ServiceResolverConfigEntry)
	}
	eventually := func(msg string, ok func() bool) {
		require.Eventually(t, ok, 15*time.Second, 100*time.Millisecond, msg)
	}

	eventually("leader lock (session + KV write)", func() bool {
		p, _, err := admin.KV().Get("atc/leader/lock/forwarder", nil)
		return err == nil && p != nil && p.Session != ""
	})
	eventually("failover resolver written (service:write, catalog read)", func() bool {
		e := resolver("payments")
		return e != nil && e.Meta["created-by"] == "atc" && len(e.Failover) > 0
	})

	fed, err := a.GetFederationStatus(ctx)
	require.NoError(t, err, "federation status (agent members)")
	require.NotEmpty(t, fed)

	require.NoError(t, a.ApplyFailoverOverride(ctx, "orders", "dc2", "", "", false), "override")
	require.NoError(t, a.PurgeServiceResolver(ctx, "orders"), "purge")

	require.NoError(t, admin.Agent().ServiceDeregister("payments-1"))
	eventually("redirect written for a removed service", func() bool {
		e := resolver("payments")
		return e != nil && e.Redirect != nil
	})

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/status", cfg.Server.HTTPListenPort))
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.NoError(t, a.ForceUnlock(ctx, "forwarder"), "force-unlock (KV read + session destroy)")
}
