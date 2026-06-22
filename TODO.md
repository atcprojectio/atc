# MoSCoW Analysis: Active Traffic Control (ATC) Roadmap

This document reviews the revised product roadmap for Active Traffic Control (ATC) after the successful completion of Hysteresis, Active-Passive HA, Hot-Reloading, WAN Federation Indicators, Expanded MCP Actions, and Target-Scoped HA Leader Locking.

---

## Must Have

### 1. Native Authentication & Authorization (RBAC)
The REST API and Web UI allow manual purges and overrides. Without authentication, anyone with network access to port 8088 can disrupt routing.
- **Solution**: Secure endpoints using:
  - **Consul Token Delegation**: Let users log in using their Consul ACL token. ATC passes this token to Consul to verify permissions.
  - **Static API Key / Basic Auth**: Simple static tokens configured for small teams.
  - **OIDC Single Sign-On**: Support OpenID Connect (OIDC) for enterprise setups.

### 2. Audit Logging
Whenever a manual override or configuration purge is executed, ATC must write a structured, high-priority audit log.
- **Solution**:
  - Record the operator's identity/token, client source IP, and the exact changes made to Consul configuration.
  - Export these logs to stdout/stderr in JSON format, ensuring easy ingestion by external SIEM/logging platforms.

---

## Should Have

### 1. OpenTelemetry Trace Propagation for Routing Decisions
While ATC exports system metrics and logs, tracing how specific health-flapping events led to routing shifts is difficult.
- **Solution**: Emit custom OTel Span Events or Trace Contexts whenever a service-resolver is written, deleted, or hot-reloaded, allowing operators to correlate routing changes directly in Grafana/Tempo.

### 2. Dry-Run / Auditing Mode
When deploying ATC to large production clusters, platform teams need to audit what actions the daemon *would* take before giving it write access.
- **Solution**: Add a `--dry-run` flag. When enabled, ATC logs target service changes and outputs the generated configurations without writing them to Consul.

### 3. Manual Override Auto-Expiration (TTL)
When operators or AI agents apply manual overrides (e.g. `POST /api/overrides`), there is a risk they forget to purge them. This leaves the service permanently bypassed by automated watch loops.
- **Solution**: Support an optional Time-To-Live (TTL) or expiration field (e.g. `duration: "1h"`) for overrides:
  - Store the expiration timestamp in the config entry metadata (`atc-override-expires-at`).
  - Implement a background TTL sweeper in ATC that automatically purges expired overrides, restoring the service to automated watchers.

---

## Could Have

### 1. Glassmorphic Web UI Strategy Editor
Currently, strategies are read-only from the dashboard UI.
- **Solution**: Provide an interactive strategy editor in the React UI where administrators can visually draft and apply routing configurations.

### 2. Webhook Notification System (Slack, MS Teams, PagerDuty)
Rules and routes are subject to change, and platform operators need notifications when routing transitions occur.
- **Solution**: Dispatch Webhook payloads to Slack, MS Teams, or PagerDuty on any automated failover/redirect or override changes.

### 3. Consul Enterprise Namespace Support
Large organizations deploy Consul with Namespaces to isolate service catalogs and network configurations. Currently, ATC lacks general namespace-awareness for its watcher and client queries (defaulting strictly to the `default` namespace).
- **Solution**: Add command-line and configuration support for namespaces:
  - Expose a `--consul_namespace` flag (or `consul_namespace` YAML key).
  - Apply the namespace query parameters to all Consul API requests (e.g., `(&api.QueryOptions{Namespace: cfg.ConsulNamespace})`).
  - Support cross-namespace failover configuration targeting.

### 4. Failover Rate Limiting / Dampening
If Consul's catalog flaps rapidly due to network instability, ATC could throttle writing config changes to prevent Consul catalog database thrashing.
- **Solution**: Implement a rate limiter or dampening window that merges and groups multiple close-successive target events before triggering a Consul API write.

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
