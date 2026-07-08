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

type silentTestingTB struct {
	testing.TB
}

func (s silentTestingTB) Log(args ...any)                  {}
func (s silentTestingTB) Logf(format string, args ...any)  {}

func TestServiceResolverLifecycle(t *testing.T) {
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

	// Start local mock HTTP server (DC1 backend)
	localServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("local-dc1-answering"))
	}))
	defer localServer.Close()

	localURL, _ := url.Parse(localServer.URL)
	localPort, _ := strconv.Atoi(localURL.Port())

	// Start remote mock HTTP server (DC2 backend / "other" service)
	remoteServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("remote-dc2-answering"))
	}))
	defer remoteServer.Close()

	remoteURL, _ := url.Parse(remoteServer.URL)
	remotePort, _ := strconv.Atoi(remoteURL.Port())

	Convey("Given an ATC instance and an in-process Consul server", t, func() {
		srv, err := testutil.NewTestServerConfigT(silentTestingTB{t}, func(c *testutil.TestServerConfig) {
			c.Stdout = io.Discard
			c.Stderr = io.Discard
		})
		So(err, ShouldBeNil)
		defer srv.Stop()

		srv.WaitForLeader(t)

		cfg := atc.Config{
			Name:               "integration-test-node",
			ConsulAddr:         srv.HTTPAddr,
			Target:             []string{"forwarder", "redirector"},
			DampeningPeriod:    "0s",
			MinDampeningPeriod: "0s",
			WriteRateLimit:     "0s",
			Server: atc_server.Config{
				LogLevel: "error",
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
			So(resolver.Redirect, ShouldBeNil)
			So(resolver.Failover, ShouldNotBeNil)
			So(resolver.Failover["*"].Targets[0].Datacenter, ShouldEqual, "dc2")

			Convey("Then client traffic should be routed to the healthy local DC1 instance", func() {
				response, err := routeRequest(client, localPort, remotePort)
				So(err, ShouldBeNil)
				So(response, ShouldEqual, "local-dc1-answering")
			})

			Convey("And then removing the service from the catalog", func() {
				err := client.Agent().ServiceDeregister("test-service-1")
				So(err, ShouldBeNil)

				// Wait for redirector to detect deletion and set Redirect
				var redirectResolver *api.ServiceResolverConfigEntry
				for i := 0; i < 50; i++ {
					entry, _, err := client.ConfigEntries().Get("service-resolver", "test-service", nil)
					if err == nil {
						if res, ok := entry.(*api.ServiceResolverConfigEntry); ok {
							if res.Redirect != nil {
								redirectResolver = res
								break
							}
						}
					}
					time.Sleep(100 * time.Millisecond)
				}

				So(redirectResolver, ShouldNotBeNil)
				So(redirectResolver.Failover, ShouldBeNil)
				So(redirectResolver.Redirect, ShouldNotBeNil)
				So(redirectResolver.Redirect.Datacenter, ShouldEqual, "dc2")

				Convey("Then client traffic should be redirected to the backup DC2 instance during outage", func() {
					response, err := routeRequest(client, localPort, remotePort)
					So(err, ShouldBeNil)
					So(response, ShouldEqual, "remote-dc2-answering")
				})

				Convey("And then adding the original service again", func() {
					err := client.Agent().ServiceRegister(reg)
					So(err, ShouldBeNil)

					// Wait for forwarder to set Failover again
					var finalResolver *api.ServiceResolverConfigEntry
					for i := 0; i < 50; i++ {
						entry, _, err := client.ConfigEntries().Get("service-resolver", "test-service", nil)
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

					So(finalResolver, ShouldNotBeNil)
					So(finalResolver.Redirect, ShouldBeNil)
					So(finalResolver.Failover, ShouldNotBeNil)

					Convey("Then client traffic should be restored back to the local DC1 instance", func() {
						response, err := routeRequest(client, localPort, remotePort)
						So(err, ShouldBeNil)
						So(response, ShouldEqual, "local-dc1-answering")
					})
				})
			})
		})
	})
}

func routeRequest(client *api.Client, localPort, remotePort int) (string, error) {
	entry, _, err := client.ConfigEntries().Get("service-resolver", "test-service", nil)
	if err != nil {
		return "", err
	}
	resolver, ok := entry.(*api.ServiceResolverConfigEntry)
	if !ok {
		return "", fmt.Errorf("invalid config entry type")
	}

	// 1. Check if Redirection is active
	if resolver.Redirect != nil {
		if resolver.Redirect.Datacenter == "dc2" {
			return makeHTTPGet(remotePort)
		}
		return makeHTTPGet(localPort)
	}

	// 2. Check if Failover is active
	if resolver.Failover != nil {
		checks, _, err := client.Health().Service("test-service", "", true, nil)
		if err == nil && len(checks) > 0 {
			return makeHTTPGet(localPort)
		}
		if fo, ok := resolver.Failover["*"]; ok && len(fo.Targets) > 0 {
			targetDC := fo.Targets[0].Datacenter
			if targetDC == "dc2" {
				return makeHTTPGet(remotePort)
			}
		}
	}

	return makeHTTPGet(localPort)
}

func makeHTTPGet(port int) (string, error) {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d", port))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}
