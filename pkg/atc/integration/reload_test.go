//go:build integration

package integration

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
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

func TestDynamicConfigReload(t *testing.T) {
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

	// Start a third HTTP server for DC3
	dc3Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("remote-dc3-answering"))
	}))
	defer dc3Server.Close()

	dc3URL, _ := url.Parse(dc3Server.URL)
	dc3Port, _ := strconv.Atoi(dc3URL.Port())

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

		// Initialize with default strategy pointing to DC2
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
		for i := 0; i < 50; i++ {
			resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/ready", httpPort))
			if err == nil && resp.StatusCode == http.StatusOK {
				resp.Body.Close()
				break
			}
			time.Sleep(100 * time.Millisecond)
		}

		Convey("When registering a service and waiting for the default DC2 failover resolver", func() {
			reg := &api.AgentServiceRegistration{
				ID:   "reload-service-1",
				Name: "reload-service",
				Tags: []string{"atc.enabled=true"},
				Port: localPort,
			}
			err = client.Agent().ServiceRegister(reg)
			So(err, ShouldBeNil)

			var resolver *api.ServiceResolverConfigEntry
			for i := 0; i < 50; i++ {
				entry, _, err := client.ConfigEntries().Get("service-resolver", "reload-service", nil)
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
			So(resolver.Failover["*"].Targets[0].Datacenter, ShouldEqual, "dc2")

			Convey("And then reloading the configuration dynamically to point to DC3", func() {
				// Update configuration strategies in-memory
				reloadedCfg := cfg
				reloadedCfg.Strategies.Failover = map[string]forwarder.FailoverStrategy{
					"default": {
						ConnectTimeout: "5s",
						Targets: []forwarder.FailoverTarget{
							{Datacenter: "dc3"},
						},
					},
				}
				reloadedCfg.Strategies.Redirect = map[string]redirector.RedirectStrategy{
					"default": {
						Datacenter: "dc3",
					},
				}

				atcInstance.ReloadConfig(reloadedCfg)

				// Deregister and re-register the service to trigger the forwarder's watcher/reconciliation loop
				err = client.Agent().ServiceDeregister("reload-service-1")
				So(err, ShouldBeNil)
				time.Sleep(100 * time.Millisecond)

				err = client.Agent().ServiceRegister(reg)
				So(err, ShouldBeNil)

				// Wait for the resolver targets to be updated in Consul to point to DC3
				var updatedResolver *api.ServiceResolverConfigEntry
				for i := 0; i < 50; i++ {
					entry, _, err := client.ConfigEntries().Get("service-resolver", "reload-service", nil)
					if err == nil {
						if res, ok := entry.(*api.ServiceResolverConfigEntry); ok {
							if len(res.Failover) > 0 && res.Failover["*"].Targets[0].Datacenter == "dc3" {
								updatedResolver = res
								break
							}
						}
					}
					time.Sleep(100 * time.Millisecond)
				}
				So(updatedResolver, ShouldNotBeNil)

				Convey("And then deregistering the service to verify the hot-reloaded Redirect strategy", func() {
					err = client.Agent().ServiceDeregister("reload-service-1")
					So(err, ShouldBeNil)

					// Wait for the Redirect resolver pointing to DC3 to be created
					var redirectResolver *api.ServiceResolverConfigEntry
					for i := 0; i < 50; i++ {
						entry, _, err := client.ConfigEntries().Get("service-resolver", "reload-service", nil)
						if err == nil {
							if res, ok := entry.(*api.ServiceResolverConfigEntry); ok {
								if res.Redirect != nil && res.Redirect.Datacenter == "dc3" {
									redirectResolver = res
									break
								}
							}
						}
						time.Sleep(100 * time.Millisecond)
					}
					So(redirectResolver, ShouldNotBeNil)

					// Verify actual HTTP response routes to DC3
					response, err := routeRequest(client, "reload-service", localPort, dc3Port)
					So(err, ShouldBeNil)
					So(response, ShouldEqual, "remote-dc3-answering")
				})
			})
		})
	})
}
