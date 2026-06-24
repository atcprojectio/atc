# MoSCoW Analysis: Active Traffic Control (ATC) Roadmap

This document reviews the revised product roadmap for Active Traffic Control (ATC) after the successful completion of Hysteresis, Active-Passive HA, Hot-Reloading, WAN Federation Indicators, Expanded MCP Actions, and Target-Scoped HA Leader Locking.

---

## Must Have

All roadmap Must Have items have been successfully implemented!

---

## Should Have

### 1. Go-Native Integration & Acceptance Testing Suite
Implement the accepted Go-native E2E integration test suite as defined in [ADR 0027](file:///Users/attachmentgenie/DevShed/Projects/atcprojectio/atc/ADR.MD#L640):
- Use `testcontainers-go` to isolate Consul and ATC daemon execution inside Go test runs.
- Set up a test suite folder `pkg/atc/integration` tagged with `//go:build integration`.
- Wire the integration test target in the Makefile (`make test-integration`) and add it to the `.github/workflows/pr.yml` CI pipeline.
- Implement Go-native MCP server SSE client assertions.

### 2. Dedicated Port for MCP Server
Implement the accepted isolated MCP listener configuration as defined in [ADR 0028](file:///Users/attachmentgenie/DevShed/Projects/atcprojectio/atc/ADR.MD#L669):
- Add a new configuration parameter `server.mcp_port` (default `8092`) and a corresponding CLI flag `--mcp-port`.
- Start a third parallel HTTP listener exclusively for MCP Server-Sent Events (SSE) traffic when `server.mcp_enabled: true` (default).
- Wire the new port mapping into Kubernetes Helm charts, Nomad jobs, and local Docker Compose files.

---

## Could Have

All roadmap Could Have items have been successfully implemented!

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

### 5. Interactive Web UI Strategy Editor
Admins originally requested an interactive web-based editor to visually draft and apply failover/redirect strategies. However, the declarative file-based YAML configurations (`strategies.yaml`) coupled with dynamic watcher hot-reloading works exceptionally well, provides GitOps friendliness, and keeps the UI client lightweight and read-only.

### 6. Webhook Notification System (Slack, MS Teams, PagerDuty)
Dispatching notifications and alerts on routing state transitions or catalog updates was considered. However, this is properly the responsibility of centralized observability and alerting tools (e.g., Prometheus Alertmanager, Grafana Alerting, Consul Event alerts) monitoring the emitted metrics and structured logs, rather than embedding notification dispatch logic directly inside the ATC daemon core.
