# MoSCoW Analysis: Active Traffic Control (ATC) Roadmap

This document reviews the revised product roadmap for Active Traffic Control (ATC) after the successful completion of Hysteresis, Active-Passive HA, Hot-Reloading, WAN Federation Indicators, Expanded MCP Actions, and Target-Scoped HA Leader Locking.

---

## Must Have

### 1. Unified Consul Watcher & Namespace Watch Scoping (ADR 0004 & ADR 0023) [DONE]
- [x] Share a single [`ConsulWatcher`](pkg/atc/watcher/watcher.go) instance across `Forwarder` and `Redirector` via `Broadcaster` (with reference-counted `Run(ctx)` lifecycle) to eliminate duplicate Consul blocking queries.
- [x] Propagate `datacenter`, `token`, and dynamic `namespace` configuration updates (`SetNamespace`) into `ConsulWatcher` so catalog and health check watches are properly scoped in Consul Enterprise environments.

### 2. Unique HA Node Identity Resolution [DONE]
- [x] Separate the logical service name (`service`) from the unique node/instance identity (`node_name` / `--node-name`, defaulting to hostname suffix when `service: "atc-service"` or empty) so HA replicas sharing `service: "atc-service"` in YAML do not collide on Consul KV session lock values or misreport standby nodes as active leaders in `GetLeaderDetails`.

### 3. Configuration Precedence (CLI Flags & Env Vars vs. YAML) [DONE]
- [x] Fix Viper key bindings and flag overrides in `cmd/server.go` and `pkg/atc/config.go` (`port` -> `server.http_listen_port`, `metrics_port` -> `server.metrics_listen_port`, `mcp_port` -> `server.mcp_port`, `log_level` -> `server.log_level`, `consul_addr`, `consul_dc`, etc.) so CLI flags and `ATC_*` environment variables take precedence over values defined in the YAML config file.

### 4. Cancel Pending Dampening Timers on Leadership Loss & Shutdown [DONE]
- [x] Stop and clear all pending hysteresis `writeTimers` and `pendingWrites` (`CancelAllPendingWrites`) in `Forwarder` and `Redirector` when `Run(ctx)` exits so a node stepping down to standby cannot fire stale scheduled Consul writes.

### 5. Thread-Safe Dynamic Config Reload & Pre-Reload Validation [DONE]
- [x] Synchronize reads of `dryRun`, `consulNamespace`, and `hasDefaultFailover` under `mu` during reconciliation and scheduled writes.
- [x] Run `ValidateConfig` inside `TriggerConfigReload` and `viper.OnConfigChange` before applying reloaded configurations to runtime state.

### 6. MCP Server Streaming Timeout Fix [DONE]
- [x] Remove the 15-second `WriteTimeout` on `mcpServer` in `pkg/atc/server/main.go` so long-lived MCP SSE / streamable HTTP connections are not prematurely terminated.

---

## Should Have

### 1. Unified Audit Logging for MCP Write Tools [DONE]
- [x] Emit structured JSON audit logs (`LogAudit` / `logAudit`) for MCP write operations (`purge_redirect_config`, `apply_failover_override`, `trigger_manual_redirect`, `reload_config`) with actor, client IP, and `"source": "mcp"` metadata to match REST API audit coverage.

### 2. Authentication Hardening & Token Delegation Caching [DONE]
- [x] Use `subtle.ConstantTimeCompare` when validating static API keys in `authMiddleware`.
- [x] Add a short-lived TTL cache for delegated Consul `/v1/agent/self` token verification (`verifyDelegatedConsulToken`) to prevent per-request Consul API amplification during dashboard polling.
- [x] Extend `ValidateConfig` to error when `auth.enabled: true` is paired with default placeholder tokens (`"atc-super-secret-token"`) or no configured auth mechanism.

### 3. Consul HTTP Client Reuse & Request Body Limits [DONE]
- [x] Reuse `*api.Client` (and its underlying `http.Transport` connection pool) across reconcilers and handlers instead of creating a new client on every request or scheduled write.
- [x] Wrap JSON POST request bodies (e.g., `/api/overrides`) with `http.MaxBytesReader` (1 MiB limit).

### 4. Lightweight CLI Initialization [DONE]
- [x] Avoid initializing OpenTelemetry batch exporters, metrics gauges, and server modules when running one-shot CLI administrative commands (`atc leader status`, `atc leader force-unlock`) via `atc.NewCLI`.

### 5. Container, Helm & Demo Observability Hardening [DONE]
- [x] Update `Dockerfile` to include root CA certificates and run as a non-root user (`gcr.io/distroless/static-debian12:nonroot`).
- [x] Configure the Helm chart `livenessProbe` to use `/health` and `readinessProbe` to use `/ready` (and verify shutdown state on `/ready`).
- [x] Update `OTEL_EXPORTER_OTLP_ENDPOINT` in `atc-demo/docker-compose.yml` from port `4317` (gRPC) to `4318` (HTTP) to match the `otlp*http` exporters in `pkg/atc/telemetry`.

---

## Could Have

### 1. Frontend Parallel Polling, Token Input Debouncing & Accessibility [DONE]
- [x] Fetch `/api/leader`, `/api/federation`, `/api/strategies`, `/api/modules`, and `/api/services` concurrently using `Promise.allSettled` instead of sequential `await` calls.
- [x] Debounce API token input changes (or apply on submit/blur) so typing a token does not fire 5 parallel requests per keystroke.
- [x] Associate form `<label>` elements with input IDs via `htmlFor` in the manual override controls.

### 2. GoReleaser & Homebrew Cask Metadata Polish [DONE]
- [x] Fix the `"org.opencontainers.image.title="` trailing `=` typo and replace the `"Foo bar"` image description placeholder in `.goreleaser.yaml`.
- [x] Populate `homepage` and `description` in `.goreleaser.yaml` and `homebrew-tap/Casks/atc.rb`.

### 3. Repository & Workspace Hygiene [DONE]
- [x] Remove tracked build artifacts under `atc/dist/` and clean up workspace OS/IDE metadata files (`.DS_Store`, `.idea/`).

---

## Won't Have (For Now)

### 1. Direct Data Plane Request Proxying
It is critical to preserve ATC's role as a **control-plane orchestrator**. ATC should configure Consul, and Consul (via Envoy Connect proxies) handles the data-plane traffic. ATC should **not** load-balance or proxy network bytes directly, keeping it lightweight and out of the critical path of network traffic.

### 2. Cross-DC Target Health Validation & Failover Routing
Delegating active health-aware cross-datacenter routing to ATC's control plane introduces severe architectural anti-patterns:
- **Redundant Complexity**: Consul Connect (the Envoy data plane) already has native, sub-second active routing endpoints and load-balancing failover. Re-implementing health checks in ATC's control plane duplicates Consul's work at a much slower timescale (seconds vs milliseconds).
- **Split-Brain Risk**: A target datacenter might be reachable from ATC but unreachable from the Envoy proxies (or vice-versa). ATC making routing alterations based on its own WAN health views can cause erroneous failovers and mismatching states.
- **Operator Confusion**: Dynamically editing static global failover lists on the fly causes the actual active Consul resolver entry to deviate from the source-of-truth configuration files and service tags, making troubleshooting extremely difficult.
- **Correct Boundary**: Consul Connect handles data-plane failovers natively. ATC should restrict its role to registering/redirecting service-resolvers during catalog enrollment and catalog deletions.

### 3. Detailed Service Instance Health & Topology Visualization
Displaying individual service instance IPs, health checks, or node locations inside the ATC dashboard is out-of-scope:
- **Duplication**: Consul UI already provides an excellent, real-time interface to inspect nodes, health checks, and instances for every datacenter.
- **Observability Stack Integration**: For service telemetry, dependency maps, and overall traffic throughput health, operators look at APM tools like Grafana, utilizing Consul metrics and Prometheus data. Attempting to replicate Grafana/Consul UI inside ATC's embedded web server adds bloat and API overhead.
- **ATC Focus**: ATC's UI should remain focused purely on visualizing and managing **configured routing rules and traffic control paths** (failovers, redirects, and overrides).

### 4. Direct/Native Vault Integration inside ATC
Building native Vault API client logic directly into the ATC binary introduces unnecessary dependencies, authentication paths (AppRole, Kubernetes auth), and maintenance overhead.
- **Why**: 
  - *Orchestrator Injection*: Modern platform environments already provide native facilities for this. Nomad features a first-class `vault` block that automatically retrieves and mounts secrets into templates or environment variables. Kubernetes supports the Vault Agent Sidecar Injector or External Secrets Operators to mount Vault secrets into containers.
  - *Standalone Agents*: When running ATC on standalone VMs, operators can use the **Vault Agent** or `envconsul` to inject the Consul ACL token into environment variables (`ATC_CONSUL_TOKEN`) dynamically prior to binary execution, keeping ATC simple and lightweight.

### 5. Interactive Web UI Strategy Editor & Manual Config Reload Button
Admins originally requested an interactive web-based editor to visually draft and apply failover/redirect strategies, as well as a manual "Reload Config" button in the dashboard header. However, the declarative file-based YAML configurations (`strategies.yaml`) coupled with automatic `fsnotify` (`viper.WatchConfig`) hot-reloading works seamlessly on file updates (with `POST /api/reload` and the `reload_config` MCP tool available for automation/API triggers), keeping the UI client lightweight and focused on routing visibility.

### 6. Webhook Notification System (Slack, MS Teams, PagerDuty)
Dispatching notifications and alerts on routing state transitions or catalog updates was considered. However, this is properly the responsibility of centralized observability and alerting tools (e.g., Prometheus Alertmanager, Grafana Alerting, Consul Event alerts) monitoring the emitted metrics and structured logs, rather than embedding notification dispatch logic directly inside the ATC daemon core.
