# ATC (Active Traffic Control)

![goreleaser](https://github.com/atcprojectio/atc/actions/workflows/publish.yml/badge.svg)

ATC is a lightweight, high-performance Go service that automates the creation and management of Consul service-resolver configurations. Like Active Traffic Control, ATC monitors the state of your Consul endpoints and automatically publishes failover or redirect rules when failing services are detected.

Originally based on a heavy dependency kit, ATC has been modernised to run natively on Go standard library primitives, making it extremely lightweight and secure with a minimal dependency tree.

---

## Features

- **Consul Automation**: Automatically watches service health checks and resolves configurations to create failover (`failover.hcl`) and redirection (`redirect.hcl`) service resolvers.
- **Consul Catalog Failover**: Automatically monitors Consul catalog changes and registers Prepared Queries (`atc-<service-name>`) with geo-failover to the nearest 2 datacenters for tagged services.
- **Oscillation Dampening (Hysteresis)**: Protects Consul from configuration churn by debouncing catalog flapping. Supports global defaults, custom tag overrides, and operator safety boundaries.
- **Active-Passive High Availability**: Run multiple ATC instances in active-passive mode. Dynamic leadership is coordinated via Consul KV Session Locks, with standby nodes serving read-only dashboards and metrics.
- **Embedded React Dashboard**: Stunning glassmorphic web dashboard served at `/` that displays tracked services, their prepared queries, active tags, failover status, and active leader designation.
- **Native Concurrency**: Powered by Go standard library `context`, `sync`, and `golang.org/x/sync/errgroup` for safe, coordinated background execution.
- **OpenTelemetry Integration**: Full support for OpenTelemetry Traces, Metrics, and Logs. Traces and logs are automatically correlated and exported via OTLP, and metrics are exposed via OTLP and a Prometheus bridge.
- **log/slog Structured Logging**: Multiplexed structured, level-filtered logging using standard Go `"log/slog"` that outputs to console/Stderr and propagates context to OpenTelemetry.
- **Dual HTTP Ports**: Isolates application API endpoints from metrics for production security:
  - **Main HTTP Port** (default `:8088`): Serves the React dashboard at `/`, exposes `/ready`, `/services`, `/api/services`, `/api/overrides`, `/api/leader`, and the MCP interface.
  - **Metrics HTTP Port** (default `:8089`): Serves OpenTelemetry metrics in Prometheus format at `/metrics`.
- **Model Context Protocol (MCP) Server**: Hosts a native MCP server over Streamable HTTP transport at `/mcp` to expose system state and manual write actions directly to LLM agents and AI clients.

> [!IMPORTANT]
> **API-to-MCP Tool Mapping Rule**
> For every HTTP API endpoint exposed by the ATC server, an equivalent MCP tool must be registered on the MCP server to ensure parity between standard monitoring APIs and agent capabilities.

---

## Install

### macOS

```bash
brew tap atcprojectio/tap
brew install atcprojectio/tap/atc
```

### From Source

```bash
# Clone the repository
git clone https://github.com/atcprojectio/atc.git
cd atc

# Compile the binary
make build
```

### Deployment

Production-grade deployment configurations for Nomad and Kubernetes are available in the [deploy](file:///Users/attachmentgenie/DevShed/Projects/atcprojectio/atc/deploy) directory.

#### Kubernetes (Helm)

A Helm chart is located under [deploy/helm/atc](file:///Users/attachmentgenie/DevShed/Projects/atcprojectio/atc/deploy/helm/atc). You can install it with:

```bash
helm install atc ./deploy/helm/atc --values ./deploy/helm/atc/values.yaml
```

To configure strategy rules, edit the `config.strategiesYaml` block in `values.yaml`.

#### Nomad (HCL)

A Nomad job definition is available at [deploy/nomad/atc.nomad.hcl](file:///Users/attachmentgenie/DevShed/Projects/atcprojectio/atc/deploy/nomad/atc.nomad.hcl). It runs two instances in Active-Passive HA mode using Consul session locks.

Submit the job using:

```bash
nomad job run ./deploy/nomad/atc.nomad.hcl
```

---

## Demo Project

A complete, self-contained multi-datacenter demo environment is available in the [atc-demo](https://github.com/atcprojectio/atc-demo) repository. It automatically spins up:
- Two WAN-federated Consul servers (`dc1` and `dc2`).
- Two ATC service instances running in active-passive HA mode.
- Mock target services and an automated Python traffic client to showcase live routing, failover, and redirect behavior.

To run it, clone the demo repository and follow the instructions in the README:

```bash
# Clone the demo repository
git clone https://github.com/atcprojectio/atc-demo.git
cd atc-demo

# Pull images, start the stack and run the demo walkthrough
make pull
make up
make run-demo
```

---

## Usage

Start the ATC background watcher process:

```bash
./dist/atc server [flags]
```

### Key Flags

- `--port` (int): Port to expose main service endpoints on (default: `8088`).
- `--metrics_port` (int): Port to expose Prometheus-formatted metrics on (default: `8089`).
- `--log_level` (string): Only log messages with this severity or above (`debug`, `info`, `warn`, `error`) (default: `info`).
- `--target` (strings): Comma-separated list of components to run (`consul`, `forwarder`, `redirector`, `server`, `all`) (default: `all`).
- `--consul_addr` (string): Consul HTTP endpoint address.
- `--consul_token` (string): Consul ACL token.
- `--consul_dc` (string): Consul target datacenter.
- `--config` (string): Path to ATC configuration file.
- `--ui-enabled` (bool): Enable serving the embedded React Web UI dashboard (default: `true`).

### Environment Variables

OpenTelemetry exporters can be configured using standard OpenTelemetry environment variables:
- `OTEL_EXPORTER_OTLP_ENDPOINT`: Target OTel collector endpoint (default: `http://localhost:4318` via HTTP).
- `OTEL_SERVICE_NAME`: The name of the service (default: `atc`).
- `OTEL_SDK_DISABLED`: Set to `true` to completely disable telemetry collection.

---

## Predefined Routing & Failover Strategies

ATC supports predefined routing strategies that admins can configure in a YAML file loaded via the `--config` flag. Predefined strategies are defined under `strategies.failover` and `strategies.redirect`.

If a strategy named `default` is configured (e.g. `strategies.failover.default` or `strategies.redirect.default`), teams can register their services using only the `atc.enabled=true` tag. ATC will automatically fall back to the `default` strategy if the specific `atc.failover` or `atc.redirect` tag is omitted.

### Configuration Example (`strategies.yaml`)

```yaml
dampening_period: "5s"
min_dampening_period: "0s"

ha:
  enabled: true
  lock_key: "atc/leader/lock"
  session_ttl: "15s"

strategies:
  failover:
    standard-failover:
      connect_timeout: "10s"
      targets:
        - datacenter: "dc2"
    multi-region-failover:
      connect_timeout: "5s"
      targets:
        - datacenter: "dc2"
        - datacenter: "dc3"
          service: "fallback-service"
  redirect:
    standard-redirect:
      datacenter: "dc2"
    geo-redirect:
      service: "geo-fallback"
      datacenter: "dc3"
```

### Invoking Strategies and Hysteresis via Consul Tags

Teams can apply these predefined strategies and override dampening boundaries to their Consul services using the following tags in the service definition:
- `atc.failover=<strategy-name>`: Specifies the predefined failover strategy to use. If omitted or not found, it defaults to failing over to the dynamically resolved target datacenter for the same service.
- `atc.redirect=<strategy-name>`: Specifies the predefined redirection strategy to use when the service goes offline (is deleted from the catalog). If omitted or not found, it defaults to redirecting to the dynamically resolved target datacenter for the same service.
- `atc.dampening=<duration>`: Custom hysteresis override (e.g. `atc.dampening=10s` or `atc.dampening=0s` for immediate mode). Clamped to `min_dampening_period`.

When a service is active, the strategy and dampening tag values are persisted in the Consul `service-resolver` entry metadata, which allows the redirector to apply the correct policies even after the service is removed from the Consul catalog.

---

## Core HTTP Endpoints

### Application API (Port 8088)

- `/` (GET): Serves the embedded React frontend dashboard.
- `/ready` (GET): Simple readiness check (`200 OK`).
- `/services` (GET): Prints a formatted ASCII table showing the status of all active components.
- `/api/services` (GET): JSON list of active Consul services tagged with `atc.enabled=true`.
- `/api/leader` (GET): JSON representing leadership status (`{"leader":true/false}`).
- `/api/federation` (GET): JSON list of WAN-federated datacenters and connection statuses.
- `/api/overrides` (POST): Manually override automatic failover/redirect routes in Consul.
  - JSON payload:
    ```json
    {
      "service": "payment-service",
      "type": "failover|redirect",
      "target_dc": "dc2"
    }
    ```
- `/mcp` (GET/POST): Streamable HTTP endpoint for Model Context Protocol interactions. Offers tools:
  - `check_readiness`
  - `check_leadership`
  - `list_atc_enabled_services`
  - `list_wan_federation_status`
  - `purge_redirect_config`
  - `apply_failover_override`
  - `trigger_manual_redirect`

### Metrics API (Port 8089)

- `/metrics` (GET): OpenTelemetry metrics in Prometheus format.

---

## Testing & Verification

### Registering and Deregistering Test Services

You can register and deregister mock services in your local Consul agent using the following make targets:

```bash
# Register a test-service with tag atc.enabled=true
make consul-register-test

# Deregister the test-service
make consul-deregister-test
```

### Claude Desktop MCP Integration

To enable the ATC Model Context Protocol (MCP) server in **Claude Desktop**, you can bridge the Streamable HTTP SSE transport to Claude's stdio interface using the official `mcp-remote` bridge client.

1. Open the Claude Desktop configuration file:
   - **macOS**: `~/Library/Application Support/Claude/claude_desktop_config.json`
   - **Windows**: `%APPDATA%\Claude\claude_desktop_config.json`

2. Add the `atc` configuration block under `mcpServers` pointing to the remote HTTP SSE endpoint:

```json
{
  "mcpServers": {
    "atc": {
      "command": "npx",
      "args": [
        "-y",
        "mcp-remote",
        "http://localhost:8088/mcp"
      ]
    }
  }
}
```

3. Restart Claude Desktop. The agent will now have access to the following tools:
   - `check_readiness`
   - `list_atc_enabled_services`
   - `purge_redirect_config`
   - `check_leadership`
   - `list_wan_federation_status`
