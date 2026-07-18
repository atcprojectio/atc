# ATC (Active Traffic Control)

![goreleaser](https://github.com/atcprojectio/atc/actions/workflows/publish.yml/badge.svg)

ATC is a lightweight, high-performance Go service that automates the creation and management of Consul service-resolver configurations. Like Active Traffic Control, ATC monitors the state of your Consul endpoints and automatically publishes failover or redirect rules when failing services are detected.

---

## Features

- **Consul Automation**: Watches service health checks and automatically publishes failover or redirection service resolvers.
- **Oscillation Dampening (Hysteresis)**: Protects Consul from configuration churn by debouncing catalog flapping.
- **Active-Passive HA**: Coordinated via target-scoped Consul KV Session Locks for independent workload failover without split-workload deadlocks.
- **Embedded Dashboard**: Stunning glassmorphic React web dashboard served at `/` displaying service status and leadership.
- **OpenTelemetry & Observability**: Native support for trace spans, structured logs (via standard `"log/slog"`), and Prometheus scraping.
- **Model Context Protocol (MCP)**: Native SSE listener at `:8092/mcp` exposing core operations directly to LLM agents.

---

## Installation

### macOS (via Homebrew)
```bash
brew tap atcprojectio/tap
brew install atcprojectio/tap/atc
```

### From Source
```bash
git clone https://github.com/atcprojectio/atc.git
cd atc
make build
```

---

## Quickstart

Start the ATC background watcher daemon:
```bash
./dist/atc server [flags]
```

### Essential Flags
- `--config` (string): Path to YAML configuration file (e.g. `deploy/strategies.yaml`).
- `--consul_addr` (string): Consul HTTP endpoint (e.g. `localhost:8500`).
- `--target` (strings): Modules to run (`consul`, `forwarder`, `redirector`, `server`, `all`) (default: `all`).
- `--dry-run` (bool): Run logic and log decisions without modifying Consul.

For the full CLI options list and environment variables, refer to the [Configuration Guide](file:///Users/attachmentgenie/DevShed/Projects/atcprojectio/atc/docs/index.html#configuration).

---

## Administrative Commands

### Configuration Validation
```bash
./dist/atc validate --config strategies.yaml
```

### Leader Management
- **Query cluster leadership status:**
  ```bash
  ./dist/atc leader status
  ```
- **Force release a component lock session:**
  ```bash
  ./dist/atc leader force-unlock --module=<forwarder|redirector>
  ```

---

## Model Context Protocol (MCP)

ATC exposes a Model Context Protocol (MCP) server over SSE on a dedicated port (`:8092`). For instructions on how to connect your AI agents or configure Claude Desktop, see the [Claude Desktop MCP Integration Guide](file:///Users/attachmentgenie/DevShed/Projects/atcprojectio/atc/docs/index.html#mcp-server) on our documentation site.

---

## Testing & Development

### Local Observability Sandbox
A complete developer environment containing Consul, ATC, and the LGTM (Loki, Grafana, Tempo, Prometheus) stack is maintained in the [atc-demo](https://github.com/atcprojectio/atc-demo) repository. To start the observability stack:
```bash
# Clone the demo repository and spin it up
git clone https://github.com/atcprojectio/atc-demo.git
cd atc-demo
make up-obs
```

### Running Tests
```bash
# Run unit tests
make test

# Run GoConvey BDD E2E integration tests (requires local consul binary)
make test-integration

# Run frontend Vitest specs
make test-frontend
```

---

## Documentation & Resources

For detailed technical specifications, architecture records, and integrations, see:
- **Reference Site & API Docs:** [docs/index.html](file:///Users/attachmentgenie/DevShed/Projects/atcprojectio/atc/docs/index.html)
- **Architecture Decision Records (ADRs):** [ADR.MD](file:///Users/attachmentgenie/DevShed/Projects/atcprojectio/atc/ADR.MD)
- **Project Roadmap & MoSCoW List:** [TODO.md](file:///Users/attachmentgenie/DevShed/Projects/atcprojectio/atc/TODO.md)
