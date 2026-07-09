//go:build integration

package integration

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/hashicorp/consul/api"
)

type silentTestingTB struct {
	testing.TB
}

func (s silentTestingTB) Log(args ...any)                  {}
func (s silentTestingTB) Logf(format string, args ...any)  {}

func getFreePort(t testing.TB) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func startMockBackends(t testing.TB) (localPort, remotePort int, cleanup func()) {
	localServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("local-dc1-answering"))
	}))

	localURL, err := url.Parse(localServer.URL)
	if err != nil {
		localServer.Close()
		t.Fatalf("failed to parse local server url: %v", err)
	}
	lPort, err := strconv.Atoi(localURL.Port())
	if err != nil {
		localServer.Close()
		t.Fatalf("failed to parse local server port: %v", err)
	}

	remoteServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("remote-dc2-answering"))
	}))

	remoteURL, err := url.Parse(remoteServer.URL)
	if err != nil {
		localServer.Close()
		remoteServer.Close()
		t.Fatalf("failed to parse remote server url: %v", err)
	}
	rPort, err := strconv.Atoi(remoteURL.Port())
	if err != nil {
		localServer.Close()
		remoteServer.Close()
		t.Fatalf("failed to parse remote server port: %v", err)
	}

	cleanup = func() {
		localServer.Close()
		remoteServer.Close()
	}

	return lPort, rPort, cleanup
}

func routeRequest(client *api.Client, serviceName string, localPort, remotePort int) (string, error) {
	entry, _, err := client.ConfigEntries().Get("service-resolver", serviceName, nil)
	if err != nil {
		return "", err
	}
	resolver, ok := entry.(*api.ServiceResolverConfigEntry)
	if !ok {
		return "", fmt.Errorf("invalid config entry type")
	}

	// 1. Check if Redirection is active
	if resolver.Redirect != nil {
		if resolver.Redirect.Datacenter != "" {
			return makeHTTPGet(remotePort)
		}
		return makeHTTPGet(localPort)
	}

	// 2. Check if Failover is active
	if resolver.Failover != nil {
		checks, _, err := client.Health().Service(serviceName, "", true, nil)
		if err == nil && len(checks) > 0 {
			return makeHTTPGet(localPort)
		}
		if fo, ok := resolver.Failover["*"]; ok && len(fo.Targets) > 0 {
			targetDC := fo.Targets[0].Datacenter
			if targetDC != "" {
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
