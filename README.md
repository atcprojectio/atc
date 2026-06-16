# ATC (Air Traffic Control)

![goreleaser](https://github.com/attachmentgenie/atc/actions/workflows/publish.yml/badge.svg)

ATC is a lightweight, high-performance Go service that automates the creation and management of Consul service-resolver configurations. Like air traffic control, ATC monitors the state of your Consul endpoints and automatically publishes failover or redirect rules when failing services are detected.

Originally based on a heavy dependency kit, ATC has been modernised to run natively on Go standard library primitives, making it extremely lightweight and secure with a minimal dependency tree.

---

## Features

- **Consul Automation**: Automatically watches service health checks and resolves configurations to create failover (`failover.hcl`) and redirection (`redirect.hcl`) service resolvers.
- **Consul Catalog Failover**: Automatically monitors Consul catalog changes and registers Prepared Queries (`atc-<service-name>`) with geo-failover to the nearest 2 datacenters for tagged services.
- **Embedded React Dashboard**: Stunning glassmorphic web dashboard served at `/` that displays tracked services, their prepared queries, active tags, and failover status.
- **Native Concurrency**: Powered by Go standard library `context`, `sync`, and `golang.org/x/sync/errgroup` for safe, coordinated background execution.
- **log/slog Structured Logging**: Built-in structured, level-filtered logging using standard Go `"log/slog"`.
- **Dual HTTP Ports**: Isolates application API endpoints from Prometheus metrics for production security:
  - **Main HTTP Port** (default `:8088`): Serves the React dashboard at `/`, exposes `/ready`, `/services`, `/api/services`, and the MCP interface.
  - **Metrics HTTP Port** (default `:8089`): Serves standard Prometheus format scrapes at `/metrics`.
- **Model Context Protocol (MCP) Server**: Hosts a native MCP server over Streamable HTTP transport at `/mcp` to expose system state directly to LLM agents and AI clients.

> [!IMPORTANT]
> **API-to-MCP Tool Mapping Rule**
> For every HTTP API endpoint exposed by the ATC server, an equivalent MCP tool must be registered on the MCP server to ensure parity between standard monitoring APIs and agent capabilities.

---

## Install

### macOS

```bash
brew tap attachmentgenie/tap
brew install attachmentgenie/tap/atc
```

### From Source

```bash
# Clone the repository
git clone https://github.com/attachmentgenie/atc.git
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
- `--metrics_port` (int): Port to expose Prometheus metrics on (default: `8089`).
- `--log_level` (string): Only log messages with this severity or above (`debug`, `info`, `warn`, `error`) (default: `info`).
- `--target` (strings): Comma-separated list of components to run (`consul`, `forwarder`, `redirector`, `server`, `all`) (default: `all`).
- `--consul_addr` (string): Consul HTTP endpoint address.
- `--consul_token` (string): Consul ACL token.
- `--consul_dc` (string): Consul target datacenter.

---

## Core HTTP Endpoints

### Application API (Port 8088)

- `/` (GET): Serves the embedded React frontend dashboard.
- `/ready` (GET): Simple readiness check (`200 OK`).
- `/services` (GET): Prints a formatted ASCII table showing the status of all active components.
- `/api/services` (GET): JSON list of active Consul services tagged with `atc.enabled=true`.
- `/mcp` (GET/POST): Streamable HTTP endpoint for Model Context Protocol interactions.

### Metrics API (Port 8089)

- `/metrics` (GET): Standard Prometheus metric scrapes.

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
