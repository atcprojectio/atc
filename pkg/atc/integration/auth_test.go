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

func TestAuthAndTokenDelegation(t *testing.T) {
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

	Convey("Given an ATC instance with authentication enabled and a Consul server", t, func() {
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
			Auth: atc.AuthConfig{
				Enabled:                true,
				StaticKeys:             []string{"atc-static-token"},
				ConsulTokenDelegation: true,
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

		// Register service to set up resolver
		reg := &api.AgentServiceRegistration{
			ID:   "auth-service-1",
			Name: "auth-service",
			Tags: []string{"atc.enabled=true"},
			Port: localPort,
		}
		err = client.Agent().ServiceRegister(reg)
		So(err, ShouldBeNil)

		// Wait for initial failover resolver to be created
		var resolver *api.ServiceResolverConfigEntry
		for i := 0; i < 50; i++ {
			entry, _, err := client.ConfigEntries().Get("service-resolver", "auth-service", nil)
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

		payload := map[string]string{
			"service":   "auth-service",
			"type":      "redirect",
			"target_dc": "dc2",
		}
		payloadBytes, _ := json.Marshal(payload)

		Convey("When sending an override request without credentials", func() {
			resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/api/overrides", httpPort), "application/json", bytes.NewBuffer(payloadBytes))
			So(err, ShouldBeNil)
			defer resp.Body.Close()

			Convey("Then it should fail with 401 Unauthorized", func() {
				So(resp.StatusCode, ShouldEqual, http.StatusUnauthorized)
			})
		})

		Convey("When sending an override request with a valid static key", func() {
			req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/api/overrides", httpPort), bytes.NewBuffer(payloadBytes))
			So(err, ShouldBeNil)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer atc-static-token")

			resp, err := http.DefaultClient.Do(req)
			So(err, ShouldBeNil)
			defer resp.Body.Close()

			Convey("Then it should succeed with 200 OK", func() {
				So(resp.StatusCode, ShouldEqual, http.StatusOK)
			})
		})

		Convey("When sending an override request with a delegated Consul token", func() {
			req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/api/overrides", httpPort), bytes.NewBuffer(payloadBytes))
			So(err, ShouldBeNil)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Consul-Token", "valid-consul-acl-token")

			resp, err := http.DefaultClient.Do(req)
			So(err, ShouldBeNil)
			defer resp.Body.Close()

			Convey("Then it should succeed with 200 OK and update the traffic response to remote DC2", func() {
				So(resp.StatusCode, ShouldEqual, http.StatusOK)

				// Wait and verify that the Consul service-resolver is updated to Redirect with manual override metadata
				var overrideResolver *api.ServiceResolverConfigEntry
				for i := 0; i < 50; i++ {
					entry, _, err := client.ConfigEntries().Get("service-resolver", "auth-service", nil)
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

				// Verify actual HTTP response of the service under test
				response, err := routeRequest(client, "auth-service", localPort, remotePort)
				So(err, ShouldBeNil)
				So(response, ShouldEqual, "remote-dc2-answering")
			})
		})
	})
}
