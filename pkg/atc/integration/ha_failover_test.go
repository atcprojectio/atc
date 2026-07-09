//go:build integration

package integration

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/atcprojectio/atc/pkg/atc"
	"github.com/atcprojectio/atc/pkg/atc/forwarder"
	atc_server "github.com/atcprojectio/atc/pkg/atc/server"
	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/sdk/testutil"
	. "github.com/smartystreets/goconvey/convey"
)

func TestHALeaderFailover(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")

	// Filter out verbose freeport logs from Stderr
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err == nil {
		os.Stderr = w
		go func() {
			scanner := bufio.NewScanner(r)
			for scanner.Scan() {
				line := scanner.Text()
				if !strings.Contains(line, "freeport:") {
					fmt.Fprintln(oldStderr, line)
				}
			}
		}()
		t.Cleanup(func() {
			w.Close()
			os.Stderr = oldStderr
		})
	}

	Convey("Given a Consul server and two ATC instances configured for HA", t, func() {
		srv, err := testutil.NewTestServerConfigT(silentTestingTB{t}, func(c *testutil.TestServerConfig) {
			c.Stdout = io.Discard
			c.Stderr = io.Discard
		})
		So(err, ShouldBeNil)
		defer srv.Stop()

		srv.WaitForLeader(t)

		// Wait for Consul node registration to be ready/propagated to prevent transient session lock errors
		consulClient, _ := api.NewClient(&api.Config{Address: srv.HTTPAddr})
		for i := 0; i < 50; i++ {
			nodes, _, err := consulClient.Catalog().Nodes(nil)
			if err == nil && len(nodes) > 0 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}

		// Base HA config pointing to the Consul server
		// Use a very short session TTL so lock loss/recovery is rapid
		baseHAConfig := atc.HaConfig{
			Enabled:    true,
			LockKey:    "atc/integration-test/leader",
			SessionTTL: "10s", // Consul minimum is 10s
		}

		cfg1 := atc.Config{
			Name:               "node-1",
			ConsulAddr:         srv.HTTPAddr,
			Target:             []string{"forwarder"},
			DampeningPeriod:    "0s",
			MinDampeningPeriod: "0s",
			WriteRateLimit:     "0s",
			Server: atc_server.Config{
				LogLevel:          "error",
				HTTPListenPort:    getFreePort(t),
				MetricsListenPort: getFreePort(t),
			},
			HA: baseHAConfig,
			Strategies: atc.StrategiesConfig{
				Failover: map[string]forwarder.FailoverStrategy{
					"default": {
						ConnectTimeout: "5s",
						Targets: []forwarder.FailoverTarget{
							{Datacenter: "dc2"},
						},
					},
				},
			},
		}

		cfg2 := atc.Config{
			Name:               "node-2",
			ConsulAddr:         srv.HTTPAddr,
			Target:             []string{"forwarder"},
			DampeningPeriod:    "0s",
			MinDampeningPeriod: "0s",
			WriteRateLimit:     "0s",
			Server: atc_server.Config{
				LogLevel:          "error",
				HTTPListenPort:    getFreePort(t),
				MetricsListenPort: getFreePort(t),
			},
			HA: baseHAConfig,
			Strategies: atc.StrategiesConfig{
				Failover: map[string]forwarder.FailoverStrategy{
					"default": {
						ConnectTimeout: "5s",
						Targets: []forwarder.FailoverTarget{
							{Datacenter: "dc2"},
						},
					},
				},
			},
		}

		atc1, err := atc.New(cfg1)
		So(err, ShouldBeNil)

		atc2, err := atc.New(cfg2)
		So(err, ShouldBeNil)

		ctx1, cancel1 := context.WithCancel(context.Background())
		ctx2, cancel2 := context.WithCancel(context.Background())
		defer func() {
			cancel1()
			cancel2()
		}()

		// Start Instance 1
		go func() { _ = atc1.Run(ctx1) }()

		// Start Instance 2
		go func() { _ = atc2.Run(ctx2) }()

		// Wait for one of them to become the leader
		var leader, standby *atc.Atc
		for i := 0; i < 50; i++ {
			if atc1.IsLeader() && !atc2.IsLeader() {
				leader = atc1
				standby = atc2
				break
			}
			if atc2.IsLeader() && !atc1.IsLeader() {
				leader = atc2
				standby = atc1
				break
			}
			time.Sleep(100 * time.Millisecond)
		}

		So(leader, ShouldNotBeNil)
		So(standby, ShouldNotBeNil)

		Convey("When the active leader is gracefully shut down", func() {
			var leaderCfg atc.Config
			if leader == atc1 {
				cancel1()
				leaderCfg = cfg1
			} else {
				cancel2()
				leaderCfg = cfg2
			}

			// Wait for the standby to acquire the lock and become leader
			isPromoted := false
			for i := 0; i < 50; i++ {
				if standby.IsLeader() {
					isPromoted = true
					break
				}
				time.Sleep(100 * time.Millisecond)
			}

			Convey("Then the standby instance should promote itself to leader", func() {
				So(isPromoted, ShouldBeTrue)

				Convey("And then starting the original leader again", func() {
					atc1Reborn, err := atc.New(leaderCfg)
					So(err, ShouldBeNil)

					ctxReborn, cancelReborn := context.WithCancel(context.Background())
					defer cancelReborn()

					go func() { _ = atc1Reborn.Run(ctxReborn) }()

					// Wait brief period for lock acquisition attempt
					time.Sleep(1 * time.Second)

					Convey("Then leadership should remain stable with the standby holding leadership and reborn node going to standby", func() {
						So(standby.IsLeader(), ShouldBeTrue)
						So(atc1Reborn.IsLeader(), ShouldBeFalse)
					})
				})
			})
		})
	})
}
