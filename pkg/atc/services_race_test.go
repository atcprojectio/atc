package atc

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/hashicorp/consul/api"
)

// TestServicesHandlerConcurrentWithReload must pass under -race: the handler used to read
// t.Cfg.ConsulNamespace without the config lock while ReloadConfig replaced t.Cfg.
func TestServicesHandlerConcurrentWithReload(t *testing.T) {
	consul := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/catalog/services":
			_, _ = w.Write([]byte(`{"payments":["atc.enabled=true"]}`))
		case "/v1/config/service-resolver":
			_, _ = w.Write([]byte(`[{"Kind":"service-resolver","Name":"gone","Meta":{"created-by":"atc","redirect-strategy":"x"},"Redirect":{"Service":"gone","Datacenter":"dc2"}}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer consul.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := Config{ConsulAddr: consul.Listener.Addr().String(), ConsulNamespace: "ns-a"}
	a := &Atc{Cfg: cfg, logger: logger, coreLogger: logger, consulClients: map[string]*api.Client{}}

	stop := make(chan struct{})
	reloaded := make(chan struct{})
	go func() {
		defer close(reloaded)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			c := cfg
			if i%2 == 0 {
				c.ConsulNamespace = "ns-b"
			}
			a.ReloadConfig(c)
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.apiServicesHandler(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/services", nil))
		}()
	}
	wg.Wait()
	close(stop)
	<-reloaded
}
