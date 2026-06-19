# ADR 0001: Modernise Core Architecture and Consolidate Consul Automation

## Status
Accepted

## Date
2026-06-19

## Context
The legacy **ATC** (Air Traffic Control) service was built on top of Grafana's `dskit` framework, which introduced a substantial dependency footprint (over 100 indirect dependencies) and forced a rigid, heavyweight modular architecture. The application also relied on `go-kit/log` for structured logging and exposed several complex submodules (`autoscaler`, `deployer`, `event_sink`, `incident`, and `radar`) that were unused and cluttered the codebase.

The primary requirement was to clean up, lightweight, and modernise the system, refocusing its scope entirely on automating Consul service-resolver routing configuration (failover and redirection) during active catalog changes, while exposing both a human-friendly Web UI dashboard and an AI-friendly Model Context Protocol (MCP) endpoint.

## Decisions

### 1. Remove `dskit` and Standardise Concurrency
We replaced all Grafana `dskit` components with Go standard library primitives:
* **HTTP Server**: Replaced `dskit/server` with standard `http.Server` running two parallel, isolated port listeners (Main application port `:8088` and Prometheus metrics scrape port `:8089`).
* **Lifecycle & Signal Management**: Replaced `dskit/services` manager with standard standard signal contexts (`os/signal` listening for `syscall.SIGTERM` / `os.Interrupt`) and native `golang.org/x/sync/errgroup` to handle background service lifecycles concurrently.

### 2. Migrate Logging to `log/slog`
We migrated from `go-kit/log` and `dskit`'s custom logging utilities to the Go standard library `log/slog` structured logging, injecting sub-system metadata (e.g. `module=forwarder`, `module=redirector`) via contextual attributes.

### 3. Prune Unused Submodules
We removed all code, configurations, and handlers related to submodules that did not participate in Consul gateway routing:
* Deleted the `autoscaler`, `deployer`, `event_sink`, `incident`, and `radar` packages.
* Kept and polished only `server`, `forwarder`, and `redirector`.

### 4. Consolidate Consul Watcher Logic
We centralized the Consul watch routines into a single unified helper in `pkg/atc/watcher/watcher.go`. It polls both service catalog changes and health check states simultaneously and broadcasts update events to submodules via a thread-safe `Broadcaster` channel subscription mechanism, eliminating redundant network polling.

### 5. Coordinated Consul Config Reconciliation
We established a strict split-brain prevention protocol between the `forwarder` and `redirector` tasks because they read/write to the same Consul `service-resolver` config entries:
* **Active State (`forwarder`)**: The `forwarder` creates and maintains the `Failover` config block (with modern `Targets` lists and omitted deprecated `Datacenters` fields) for active catalog services.
* **Offline State (`redirector`)**: The `redirector` creates a `Redirect` config block pointing to a remote datacenter when services are completely absent from the local catalog.
* **Coordination**: We introduced a `forwarderEnabled` flag in `redirector`'s constructor. When set to `true`, the `redirector` skips writing base/empty config entries for active services. This prevents concurrent overwrite race conditions where the `redirector` would wipe out the `Failover` block written by the `forwarder`.

### 6. Embedded React UI Dashboard
We added a React frontend dashboard (scaffolded via Vite) that compiles static production assets directly into the Go server directory. The Go server uses `go:embed` to serve the dashboard at the `/` root directly from the compiled binary. The dashboard visualizes active/redirecting flows, service tags, and includes a confirmation-based **Purge** trigger to clean up stale configurations.

### 7. Native Model Context Protocol (MCP) Server
We exposed a streamable Model Context Protocol server over HTTP at `/mcp` using the official MCP Go SDK. We registered tools to check readiness, list ATC-tracked services, and purge redirect configs. We wrapped the handler in custom CORS middleware to guarantee compatibility with Desktop/Electron-based MCP clients (such as Claude Desktop).

### 8. Containerised Local Dev Environment
We refactored all native `consul` commands inside the `Makefile` to use Docker containers:
* `make consul-up`: Runs a local dev Consul container in the background (`-d --rm`).
* `make consul-down`: Gracefully stops the Consul container.
* `make consul-register-test` / `consul-deregister-test`: Use `curl` to dynamically mock catalog services against the container's REST API.

## Consequences

* **Reduced Complexity**: The codebase was simplified and reduced to ~10% of its original dependency tree size, boosting build speeds and binary portability.
* **Stateless Self-Healing**: The Consul reconciliation loop operates entirely statelessly against Consul's current state, allowing the service to self-heal and recover configuration states instantly upon startup or node recovery.
* **Enhanced Diagnostics**: Humans can audit cluster failover states in real time using the built-in dark-mode dashboard, while AI developers can inspect and mutate state directly through the standardized `/mcp` interface.

---

# ADR 0002: Active Traffic Control (ATC) Routing, HA, and Visualization Design

## Status
Accepted

## Date
2026-06-19

## Context
Following the core architecture modernization, we needed to make key architectural decisions to prepare the ATC system for production-grade High Availability (HA) deployments, define clear boundaries for traffic routing control loops, design UI visualization scope, and establish a robust validation environment.

## Decisions

### 1. Active-Passive High Availability (HA) via Consul Locks
We will implement leader election using **Consul KV Session locks** (active-passive mode) rather than embedding a separate Raft consensus ring (such as HashiCorp Raft) in the ATC daemons:
* **Stateless Deployment**: Using Consul locks allows ATC to remain completely stateless. Adding embedded Raft would require persistent disk storage (PVCs) for Raft logs and cluster peer configuration, turning a simple stateless container deployment into a complex stateful set.
* **No Redundant Consensus**: Since ATC already depends on Consul being healthy to perform any traffic routing coordination, setting up a separate consensus ring (Raft on top of Raft) adds useless operational complexity and split-brain vectors without any additional resiliency.

### 2. Static Failover Routing (Deferring Active Health Routing to Consul Data Plane)
ATC will **not** dynamically rewrite or invent resolver target lists based on its own WAN health views. Active datacenter failover targets remain statically defined in the strategies configuration:
* **Redundant Complexity**: Consul Connect (the Envoy data plane) already has native, sub-second active routing endpoints and load-balancing failover. Re-implementing health checks in ATC's control plane duplicates Consul's work at a much slower timescale (seconds vs milliseconds).
* **Split-Brain Risk**: A target datacenter might be reachable from ATC but unreachable from the Envoy proxies (or vice-versa). ATC making routing alterations based on its own WAN health views can cause erroneous failovers and mismatching states.
* **Operator Confusion**: Dynamically editing static global failover lists on the fly causes the actual active Consul resolver entry to deviate from the source-of-truth configuration files and service tags, making troubleshooting extremely difficult.
* **Correct Boundary**: Consul Connect handles data-plane failovers natively. ATC will restrict its role to registering/redirecting service-resolvers during catalog enrollment and catalog deletions.

### 3. WAN Federation Indicators instead of Detailed Health Visualization
The ATC React dashboard will visualize the configured routing paths and display a lightweight **Target Datacenter WAN Federation Status Indicator** (obtained via a cheap, cached query to Consul's WAN members list). It will **not** replicate detailed service instance health, node IPs, or system topologies:
* **Duplication**: Consul UI already provides an excellent, real-time interface to inspect nodes, health checks, and instances for every datacenter.
* **Observability Stack Integration**: For service telemetry, dependency maps, and overall traffic throughput health, operators look at APM tools like Grafana, utilizing Consul metrics and Prometheus data. Attempting to replicate Grafana/Consul UI inside ATC's embedded web server adds bloat and API overhead.
* **ATC Focus**: ATC's UI should remain focused purely on visualizing and managing **configured routing rules and traffic control paths** (failovers, redirects, and overrides).

### 4. Local Client-Side Routing Simulation for Validation
For local verification of the failover/redirect lifecycle, the demo environment uses lightweight `hashicorp/http-echo` containers as mock targets and a smart **Python Traffic Client** to query Consul and simulate routing decisions:
* **Why**: Running full Envoy sidecars, Consul Connect controllers, and certificate authorities in Docker Compose adds significant resource overhead and startup latency. The client-side simulation validates Consul's resolver configurations and routing logic cleanly and fast.

## Consequences
* **Lightweight & Stateless**: ATC can be deployed in Kubernetes as a standard stateless `Deployment` with simple replica counts.
* **Operational Simplicity**: No PVCs, stateful disks, or complex clustering configs are introduced.
* **Reduced Blast Radius**: Keeping active health-routing in the data plane (Consul Connect) keeps the network path resilient even if the ATC control plane is completely down.
* **Clean Tool Boundaries**: ATC acts strictly as a policy translator and offline cleanup manager, while Grafana/Consul UI remain the telemetry and catalog monitoring systems.

---

# ADR 0003: OpenTelemetry Integration for Observability

## Status
Accepted

## Date
2026-06-19

## Context
Standardized application telemetry (metrics, traces, logs) is required to diagnose traffic control actions, catalog sync cycles, and connection latency in production.

## Decisions
- **SDK**: Adopt the standard OpenTelemetry Go SDK to emit trace spans and runtime metrics.
- **Exporters**: Export signals using the OTLP format to an OpenTelemetry Collector.
- **Instrumentation**: Auto-instrument trace scopes for HTTP handlers, and manually instrument reconciliation loops.
- **Correlations**: Inject trace and span context into structured `slog` messages.

## Consequences
- **Benefits**:
  - Enhanced operational transparency and standard telemetry reporting.
  - Easier root-cause analysis for routing adjustments.
- **Consequences**:
  - Small runtime tracing latency and trace volumes.

---

# ADR 0004: Predefined Redirection and Failover Strategies

## Status
Accepted

## Date
2026-06-19

## Context
Service owners need template-based patterns for failover and redirect configurations instead of writing complex targets on individual services.

## Decisions
- **Strategies**: Define admin-controlled named failover and redirect strategies in a local YAML configuration (`strategies.yaml`).
- **Load Configuration**: Expose flags to load configuration at startup.
- **Consul Tags**: Enable service owners to apply these named strategies via standard Consul service catalog tags: `atc.failover=<strategy>` and `atc.redirect=<strategy>`.

## Consequences
- **Benefits**:
  - Consistent routing policies across teams.
  - Simplified tag-based onboarding.
- **Consequences**:
  - Changes to templates require config reloading.

---

# ADR 0005: Strict Consul Service Resolver Metadata Configuration and In-Memory Cache

## Status
Accepted

## Date
2026-06-19

## Context
Storing both redirect and failover strategies in the Consul `service-resolver` metadata is misleading because only one strategy is active at any time. However, when a service goes offline, its catalog tags disappear, making the redirector unable to resolve the correct redirect strategy.

## Decisions
- **Metadata Config**: Enforce strict metadata configuration: write only the active strategy to the Consul `service-resolver` metadata (`failover-strategy` for active failovers, `redirect-strategy` for deleted service redirects).
- **In-Memory Cache**: Introduce an in-memory thread-safe `strategiesCache` in reconcilers to store configuration strategies parsed from active catalog scans.
- **Fallback Resolution**: Check the in-memory cache first to resolve redirect strategies for deleted services, falling back to existing metadata.
- **API & UI Sync**: Update the dashboard `/api/services` and React UI to merge these cached strategies, showing both strategy names for active and deleted services.

## Consequences
- **Benefits**:
  - Accurate metadata state in Consul.
  - API/UI dashboard continues to show complete configurations.
- **Consequences**:
  - Simultaneous restart of daemon and catalog deletion defaults to default redirects.

---

# ADR 0006: Custom Zero-Dependency Documentation Website and GitHub Pages Publishing

## Status
Accepted

## Date
2026-06-19

## Context
Need robust project documentation that can double as a user-facing project website, with automated deployment and low dependency overhead.

## Decisions
- **Site Structure**: Build a modern dark-mode single-page portal inside a `docs/` folder using raw HTML, CSS, and JS (leveraging Google Fonts, FontAwesome, and Prism.js).
- **Publishing Pipeline**: Deploy it using GitHub Actions (`.github/workflows/pages.yml`) to GitHub Pages on every push to `main` branch under the `docs/` path.
- **Homebrew Tap**: Distribute macOS/Linux releases via Homebrew in an official tap (`atcprojectio/tap`).

## Consequences
- **Benefits**:
  - Fast website performance.
  - Zero build dependencies for documentation.
  - Automatic deployment on push.
- **Consequences**:
  - Content written in HTML instead of markdown, requiring structured tags.

---

# ADR 0007: Service-Specific Hysteresis Overrides via Consul Tags

## Status
Accepted

## Date
2026-06-19

## Context
While a global `dampening_period` protects the Consul cluster from configuration churn during health check oscillations (flapping), different services have differing SLAs. High-priority services require instant failover (low or zero dampening), whereas low-priority background workers can tolerate longer reconciliation delays. We need a way for service owners to configure their own dampening periods while giving platform operators control over safety boundaries.

## Decisions
- **Service-Specific Tags**: Allow services to override the global hysteresis period using the tag `atc.dampening=<duration>` (e.g. `atc.dampening=10s` or `atc.dampening=0s`).
- **Zero-Dampening (Immediate Mode)**: Allow `0` or `0s` as valid tag values to indicate that no dampening should be applied, triggering immediate reconciliation.
- **Operator Safety Boundary**: Expose a global configuration parameter `min_dampening_period` (defaulting to `0s`). ATC will clamp any service's custom tag value to this minimum limit.
- **Ultimate Freedom**: By setting `min_dampening_period` to `0s` globally, operators grant services the absolute freedom to bypass hysteresis completely and run in instant failover mode.

## Consequences
- **Benefits**:
  - Application teams can configure SLA sensitivities independently.
  - Operators retain global governance limits to protect the Consul leader.
- **Consequences**:
  - Unstructured service tags must be parsed safely with fallback to the global configuration on errors.

---

# ADR 0008: Zero-Downtime Strategy Reloading & File Watcher

## Status
Accepted

## Date
2026-06-19

## Context
Predefined strategies (failover, redirect) and dampening configurations (hysteresis defaults and safety limits) are loaded from a configuration file at startup. Previously, changes to these configurations required a full restart of the ATC daemon. To support zero-downtime operations and rapid policy adjustments, we need the daemon to dynamically reload changes without causing traffic disruptions or dropping active reconciliation watches.

## Decisions
- **Active Configuration File Watcher**: Wire up an active filesystem watcher on the loaded configuration file using Viper's `WatchConfig()` and `OnConfigChange()` primitives.
- **Dynamic In-Memory Reloading**: When filesystem events trigger, unmarshal the configuration and call a new `ReloadConfig` method on the `Atc` coordinator to update the parameters in-memory.
- **Thread-Safe Propagation**:
  - Update `Atc.Cfg` under a write lock (`Atc.cfgMu`).
  - Pass the new named strategies and dampening parameters to `Forwarder` and `Redirector` modules.
  - Implement read/write locks (`sync.RWMutex`) inside the `Forwarder` and `Redirector` modules to snapshot configuration parameters under a read lock at the start of each reconciliation iteration, avoiding data races with background watch routines.

## Consequences
- **Benefits**:
  - Zero-downtime configuration updates for strategies, timeouts, and dampening settings.
  - Continuous watch reconciliation is preserved without tearing down Consul catalog observers.
  - Thread-safe updates prevent race conditions and memory corruption.
- **Consequences**:
  - A small synchronization lock overhead when starting each reconciliation loop.

---

# ADR 0009: Target Datacenter WAN Federation Indicators

## Status
Accepted

## Date
2026-06-19

## Context
When failover and redirect routing strategies target remote datacenters, the routing resolves successfully only if those datacenters are WAN-federated and reachable via gossip/routing layers. If a datacenter connection is failed or not federated, Envoy proxies will fail to route requests. To prevent operator misconfigurations, the dashboard UI should visually indicate the connection health of each target datacenter.

## Decisions
- **WAN Gossip Monitoring**: Fetch the list of WAN members by calling `client.Agent().Members(true)` against the local Consul agent. This is a very cheap local gossip check.
- **Federation REST API**: Expose a new endpoint `GET /api/federation` returning a JSON list of unique datacenters and their statuses (`alive`, `failed`, or `offline`).
- **MCP Parity**: Expose the equivalent `list_wan_federation_status` tool on the Model Context Protocol (MCP) server.
- **Glassmorphic UI Badges**: Update the React dashboard to render small visual indicators next to datacenters in the failover/redirect paths based on status:
  - Active/Alive: `● dc2` (green dot).
  - Failed/Unreachable: `▲ dc2 (failed)` (red warning).
  - Unfederated/Missing: `⚠️ dc2 (unfederated)` (orange warning).

## Consequences
- **Benefits**:
  - Proactive warning visibility for invalid failover targets.
  - Parity between API endpoints and LLM agent tool sets.
  - Zero performance impact on the Consul leader since coordinates are read from the local agent.
- **Consequences**:
  - Requires active client polling of the `/api/federation` endpoint during catalog refreshes.

---

# ADR 0010: Expanded MCP Write Actions & Reconciler Bypass

## Status
Accepted

## Date
2026-06-19

## Context
Operators and AI agents need the ability to manually override traffic routing in the Consul service mesh, bypassing the automated reconciler logic (which continuously aligns Consul configuration entries with service catalog status and tags). This is critical for emergency failover, manual redirect testing, or staging traffic migrations. The override mechanism must be stateless, self-documenting in Consul, and thread-safe.

## Decisions
- **Manual Overrides HTTP API**: Expose `POST /api/overrides` taking a JSON payload specifying the service name, override type (`failover` or `redirect`), and target datacenter.
- **MCP Parity Tools**: Register two new write tools on the MCP server: `apply_failover_override(service, target_dc)` and `trigger_manual_redirect(service, redirect_dc)`.
- **Stateless Persistence via Metadata**: Manual overrides write a `service-resolver` config entry to Consul with the metadata tag `"created-by": "atc-override"`.
- **Reconciler Bypass (Skip)**: Update both `Forwarder` and `Redirector` reconciliation loops. If an existing `service-resolver` entry has `Meta["created-by"] == "atc-override"`, the reconciler immediately skips any automated updates or deletion for that service.
- **Bypass Clearing**: To restore automated watch reconciliation, operators can call `DELETE /api/services?name=<service>` which purges the entry from Consul.

## Consequences
- **Benefits**:
  - Operators and AI agents can execute manual overrides thread-safely without configuration race conditions.
  - No local state database is required, maintaining ATC's stateless design.
  - Full parity between standard REST endpoints and MCP agent tools.
- **Consequences**:
  - Restoring automated mode requires explicitly purging the override entry.

---

# ADR 0011: Helm Chart and Nomad Job Deployment Targets

## Status
Accepted

## Date
2026-06-19

## Context
Deploying ATC to production environments requires supporting both Kubernetes (the dominant container orchestration platform) and HashiCorp Nomad (which is frequently used in environments that heavily leverage HashiCorp Consul). The deployment configurations must support active-passive high availability (HA) session locks, log level configuration, metrics endpoints exposing, and mounting predefined strategy configuration files.

## Decisions
- **Kubernetes (Helm)**: Package ATC as a standard Helm v3 chart located under `deploy/helm/atc`. It manages replicas, configuration maps containing strategies, ClusterIP services for application and metrics endpoints, and standard RBAC components.
- **Nomad Job Specification**: Provide an HCL2 job file under `deploy/nomad/atc.nomad.hcl` configuring a Docker task with two task replicas, exposing http/metrics ports, and mounting configurations via Nomad templates.
- **Continuous Integration Linting**: Add a Helm lint step (`helm lint`) to the Pull Request workflow to ensure syntax correctness of all templates.
- **Automated Dependency Auditing**: Add a `helm` ecosystem tracking block to `.github/dependabot.yml` to automatically verify chart dependency updates.

## Consequences
- **Benefits**:
  - Out-of-the-box support for deploying ATC to both major orchestrators (Nomad and Kubernetes).
  - Standardized configuration parameters (Consul API URLs, tokens, metrics scraping) exposed as variables.
  - Automated CI validation of Helm charts.
- **Consequences**:
  - Standard variables must align with environment variables mapped to Viper settings in ATC.
