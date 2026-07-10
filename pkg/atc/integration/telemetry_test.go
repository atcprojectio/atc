//go:build integration

package integration

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/atcprojectio/atc/pkg/atc"
	"github.com/atcprojectio/atc/pkg/atc/forwarder"
	"github.com/atcprojectio/atc/pkg/atc/redirector"
	atc_server "github.com/atcprojectio/atc/pkg/atc/server"
	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/sdk/testutil"
	. "github.com/smartystreets/goconvey/convey"
)

func TestTelemetryAndHealthCheck(t *testing.T) {
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

	localPort, _, cleanup := startMockBackends(t)
	defer cleanup()

	Convey("Given an ATC instance and a Consul server", t, func() {
		srv, err := testutil.NewTestServerConfigT(silentTestingTB{t}, func(c *testutil.TestServerConfig) {
			c.Stdout = io.Discard
			c.Stderr = io.Discard
		})
		So(err, ShouldBeNil)
		defer srv.Stop()

		srv.WaitForLeader(t)

		// Wait for Consul node registration to be ready/propagated
		consulClient, _ := api.NewClient(&api.Config{Address: srv.HTTPAddr})
		for i := 0; i < 50; i++ {
			nodes, _, err := consulClient.Catalog().Nodes(nil)
			if err == nil && len(nodes) > 0 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}

		httpPort := getFreePort(t)
		metricsPort := getFreePort(t)
		mcpPort := getFreePort(t)

		cfg := atc.Config{
			Name:               "integration-test-node",
			ConsulAddr:         srv.HTTPAddr,
			Target:             []string{"forwarder", "redirector"},
			DampeningPeriod:    "0s",
			MinDampeningPeriod: "0s",
			WriteRateLimit:     "0s",
			Server: atc_server.Config{
				LogLevel:          "error",
				HTTPListenPort:    httpPort,
				MetricsListenPort: metricsPort,
				McpListenPort:     mcpPort,
				McpEnabled:        true,
			},
			Strategies: atc.StrategiesConfig{
				Failover: map[string]forwarder.FailoverStrategy{
					"default": {
						ConnectTimeout: "5s",
						Targets: []forwarder.FailoverTarget{
							{Datacenter: "dc2"},
						},
					},
				},
				Redirect: map[string]redirector.RedirectStrategy{
					"default": {
						Datacenter: "dc2",
					},
				},
			},
		}

		atcInstance, err := atc.New(cfg)
		So(err, ShouldBeNil)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		errChan := make(chan error, 1)
		go func() {
			errChan <- atcInstance.Run(ctx)
		}()

		client, err := api.NewClient(&api.Config{Address: srv.HTTPAddr})
		So(err, ShouldBeNil)

		// Wait for HTTP server to become ready
		var readyResp *http.Response
		for i := 0; i < 50; i++ {
			resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/ready", httpPort))
			if err == nil && resp.StatusCode == http.StatusOK {
				readyResp = resp
				break
			}
			time.Sleep(100 * time.Millisecond)
		}

		Convey("When querying the readiness check /ready", func() {
			So(readyResp, ShouldNotBeNil)
			defer readyResp.Body.Close()

			body, err := io.ReadAll(readyResp.Body)
			So(err, ShouldBeNil)
			So(string(body), ShouldEqual, "OK")

			Convey("And when querying the dedicated MCP endpoint", func() {
				var mcpResp *http.Response
				for i := 0; i < 50; i++ {
					resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/mcp", mcpPort))
					if err == nil {
						mcpResp = resp
						break
					}
					time.Sleep(100 * time.Millisecond)
				}
				So(mcpResp, ShouldNotBeNil)
				defer mcpResp.Body.Close()
				So(mcpResp.StatusCode, ShouldNotEqual, http.StatusNotFound)
			})

			Convey("And when registering a service to trigger forwarder reconciliation", func() {
				reg := &api.AgentServiceRegistration{
					ID:   "telemetry-service-1",
					Name: "telemetry-service",
					Tags: []string{"atc.enabled=true"},
					Port: localPort,
				}
				err = client.Agent().ServiceRegister(reg)
				So(err, ShouldBeNil)

				// Wait for the failover resolver to be created to ensure forwarder has run reconciliation
				var resolver *api.ServiceResolverConfigEntry
				for i := 0; i < 50; i++ {
					entry, _, err := client.ConfigEntries().Get("service-resolver", "telemetry-service", nil)
					if err == nil {
						if res, ok := entry.(*api.ServiceResolverConfigEntry); ok {
							if len(res.Failover) > 0 {
								resolver = res
								break
							}
						}
					}
					time.Sleep(100 * time.Millisecond)
				}
				So(resolver, ShouldNotBeNil)

				Convey("Then the Prometheus /metrics endpoint should expose the reconciliation counters", func() {
					var metricsResp *http.Response
					for i := 0; i < 50; i++ {
						resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", metricsPort))
						if err == nil && resp.StatusCode == http.StatusOK {
							metricsResp = resp
							break
						}
						time.Sleep(100 * time.Millisecond)
					}
					So(metricsResp, ShouldNotBeNil)
					defer metricsResp.Body.Close()

					metricsBody, err := io.ReadAll(metricsResp.Body)
					So(err, ShouldBeNil)
					metricsStr := string(metricsBody)

					// Verify standard OTel target metadata and custom forwarder reconciliation total counter are present
					So(metricsStr, ShouldContainSubstring, "atc_forwarder_reconcile_runs_total")
					So(metricsStr, ShouldContainSubstring, "target_info")
				})
			})
		})
	})
}
