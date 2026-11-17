package atc

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/assert"
)

// writeCountingConsul answers like Consul with no existing resolver and counts writes.
func writeCountingConsul(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var writes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if strings.HasPrefix(r.URL.Path, "/v1/config/service-resolver/") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"Kind":"service-resolver","Name":"payments","Meta":{"created-by":"atc-override"}}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		default:
			writes.Add(1)
			_, _ = w.Write([]byte(`true`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &writes
}

func newRulesAtc(addr string, logs *bytes.Buffer) *Atc {
	logger := slog.New(slog.NewTextHandler(logs, nil))
	return &Atc{Cfg: Config{ConsulAddr: addr}, logger: logger, coreLogger: logger, consulClients: map[string]*api.Client{}}
}

func postOverride(a *Atc, body string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	a.apiOverridesHandler(rr, httptest.NewRequest(http.MethodPost, "/api/overrides", strings.NewReader(body)))
	return rr
}

func TestOverrideDurationMustBePositive(t *testing.T) {
	srv, writes := writeCountingConsul(t)
	var logs bytes.Buffer
	a := newRulesAtc(srv.Listener.Addr().String(), &logs)

	for _, d := range []string{"-5m", "0s", "10/s"} {
		rr := postOverride(a, `{"service":"payments","type":"failover","target_dc":"dc2","duration":"`+d+`"}`)
		assert.Equal(t, http.StatusBadRequest, rr.Code, "duration %q (was a 500 before)", d)
	}
	assert.Zero(t, writes.Load())
	assert.Contains(t, logs.String(), "outcome=rejected", "refused attempts are audited")
}

func TestDryRunOverridesAndPurgesSaySo(t *testing.T) {
	srv, writes := writeCountingConsul(t)
	var logs bytes.Buffer
	a := newRulesAtc(srv.Listener.Addr().String(), &logs)
	a.Cfg.DryRun = true

	rr := postOverride(a, `{"service":"payments","type":"redirect","target_dc":"dc2","duration":"5m"}`)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"status":"dry_run"`)

	rr = httptest.NewRecorder()
	a.apiServicesDeleteHandler(rr, httptest.NewRequest(http.MethodDelete, "/api/services?name=payments", nil))
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"status":"dry_run"`)

	assert.Zero(t, writes.Load(), "dry-run must not write or delete")
	assert.Contains(t, logs.String(), "outcome=dry_run")
}

func TestOverrideAuditRecordsOutcome(t *testing.T) {
	srv, _ := writeCountingConsul(t)
	var logs bytes.Buffer
	a := newRulesAtc(srv.Listener.Addr().String(), &logs)
	rr := postOverride(a, `{"service":"payments","type":"failover","target_dc":"dc2"}`)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, logs.String(), "outcome=applied")
}
