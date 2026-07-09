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

func TestHysteresisDampening(t *testing.T) {
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

	Convey("Given an ATC instance with dampening and a Consul server", t, func() {
		srv, err := testutil.NewTestServerConfigT(silentTestingTB{t}, func(c *testutil.TestServerConfig) {
			c.Stdout = io.Discard
			c.Stderr = io.Discard
		})
		So(err, ShouldBeNil)
		defer srv.Stop()

		srv.WaitForLeader(t)

		httpPort := getFreePort(t)
		metricsPort := getFreePort(t)

		// Configure with a 2-second dampening period
		cfg := atc.Config{
			Name:               "integration-test-node",
			ConsulAddr:         srv.HTTPAddr,
			Target:             []string{"forwarder", "redirector"},
			DampeningPeriod:    "2s",
			MinDampeningPeriod: "2s",
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

		Convey("When rapidly registering and deregistering the service (flapping)", func() {
			reg := &api.AgentServiceRegistration{
				ID:   "flap-service-1",
				Name: "flap-service",
				Tags: []string{"atc.enabled=true"},
				Port: localPort,
			}

			// Flapping loop: register/deregister rapidly, ending on a register
			for i := 0; i < 5; i++ {
				err = client.Agent().ServiceRegister(reg)
				So(err, ShouldBeNil)
				time.Sleep(100 * time.Millisecond)

				if i < 4 {
					err = client.Agent().ServiceDeregister("flap-service-1")
					So(err, ShouldBeNil)
					time.Sleep(100 * time.Millisecond)
				}
			}

			// Assert that during this rapid flapping, no config entry was created yet
			// because the 2s dampening period prevents immediate writes.
			entry, _, err := client.ConfigEntries().Get("service-resolver", "flap-service", nil)
			So(err, ShouldNotBeNil) // Should return "not found" error
			So(entry, ShouldBeNil)

			// Wait for the 2-second dampening period to expire
			time.Sleep(2500 * time.Millisecond)

			// Assert that the final state (Failover, since the service was left registered)
			// has been successfully written once.
			var finalResolver *api.ServiceResolverConfigEntry
			for i := 0; i < 10; i++ {
				entry, _, err = client.ConfigEntries().Get("service-resolver", "flap-service", nil)
				if err == nil {
					if res, ok := entry.(*api.ServiceResolverConfigEntry); ok {
						if len(res.Failover) > 0 {
							finalResolver = res
							break
						}
					}
				}
				time.Sleep(100 * time.Millisecond)
			}

			Convey("Then the intermediate writes should be dampened and only the final state written", func() {
				So(finalResolver, ShouldNotBeNil)
				So(finalResolver.Failover, ShouldNotBeNil)
				So(finalResolver.Failover["*"].Targets[0].Datacenter, ShouldEqual, "dc2")
				So(finalResolver.Redirect, ShouldBeNil)

				// Verify actual HTTP response of the service under test
				response, err := routeRequest(client, "flap-service", localPort, remotePort)
				So(err, ShouldBeNil)
				So(response, ShouldEqual, "local-dc1-answering")
			})
		})
	})
}
