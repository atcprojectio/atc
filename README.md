# ATC (Active Traffic Control)

![goreleaser](https://github.com/atcprojectio/atc/actions/workflows/publish.yml/badge.svg)

ATC is a lightweight, high-performance Go service that automates the creation and management of Consul service-resolver configurations. Like Active Traffic Control, ATC monitors the state of your Consul endpoints and automatically publishes failover or redirect rules when failing services are detected.

Originally based on a heavy dependency kit, ATC has been modernised to run natively on Go standard library primitives, making it extremely lightweight and secure with a minimal dependency tree.

---

## Features

- **Consul Automation**: Automatically watches service health checks and resolves configurations to create failover (`failover.hcl`) and redirection (`redirect.hcl`) service resolvers.
- **Consul Catalog Failover**: Automatically monitors Consul catalog changes and registers Prepared Queries (`atc-<service-name>`) with geo-failover to the nearest 2 datacenters for tagged services.
- **Embedded React Dashboard**: Stunning glassmorphic web dashboard served at `/` that displays tracked services, their prepared queries, active tags, and failover status.
- **Native Concurrency**: Powered by Go standard library `context`, `sync`, and `golang.org/x/sync/errgroup` for safe, coordinated background execution.
- **OpenTelemetry Integration**: Full support for OpenTelemetry Traces, Metrics, and Logs. Traces and logs are automatically correlated and exported via OTLP, and metrics are exposed via OTLP and a Prometheus bridge.
- **log/slog Structured Logging**: Multiplexed structured, level-filtered logging using standard Go `"log/slog"` that outputs to console/Stderr and propagates context to OpenTelemetry.
- **Dual HTTP Ports**: Isolates application API endpoints from metrics for production security:
  - **Main HTTP Port** (default `:8088`): Serves the React dashboard at `/`, exposes `/ready`, `/services`, `/api/services`, and the MCP interface.
  - **Metrics HTTP Port** (default `:8089`): Serves OpenTelemetry metrics in Prometheus format at `/metrics`.
- **Model Context Protocol (MCP) Server**: Hosts a native MCP server over Streamable HTTP transport at `/mcp` to expose system state directly to LLM agents and AI clients.

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

### Environment Variables

OpenTelemetry exporters can be configured using standard OpenTelemetry environment variables:
- `OTEL_EXPORTER_OTLP_ENDPOINT`: Target OTel collector endpoint (default: `http://localhost:4318` via HTTP).
- `OTEL_SERVICE_NAME`: The name of the service (default: `atc`).
- `OTEL_SDK_DISABLED`: Set to `true` to completely disable telemetry collection.

---

## Predefined Routing & Failover Strategies

ATC supports predefined routing strategies that admins can configure in a YAML file loaded via the `--config` flag. Predefined strategies are defined under `strategies.failover` and `strategies.redirect`.

### Configuration Example (`strategies.yaml`)

```yaml
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

### Invoking Strategies via Consul Tags

Teams can apply these predefined strategies to their Consul services using the following tags in the service definition:
- `atc.failover=<strategy-name>`: Specifies the predefined failover strategy to use. If omitted or not found, it defaults to failing over to the dynamically resolved target datacenter for the same service.
- `atc.redirect=<strategy-name>`: Specifies the predefined redirection strategy to use when the service goes offline (is deleted from the catalog). If omitted or not found, it defaults to redirecting to the dynamically resolved target datacenter for the same service.

When a service is active, the strategy names are persisted in the Consul `service-resolver` entry metadata, which allows the redirector to apply the correct redirect strategy even after the service is removed from the Consul catalog.

---

## Core HTTP Endpoints

### Application API (Port 8088)

- `/` (GET): Serves the embedded React frontend dashboard.
- `/ready` (GET): Simple readiness check (`200 OK`).
- `/services` (GET): Prints a formatted ASCII table showing the status of all active components.
- `/api/services` (GET): JSON list of active Consul services tagged with `atc.enabled=true`.
- `/mcp` (GET/POST): Streamable HTTP endpoint for Model Context Protocol interactions.

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
   - `list_services`
   - `list_atc_enabled_services`
