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
Accepted

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
Accepted

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
