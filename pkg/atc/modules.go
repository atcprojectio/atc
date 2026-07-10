package atc

import (
	"log/slog"
	"net/http"
	"slices"

	"github.com/atcprojectio/atc/pkg/atc/forwarder"
	mcp_server "github.com/atcprojectio/atc/pkg/atc/mcp"
	"github.com/atcprojectio/atc/pkg/atc/redirector"
	atc_server "github.com/atcprojectio/atc/pkg/atc/server"
	"github.com/atcprojectio/atc/pkg/atc/telemetry"
)

const (
	Forwarder  string = "forwarder"
	Redirector string = "redirector"
	Server     string = "server"
	Consul     string = "consul"
	All        string = "all"
)

var UserVisibleModules = []string{
	Consul,
	Forwarder,
	Redirector,
	All,
}

func (t *Atc) initServer() error {
	serv, err := atc_server.New(t.Cfg.Server, t.logger.With(slog.String("module", "server")))
	if err != nil {
		return err
	}

	if telemetry.GlobalPrometheusHandler != nil {
		serv.MetricsMux.Handle("/metrics", telemetry.GlobalPrometheusHandler)
	}

	serv.Mux.HandleFunc("/ready", OkHandler())
	serv.Mux.Handle("GET /api/services", t.authMiddleware(http.HandlerFunc(t.apiServicesHandler)))
	serv.Mux.Handle("DELETE /api/services", t.authMiddleware(http.HandlerFunc(t.apiServicesDeleteHandler)))
	serv.Mux.Handle("GET /api/leader", t.authMiddleware(http.HandlerFunc(t.apiLeaderHandler)))
	serv.Mux.Handle("GET /api/federation", t.authMiddleware(http.HandlerFunc(t.apiFederationHandler)))
	serv.Mux.Handle("POST /api/overrides", t.authMiddleware(http.HandlerFunc(t.apiOverridesHandler)))
	serv.Mux.Handle("GET /api/strategies", t.authMiddleware(http.HandlerFunc(t.apiStrategiesHandler)))
	serv.Mux.Handle("GET /api/modules", t.authMiddleware(http.HandlerFunc(t.apiModulesHandler)))
	if t.Cfg.Server.McpEnabled {
		mcpHandler := t.authMiddleware(mcp_server.NewHandler(t))
		serv.McpMux.Handle("/mcp", mcpHandler)
		serv.McpMux.Handle("/", mcpHandler)
	} else {
		disabledHandler := t.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "MCP server is disabled", http.StatusNotFound)
		}))
		serv.McpMux.Handle("/mcp", disabledHandler)
		serv.McpMux.Handle("/", disabledHandler)
	}

	t.Server = serv
	return nil
}

func (t *Atc) initForwarder() error {
	forward, err := forwarder.New(
		t.logger.With(slog.String("module", "forwarder")),
		t.Cfg.ConsulAddr,
		t.Cfg.ConsulToken,
		t.Cfg.ConsulDC,
		t.Cfg.ConsulNamespace,
		t.Cfg.WriteRateLimit,
		t.Cfg.Strategies.Failover,
		t.Cfg.DampeningPeriod,
		t.Cfg.MinDampeningPeriod,
		t.Cfg.DryRun,
	)
	if err != nil {
		return err
	}
	t.Forwarder = forward
	return nil
}

func (t *Atc) initRedirector() error {
	forwarderEnabled := t.enabledModules[Forwarder]
	_, hasDefaultFailover := t.Cfg.Strategies.Failover["default"]
	redirect, err := redirector.New(
		t.logger.With(slog.String("module", "redirector")),
		t.Cfg.ConsulAddr,
		t.Cfg.ConsulToken,
		t.Cfg.ConsulDC,
		t.Cfg.ConsulNamespace,
		t.Cfg.WriteRateLimit,
		forwarderEnabled,
		t.Cfg.Strategies.Redirect,
		t.Cfg.DampeningPeriod,
		t.Cfg.MinDampeningPeriod,
		hasDefaultFailover,
		t.Cfg.DryRun,
	)
	if err != nil {
		return err
	}
	t.Redirector = redirect
	return nil
}

// resolveModules transitively resolves module dependencies
func resolveModules(targets []string) map[string]bool {
	deps := map[string][]string{
		Consul:     {Forwarder, Redirector},
		Forwarder:  {Server},
		Redirector: {Server},
		All:        {Consul},
	}

	enabled := make(map[string]bool)
	var visit func(string)
	visit = func(mod string) {
		if enabled[mod] {
			return
		}
		enabled[mod] = true
		for _, d := range deps[mod] {
			visit(d)
		}
	}

	for _, t := range targets {
		visit(t)
	}
	return enabled
}

func (t *Atc) UserVisibleModuleNames() []string {
	names := slices.Clone(UserVisibleModules)
	slices.Sort(names)
	return names
}

func (t *Atc) DependenciesForModule(mod string) []string {
	enabled := resolveModules([]string{mod})
	deps := make([]string, 0, len(enabled))
	for k := range enabled {
		if k != mod {
			deps = append(deps, k)
		}
	}
	slices.Sort(deps)
	return deps
}
