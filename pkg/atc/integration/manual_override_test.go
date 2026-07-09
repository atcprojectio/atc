//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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

func TestManualOverrideAPI(t *testing.T) {
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

	localPort, remotePort, cleanup := startMockBackends(t)
	defer cleanup()

	Convey("Given an ATC instance and an in-process Consul server", t, func() {
		srv, err := testutil.NewTestServerConfigT(silentTestingTB{t}, func(c *testutil.TestServerConfig) {
			c.Stdout = io.Discard
			c.Stderr = io.Discard
		})
		So(err, ShouldBeNil)
		defer srv.Stop()

		srv.WaitForLeader(t)

		httpPort := getFreePort(t)
		metricsPort := getFreePort(t)

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

		Convey("When registering the service with atc.enabled=true tag", func() {
			reg := &api.AgentServiceRegistration{
				ID:   "test-service-1",
				Name: "test-service",
				Tags: []string{"atc.enabled=true"},
				Port: localPort,
			}
			err := client.Agent().ServiceRegister(reg)
			So(err, ShouldBeNil)

			// Wait for the reconciler to run and create the resolver entry with Failover
			var resolver *api.ServiceResolverConfigEntry
			for i := 0; i < 50; i++ {
				entry, _, err := client.ConfigEntries().Get("service-resolver", "test-service", nil)
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
			So(resolver.Failover, ShouldNotBeNil)

			Convey("Then client traffic should be routed to the healthy local DC1 instance", func() {
				response, err := routeRequest(client, "test-service", localPort, remotePort)
				So(err, ShouldBeNil)
				So(response, ShouldEqual, "local-dc1-answering")
			})

			Convey("And then applying a manual redirect override via HTTP API", func() {
				payload := map[string]string{
					"service":   "test-service",
					"type":      "redirect",
					"target_dc": "dc2",
				}
				payloadBytes, _ := json.Marshal(payload)

				resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/api/overrides", httpPort), "application/json", bytes.NewBuffer(payloadBytes))
				So(err, ShouldBeNil)
				defer resp.Body.Close()
				So(resp.StatusCode, ShouldEqual, http.StatusOK)

				// Wait and verify that the Consul service-resolver is updated to Redirect with manual override metadata
				var overrideResolver *api.ServiceResolverConfigEntry
				for i := 0; i < 50; i++ {
					entry, _, err := client.ConfigEntries().Get("service-resolver", "test-service", nil)
					if err == nil {
						if res, ok := entry.(*api.ServiceResolverConfigEntry); ok {
							if res.Redirect != nil && res.Meta["created-by"] == "atc-override" {
								overrideResolver = res
								break
							}
						}
					}
					time.Sleep(100 * time.Millisecond)
				}

				So(overrideResolver, ShouldNotBeNil)
				So(overrideResolver.Redirect, ShouldNotBeNil)
				So(overrideResolver.Redirect.Datacenter, ShouldEqual, "dc2")

				Convey("Then client traffic should be redirected to the backup DC2 instance", func() {
					response, err := routeRequest(client, "test-service", localPort, remotePort)
					So(err, ShouldBeNil)
					So(response, ShouldEqual, "remote-dc2-answering")
				})

				Convey("And then simulating catalog changes (reregistering the service)", func() {
					err := client.Agent().ServiceRegister(reg)
					So(err, ShouldBeNil)

					// Wait short period to ensure the automated reconciler didn't overwrite it
					time.Sleep(500 * time.Millisecond)

					entry, _, err := client.ConfigEntries().Get("service-resolver", "test-service", nil)
					So(err, ShouldBeNil)
					res := entry.(*api.ServiceResolverConfigEntry)
					So(res.Redirect, ShouldNotBeNil)
					So(res.Meta["created-by"], ShouldEqual, "atc-override")

					Convey("And then purging the override via the delete API", func() {
						req, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("http://127.0.0.1:%d/api/services?name=test-service", httpPort), nil)
						deleteResp, err := http.DefaultClient.Do(req)
						So(err, ShouldBeNil)
						defer deleteResp.Body.Close()
						So(deleteResp.StatusCode, ShouldEqual, http.StatusNoContent)

						// Trigger a catalog update event to wake up the forwarder's reconciler
						err = client.Agent().ServiceDeregister("test-service-1")
						So(err, ShouldBeNil)

						time.Sleep(100 * time.Millisecond)

						err = client.Agent().ServiceRegister(reg)
						So(err, ShouldBeNil)

						// Wait for the automated reconciler to restore the normal failover configuration
						var finalResolver *api.ServiceResolverConfigEntry
						for i := 0; i < 50; i++ {
							entry, _, err := client.ConfigEntries().Get("service-resolver", "test-service", nil)
							if err == nil {
								if res, ok := entry.(*api.ServiceResolverConfigEntry); ok {
									if len(res.Failover) > 0 && res.Meta["created-by"] == "atc" {
										finalResolver = res
										break
									}
								}
							}
							time.Sleep(100 * time.Millisecond)
						}

						So(finalResolver, ShouldNotBeNil)
						So(finalResolver.Failover, ShouldNotBeNil)
						So(finalResolver.Redirect, ShouldBeNil)

						Convey("Then client traffic should be restored back to the local DC1 instance", func() {
							response, err := routeRequest(client, "test-service", localPort, remotePort)
							So(err, ShouldBeNil)
							So(response, ShouldEqual, "local-dc1-answering")
						})
					})
				})
			})
		})
	})
}
