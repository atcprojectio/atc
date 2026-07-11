# Architecture Decision Records (ADR) - Active Traffic Control (ATC)

This document records the key architectural decisions made during the design, development, and modernization of the Active Traffic Control (ATC) project.

---

## ADR 0001: Remove `dskit` and Standardise Concurrency

### Status
Accepted

### Date
2026-06-19

### Context
The legacy ATC service relied on Grafana's `dskit` framework, which introduced a substantial dependency footprint (over 100 indirect dependencies) and forced a rigid, heavyweight modular lifecycle architecture.

### Decision
Replace all Grafana `dskit` components with Go standard library primitives:
- Run two parallel, isolated `http.Server` listeners: main application port `:8088` and Prometheus metrics scrape port `:8089`.
- Manage lifecycle shutdown signals with standard `os/signal` listening for `syscall.SIGTERM` and `os.Interrupt`.
- Run concurrent background loops using `golang.org/x/sync/errgroup`.

### Consequences
- **Benefits**:
  - Dramatic reduction in the codebase dependency footprint, enhancing build speed and binary portability.
  - Standardized concurrency model aligning with idiomatic Go.
- **Consequences**:
  - Standard library implementations must be manually configured (e.g. timeout settings, CORS handling).

---

## ADR 0002: Migrate Logging to `log/slog`

### Status
Accepted

### Date
2026-06-19

### Context
Legacy structured logging was managed via `go-kit/log` and custom `dskit` wrapper libraries, which added extra dependency weight and deviated from modern Go logging standards.

### Decision
Migrate the entire logging infrastructure to Go's standard library `log/slog` package, injecting subsystem metadata (e.g. `module=forwarder`, `module=redirector`) via contextual attributes.

### Consequences
- **Benefits**:
  - Out-of-the-box structured json/text logging with zero third-party dependencies.
  - Native integration with standard context propagation.
- **Consequences**:
  - Requires updating legacy log format signatures throughout the codebase.

---

## ADR 0003: Prune Unused Submodules

### Status
Accepted

### Date
2026-06-19

### Context
The legacy application possessed multiple complex submodules (`autoscaler`, `deployer`, `event_sink`, `incident`, and `radar`) that were unused, out-of-scope, and cluttered the repository.

### Decision
Prune all unused packages from the repository, keeping only the core modules: `server`, `forwarder`, and `redirector`.

### Consequences
- **Benefits**:
  - Smaller repository size and reduced cognitive load for new developers.
  - Tight focus on core Consul gateway routing automation.
- **Consequences**:
  - Removed features must be re-implemented if their use-case returns.

---

## ADR 0004: Consolidate Consul Watcher Logic

### Status
Accepted

### Date
2026-06-19

### Context
Separate background modules registered their own polling routines against the Consul catalog, generating redundant client calls and potential consistency lags.

### Decision
Centralize the Consul watch routines into a single unified helper in `pkg/atc/watcher/watcher.go`. It polls catalog changes and health check states simultaneously and broadcasts events via a thread-safe `Broadcaster` channel subscription mechanism.

### Consequences
- **Benefits**:
  - Eliminated redundant network polling of the Consul API.
  - Synchronized state events across reconcilers.
- **Consequences**:
  - The watcher becomes a single point of failure for catalog reconciliation.

---

## ADR 0005: Coordinated Consul Config Reconciliation

### Status
Accepted

### Date
2026-06-19

### Context
Both the `forwarder` (handling active failovers) and `redirector` (handling offline redirects) write configuration updates to the same Consul `service-resolver` entries, risking race conditions and split-brain configurations.

### Decision
Introduce a `forwarderEnabled` configuration flag in the `redirector`'s constructor. When set to `true`, the redirector is instructed to skip writing base/empty config entries for active services, preventing them from overwriting active configurations written by the forwarder.

### Consequences
- **Benefits**:
  - Prevents race conditions and guarantees configuration consistency.
  - Clean operational boundaries between active and offline state loops.
- **Consequences**:
  - Reconcilers must check the state of other modules before executing changes.

---

## ADR 0006: Embedded React UI Dashboard

### Status
Accepted

### Date
2026-06-19

### Context
Operators required a user-friendly way to audit cluster failover paths, active service tags, and clean up stale resolver configurations.

### Decision
Develop a React-based frontend dashboard (scaffolded with Vite) and embed the compiled production assets directly inside the Go server using `go:embed`.

### Consequences
- **Benefits**:
  - Single-binary distribution containing both the server daemon and the administration UI.
  - Real-time visual mapping of traffic flow.
- **Consequences**:
  - Building the Go binary requires a Node.js compilation step for frontend assets.

---

## ADR 0007: Native Model Context Protocol (MCP) Server

### Status
Accepted

### Date
2026-06-19

### Context
In addition to a human-friendly UI, the ATC service needed to be easily inspectable and manageable by AI agents and automated coding assistants.

### Decision
Expose a streamable Model Context Protocol (MCP) server over HTTP at `/mcp` using the official MCP Go SDK, registering tools to list services, check readiness, and purge configurations.

### Consequences
- **Benefits**:
  - AI agents can inspect, audit, and clean up routing configurations natively.
- **Consequences**:
  - Requires custom CORS middleware configuration to support Electron-based desktop clients (e.g. Claude Desktop).

---

## ADR 0008: Containerised Local Dev Environment

### Status
Accepted

### Date
2026-06-19

### Context
Local development originally required running native system Consul processes, which made setup platform-dependent and prone to host ports conflicts.

### Decision
Refactor all Makefile helper targets to run a local dev Consul agent inside a Docker background container, mocking catalog state changes dynamically using `curl` against the container HTTP API.

### Consequences
- **Benefits**:
  - Platform-agnostic, zero-install local developer setup.
- **Consequences**:
  - Requires Docker daemon running on the host machine.

---

## ADR 0009: Active-Passive High Availability (HA) via Consul Locks

### Status
Accepted

### Date
2026-06-19

### Context
Designing for High Availability (HA) in production should not introduce complex stateful storage or consensus ring overhead (like an embedded Raft log) in the ATC daemons themselves.

### Decision
Implement leader election using **Consul KV Session locks** (active-passive mode) instead of Raft consensus. The standby daemons will watch the lock key and transition to active if the leader session expires.

### Consequences
- **Benefits**:
  - ATC daemons remain completely stateless (no PVCs, stateful sets, or Raft peer configurations).
  - Simple horizontal scaling via Kubernetes deployments.
- **Consequences**:
  - ATC depends entirely on Consul's health for leader election.

---

## ADR 0010: Defer Active Health Routing to Consul Connect Data Plane

### Status
Accepted

### Date
2026-06-19

### Context
Determining whether ATC should dynamically update failover lists on the fly based on active WAN checks, or defer load-balancing health decisions.

### Decision
Keep failover target lists statically defined in the strategies configuration and defer active data-plane health routing entirely to Consul Connect (Envoy proxies). ATC will restrict its role to registering/redirecting service-resolvers during catalog updates.

### Consequences
- **Benefits**:
  - Prevents split-brain routing states between ATC's control plane view and Envoy's data plane view.
  - Keeps the network data path functional even if the ATC control plane is down.
- **Consequences**:
  - Changes to failover target priorities must be updated via configuration files.

---

## ADR 0011: WAN Federation Indicators instead of Detailed Health Visualization

### Status
Accepted

### Date
2026-06-19

### Context
Defining the visualization scope of the React dashboard without duplicating Consul UI's detailed node/instance metrics or Grafana's telemetry charts.

### Decision
Display only configured routing paths and a lightweight Target Datacenter WAN Federation Status Indicator (fetched from Consul's WAN members list), rather than detailed instance health logs.

### Consequences
- **Benefits**:
  - Keeps the ATC UI focused solely on configured routing policies.
  - Low API overhead and memory footprint.
- **Consequences**:
  - Operators must consult Consul UI or Grafana for container/instance level diagnostics.

---

## ADR 0012: Local Client-Side Routing Simulation for Validation

### Status
Accepted

### Date
2026-06-19

### Context
Running full Envoy sidecars, Connect CA controllers, and Consul proxies in local Docker Compose generates massive CPU overhead and slow startup latencies for validation testing.

### Decision
Use lightweight `hashicorp/http-echo` containers as mock targets and a client-side Python simulation script to query Consul and validate routing paths.

### Consequences
- **Benefits**:
  - Extremely fast validation feedback loops.
  - Low system resources consumption during local dev.
- **Consequences**:
  - Does not validate Envoy proxy binary behavior directly.

---

## ADR 0013: OpenTelemetry Integration for Observability

### Status
Accepted

### Date
2026-06-19

### Context
Standardized application telemetry (metrics, traces, logs) is required to diagnose traffic control actions, catalog sync cycles, and connection latency in production.

### Decision
- **SDK**: Adopt the standard OpenTelemetry Go SDK to emit trace spans and runtime metrics.
- **Exporters**: Export signals using the OTLP format to an OpenTelemetry Collector.
- **Instrumentation**: Auto-instrument trace scopes for HTTP handlers, and manually instrument reconciliation loops.
- **Correlations**: Inject trace and span context into structured `slog` messages.

### Consequences
- **Benefits**:
  - Enhanced operational transparency and standard telemetry reporting.
  - Easier root-cause analysis for routing adjustments.
- **Consequences**:
  - Small runtime tracing latency and trace volumes.

---

## ADR 0014: Predefined Redirection and Failover Strategies

### Status
Accepted

### Date
2026-06-19

### Context
Service owners need template-based patterns for failover and redirect configurations instead of writing complex targets on individual services.

### Decision
- **Strategies**: Define admin-controlled named failover and redirect strategies in a local YAML configuration (`strategies.yaml`).
- **Load Configuration**: Expose flags to load configuration at startup.
- **Consul Tags**: Enable service owners to apply these named strategies via standard Consul service catalog tags: `atc.failover=<strategy>` and `atc.redirect=<strategy>`.

### Consequences
- **Benefits**:
  - Consistent routing policies across teams.
  - Simplified tag-based onboarding.
- **Consequences**:
  - Changes to templates require config reloading.

---

## ADR 0015: Strict Consul Service Resolver Metadata Configuration and In-Memory Cache

### Status
Accepted

### Date
2026-06-19

### Context
Storing both redirect and failover strategies in the Consul `service-resolver` metadata is misleading because only one strategy is active at any time. However, when a service goes offline, its catalog tags disappear, making the redirector unable to resolve the correct redirect strategy.

### Decision
- **Metadata Config**: Enforce strict metadata configuration: write only the active strategy to the Consul `service-resolver` metadata (`failover-strategy` for active failovers, `redirect-strategy` for deleted service redirects).
- **In-Memory Cache**: Introduce an in-memory thread-safe `strategiesCache` in reconcilers to store configuration strategies parsed from active catalog scans.
- **Fallback Resolution**: Check the in-memory cache first to resolve redirect strategies for deleted services, falling back to existing metadata.
- **API & UI Sync**: Update the dashboard `/api/services` and React UI to merge these cached strategies, showing both strategy names for active and deleted services.

### Consequences
- **Benefits**:
  - Accurate metadata state in Consul.
  - API/UI dashboard continues to show complete configurations.
- **Consequences**:
  - Simultaneous restart of daemon and catalog deletion defaults to default redirects.

---

## ADR 0016: Custom Zero-Dependency Documentation Website and GitHub Pages Publishing

### Status
Accepted

### Date
2026-06-19

### Context
Need robust project documentation that can double as a user-facing project website, with automated deployment and low dependency overhead.

### Decision
- **Site Structure**: Build a modern dark-mode single-page portal inside a `docs/` folder using raw HTML, CSS, and JS (leveraging Google Fonts, FontAwesome, and Prism.js).
- **Publishing Pipeline**: Deploy it using GitHub Actions (`.github/workflows/pages.yml`) to GitHub Pages on every push to `main` branch under the `docs/` path.
- **Homebrew Tap**: Distribute macOS/Linux releases via Homebrew in an official tap (`atcprojectio/tap`).

### Consequences
- **Benefits**:
  - Fast website performance.
  - Zero build dependencies for documentation.
  - Automatic deployment on push.
- **Consequences**:
  - Content written in HTML instead of markdown, requiring structured tags.

---

## ADR 0017: Configuration Validation and Linting Subcommand

### Status
Accepted

### Date
2026-06-22

### Context
Before applying configuration changes (like pre-defined failover/redirect strategies, dampening periods, or HA settings) in production or hot-reloading them, platform operators need to verify that their configuration files are syntactically and semantically correct. Doing validation only at startup or during runtime triggers errors that can cause application restarts to fail or leader elections to malfunction.

### Decision
Implement a dedicated config validation capability:
- **Core Engine**: Create a standalone `ValidateConfig` function to run semantic checks against the parsed configuration model.
- **CLI Subcommand**: Expose a subcommand (`atc validate --config strategies.yaml`) that performs the validation.
- **Exit Behavior**: On success, exit with code `0`. On failure, write detailed error messages (including invalid duration syntax, out-of-bounds HA session TTLs, invalid target modules, or missing redirect targets) to standard error and exit with code `1`.
- **CI/CD Integration**: Promote running the `validate` command inside pre-commit hooks and Pull Request pipelines to lint configuration files as part of GitOps.

### Consequences
- **Benefits**:
  - Prevents bad/flapping configuration updates from being pushed to production.
  - Catches invalid Consul session TTL constraints early before they fail at the Consul Lock level.
  - Clear integration path for GitOps CI/CD pipelines.
- **Consequences**:
  - Increases the CLI surface area with a new subcommand.

---

## ADR 0018: Defer Request Proxying to Envoy (Data Plane)

### Status
Rejected

### Date
2026-06-22

### Context
ATC handles service-resolver configuration, but platform teams want to clarify if ATC should ever handle direct data plane load balancing or proxying of request bytes.

### Decision
ATC will strictly remain a control-plane orchestrator. All request proxying and data-plane load-balancing logic is deferred to Consul Connect and Envoy proxies. ATC will not implement proxying logic, keeping the binary lightweight and keeping it out of the critical network path.

### Consequences
- **Benefits**:
  - Keep ATC simple and stateless.
  - Zero performance impact on active network traffic.
- **Consequences**:
  - Operators must configure Envoy sidecars for data plane traffic routing.

---

## ADR 0019: Defer Vault Secret Injection to Orchestrator or Agent wrappers

### Status
Rejected

### Date
2026-06-22

### Context
ATC requires a Consul ACL token. Platform teams requested a native Vault client integration to fetch this token directly from Vault.

### Decision
Instead of embedding a native Vault API client and handling multiple authentication patterns inside ATC, secret retrieval is deferred to orchestrator-native injection (such as Kubernetes Vault Sidecar, Nomad vault block, or External Secrets Operator) or VM-level runners (like Vault Agent or `envconsul` wrapper).

### Consequences
- **Benefits**:
  - Avoids embedding heavy Vault SDK dependencies.
  - No new authentication options or configuration parameters need to be supported inside ATC.
- **Consequences**:
  - Operators must use wrapper scripts or orchestrator integrations to inject secrets.

---

## ADR 0020: Publish versioned Helm charts to GitHub Container Registry (GHCR) as OCI Artifacts

### Status
Accepted

### Date
2026-06-23

### Context
ATC includes a Helm chart for deployment inside Kubernetes clusters. To make the installation standard and seamless, versioned releases of the chart should be distributed through an official repository/registry. Traditional chart registries require maintaining a separate `gh-pages` branch and index file, whereas Helm 3 natively supports storing chart packages in standard OCI registries.

### Decision
Publish official ATC Helm chart releases directly to GitHub Container Registry (GHCR) as OCI registry artifacts:
- **Workflow**: Integrate Helm packaging and publishing directly into [.github/workflows/publish.yml](file:///Users/attachmentgenie/DevShed/Projects/atcprojectio/atc/.github/workflows/publish.yml), running on push tag events.
- **Dynamic Versioning**: Use the Git tag (`GITHUB_REF_NAME`) to inject semver chart versions (`--version`) and application versions (`--app-version`) dynamically at packaging time.
- **Login & Registry**: Use `helm registry login ghcr.io` using the built-in `GITHUB_TOKEN` secrets context and push to `oci://ghcr.io/${OWNER}/charts/atc`.

### Consequences
- **Benefits**:
- Users must use Helm OCI protocols (`oci://` URLs) to resolve and install the chart instead of traditional HTTP repositories.

---

## ADR 0021: Native Authentication, JSON Audit Logging, and Predefined Strategies API

### Status
Accepted

### Date
2026-06-23

### Context
To protect ATC daemon core operations in production, both REST API and MCP endpoints must be secured. Operations like manual overrides and purges require tracking through high-priority audit logs. Finally, pre-defined strategies need to be exposed via APIs so that dashboards and external tooling can inspect available routing strategies.

### Decision
- **Auth Middleware**: Implement an `authMiddleware` that intercepts requests to REST and MCP endpoints (except `/health` and `/ready`), validating tokens using static keys or delegating verification to Consul's `/v1/agent/self` API.
- **Audit Logging**: Implement a structured `logAudit` function that logs manual overrides and purges in JSON to standard output/error, masking sensitive tokens.
- **Predefined Strategies API**: Expose `/api/strategies` and the `list_strategies` MCP tool to return active predefined strategies.
- **Dashboard Updates**: Update the React dashboard to support token management, localStorage persistence, header injection, and displaying predefined strategies.

### Consequences
- **Benefits**:
  - Secure API and MCP access.
  - Simplified RBAC through Consul token delegation.
  - Improved compliance and troubleshooting using structured JSON audit logs.
- **Consequences**:
  - Slightly higher request processing overhead due to middleware checks.
  - Operators must configure API keys or delegate access when enabling authentication.

---

## ADR 0022: OpenTelemetry Trace Event propagation, Dry-Run Mode, Manual Override TTL, and Config Reload

### Status
Accepted

### Date
2026-06-23

### Context
Operators need visibility into routing decisions, the ability to test modifications without writing to Consul (dry-run), dynamic configuration reloads without restarting the daemon, and automatic expiration of manual overrides to avoid stale routes.

### Decision
- **OTel Trace Propagation**: Record trace span events during service resolver writes, deletes, and configuration hot-reloads using package-level `coreTracer`.
- **Dry-Run Mode**: Introduce a `--dry-run` CLI flag and `dry_run` configuration key to bypass Consul modifications in forwarder, redirector, and purges, logging the bypassed decisions instead.
- **Manual Override TTL Expiration**: Support optional duration inputs in manual overrides (`ApplyFailoverOverride` / `TriggerManualRedirect`), store expiration timestamps (`atc-override-expires-at`) in config entry metadata, and run a background loop sweeper in ATC that purges expired overrides.
- **Config Reload API**: Register `POST /api/reload` and MCP tool `reload_config` to dynamically trigger viper reload and config re-unmarshalling.
- **Frontend Integration**: Display expiration badges on card overrides, expose control form overlays to set TTL when applying manual overrides, and add a reload configuration button in the header.

### Consequences
- **Benefits**:
  - Risk-free testing of catalog routing changes via dry-run mode.
  - Automatic cleanup of overrides preventing stale routing configurations.
  - Rich OpenTelemetry observability and dynamic reload capability.
- **Consequences**:
  - Overhead of background sweep goroutine querying Consul for active overrides.
  - Expired overrides require ATC leader instance to perform deletions.

---

## ADR 0023: Consul Enterprise Namespace Support and Failover Rate Limiting

### Status
Accepted

### Date
2026-06-23

### Context
When running Consul in Enterprise environments, catalog services, health checks, and service-resolver config entries are segmented into administrative namespaces. To support these environments, ATC must scope watcher plans and config operations to the target namespace. Additionally, during cluster flapping or deployment changes, catalog event streams can fire in rapid succession, leading to API write thrashing. Coalescing/debouncing these events avoids catalog database thrashing.

### Decision
- **Consul Namespace Scoping**: Add `consul_namespace` configuration parameter and `--consul-namespace` CLI flag. Configure the base Consul `api.Config` Namespace field so that all config-entry reads/writes/deletes are scoped. Pass `"namespace": namespace` in Consul `watch.Parse` parameters to scope catalog/health changes to the same boundary.
- **Failover Rate Limiting (Coalescing)**: Implement a coalescing event loop in `Forwarder` and `Redirector` using a debounce timer (`time.Timer`). If watcher events arrive in rapid succession, postpone `reconcile` execution by the configured `write_rate_limit` duration (default "1s"), batching multiple catalog events into a single update.
- **Cross-Namespace Overrides**: Accept optional target namespaces in manual overrides and strategies, writing them to target `ServiceResolverConfigEntry` destination resolvers.
- **Frontend Badges**: Render namespace badges next to service names on service cards in the UI dashboard when they belong to non-default namespaces.

### Consequences
- **Benefits**:
  - Full compatibility with Consul Enterprise administrative boundaries.
  - Reduced write pressure and API write thrashing on the Consul leader agent during service flaps.
  - Visibility into namespace ownership directly from the React dashboard.
- **Consequences**:
  - Catalog updates are delayed by the rate-limit window (e.g. 1 second), introducing a small reconciliation lag.

---

## ADR 0024: Defer Strategy Editing to Declarative Config Files and GitOps

### Status
Rejected

### Date
2026-06-23

### Context
Admins requested a visual, web-based strategy editor in the React UI dashboard to visually draft, edit, and apply failover/redirect strategies.

### Decision
Reject building an interactive visual strategy editor inside the Web UI dashboard. Instead, strategies will remain declaratively defined in the central configuration file (`strategies.yaml`) and loaded at startup/reload. Any updates to strategies will be handled by updating the file (which supports hot-reloading via `/api/reload` or SIGHUP), encouraging GitOps workflows.

### Consequences
- **Benefits**:
  - Keeps the dashboard UI lightweight, simple, and read-only.
  - Aligns with infrastructure-as-code and GitOps principles (all strategies are tracked in version control).
  - Avoids state synchronization issues or write conflicts between multiple operators editing strategies concurrently.
- **Consequences**:
  - Operators must edit configuration files directly (via text editors or Git commits) rather than updating them via the dashboard.

---

## ADR 0025: Defer Webhook Notification Dispatch to External Observability Stack

### Status
Rejected

### Date
2026-06-23

### Context
Operators requested that the ATC daemon directly dispatch webhook alerts to Slack, Microsoft Teams, or PagerDuty on state transitions (e.g. during failovers or manual overrides).

### Decision
Reject embedding webhook alert dispatch logic inside the ATC daemon core. Instead, ATC will emit detailed, structured JSON logs (including audit logs) and OpenTelemetry metrics/traces, and operators should configure external observability alerting engines (such as Prometheus Alertmanager, Grafana Alerting, or log aggregation alerting rules) to handle notification routing.

### Consequences
- **Benefits**:
  - Keeps the ATC daemon core lightweight, focusing strictly on control-plane reconciliation.
  - Avoids introducing webhook retries, backoff loops, credential storage, or notification delivery guarantee logic inside ATC.
  - Leverages existing, robust enterprise alerting systems that platform teams already maintain.
- **Consequences**:
  - Operators must configure external alerts based on ATC logs or metrics to receive notifications.

---

## ADR 0026: Local Developer Observability Stack

### Status
Accepted

### Date
2026-06-19

### Context
Developing and validating the OpenTelemetry instrumentation locally required a standardized, easy-to-use logging, metrics, and tracing backend. Setting up these tools individually (Prometheus, Loki, Tempo, Grafana) was tedious and prone to configuration drift between developers.

### Decision
Provide a centralized `docker-compose.yml` environment containing a local dev Consul agent and a complete LGTM (Loki, Grafana, Tempo, Mimir/Prometheus) observability stack:
- Include an OpenTelemetry Collector acting as the central gateway for all OTel signals.
- Auto-provision Grafana datasources to load the Loki, Prometheus, and Tempo endpoints automatically on startup.
- Consolidate all container configuration files into a dedicated `deploy/observability/` folder.

### Consequences
- **Benefits**:
  - Zero-configuration local verification of traces, logs, and metrics.
  - Isolated developer environment mimicking production observability pipelines.
- **Consequences**:
  - Developers need Docker/Docker Compose installed locally, consuming minor system resources.

---

## ADR 0027: Go-Native BDD E2E Integration and Acceptance Testing

### Status
Accepted

### Date
2026-07-07

### Context
ATC requires automated acceptance testing to validate multi-datacenter watcher, failover, redirect, HA, and MCP workflows. Testing these flows involves multi-step processes, event-driven state transitions, and time-dependent waits (e.g. for HA session lock TTLs and Hysteresis cooldowns). We need to choose between standard procedural testing in Go (using tags and testcontainers-go) vs layering or replacing it with a BDD framework (e.g. GoConvey, Ginkgo/Gomega, or Godog/Cucumber).

### Decision
Adopt GoConvey as the BDD DSL library combined with HashiCorp's Consul `testutil` package to run fast, in-process dev servers:
- **GoConvey BDD DSL**: Write integration and E2E test scenarios using GoConvey's nested `Convey` and `So` block structures, which execute natively under standard `go test`.
- **In-Process Consul Test Server**: Utilize HashiCorp's `github.com/hashicorp/consul/sdk/testutil` library to spin up a local Consul server programmatically in-process, managed globally (e.g. in `TestMain`).
- **Reject `testcontainers-go`**: Avoid the container boot latency and cleanup overhead of Docker containers inside individual test flows, which would degrade performance under GoConvey's nested scope execution.

### Consequences
- **Benefits**:
  - Highly readable, hierarchical, and self-documenting BDD test reports directly in the terminal or GoConvey's Web UI.
  - Extremely fast E2E execution times because we avoid the overhead of spawning physical Docker containers for each test run.
  - 100% compatibility with standard Go toolchain (`go test`), native IDE debuggers, and profiling tools.
- **Consequences**:
  - Introduces external dependencies for GoConvey's DSL and the Consul SDK `testutil` library.
  - Requires careful management of shared Consul server state across nested `Convey` blocks to avoid cross-test pollution.
  - Developers must have the `consul` binary available on their system path for the `testutil` server to initialize correctly.

---

## ADR 0028: Dedicated Port for MCP Server

### Status
Accepted

### Date
2026-06-24

### Context
The native Model Context Protocol (MCP) server currently shares the main application port `:8088`. Because MCP relies on persistent Server-Sent Events (SSE) connections, this can lead to connection socket exhaustion for browser clients loading the Web UI dashboard. Additionally, exposing administrative write-authorized MCP tools on the same port as public-facing dashboard traffic limits the ability to enforce strict network-level firewall access controls (ACLs) to isolate machine-to-machine AI traffic. We need to decide whether to move the MCP server to the unauthenticated metrics port `:8089` or to a dedicated MCP port (e.g. `:8092`).

### Decision
Move the MCP server to a dedicated listener port (default `:8092` / CLI flag `--mcp-port` / configuration `server.mcp_port`):
- Reject moving the MCP server to the metrics port (`:8089`) due to a critical authentication conflict (the metrics port is unauthenticated, while MCP tools require write authorization/authentication).
- Run a third isolated `http.Server` instance dedicated to MCP SSE connections when `--mcp-enabled=true` (default).
- Running the MCP server should be optional. Setting the dedicated port to `0` results in the MCP server not being started

### Consequences
- **Benefits**:
  - Complete network-level security isolation (operators can firewall the MCP port separately from Web UI traffic).
  - Prevents browser client socket starvation (dashboard UI responsiveness remains unaffected by active AI agent sessions).
  - Allows custom TLS/auth settings specifically for MCP traffic.
- **Consequences**:
  - Operators must configure and bind an additional port (e.g. `:8092`) in deployment templates (Helm, Nomad, Docker Compose).

---

## ADR 0029: CLI Leader Management and Detailed Leader Discovery

### Status
Accepted

### Date
2026-07-11

### Context
Operators required a way to query cluster-wide leadership states, identify which node is the current active leader for each module, and execute administrative force-unlock actions to resolve hung locks without needing direct access to the Consul CLI or agent commands.

### Decision
- **Add native CLI subcommands:** Implement `atc leader status` and `atc leader force-unlock --module=<module>` to display the leadership topology and release component locks.
- **Extend REST API:** Update `/api/leader` response to return a detailed `modules` block containing the lock key, active Consul session ID, and owner node name for each module, while retaining legacy top-level keys for backward compatibility.
- **Standardize Default Node Name:** Populate the node `Name` configuration fallback (`atc-node-<hostname>`) at instantiation time in `atc.New()` to ensure consistent status comparisons between the CLI and running daemons.

### Consequences
- **Benefits**:
  - Operators can inspect and resolve HA lock states cleanly using native CLI utilities without exposing Consul's internal structures.
  - Consistent node name resolution eliminates status reporting mismatches between the dashboard and CLI.
- **Consequences**:
  - The `/api/leader` payload size is slightly larger, but remains backward compatible.


