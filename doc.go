/*
Package main represents the entrypoint for the ATC (Air Traffic Control) command-line utility.

ATC automates the creation and management of Consul service-resolver configurations to control
routing of ingress requests for failing services. By watching service checks and endpoints,
ATC automatically publishes failover and redirect rules to Consul.

Usage:

	atc server [flags]
	atc modules
	atc version

Targets:

	- consul: Runs both the Forwarder and Redirector watcher services.
	- forwarder: Watches Consul endpoints to automatically configure request forwarding.
	- redirector: Watches Consul endpoints to automatically configure geo-failover prepared queries.
	- server: Spins up the internal HTTP handler (health, metrics, services).
	- all: Resolves to 'consul' (runs all services).

HTTP Endpoints:

ATC server hosts two separate HTTP port listeners:
	- Main Port (default :8088): Serves the React frontend dashboard at `/`, exposes /ready,
	  /services, JSON API service list (/api/services), and the MCP server interface.
	- Metrics Port (default :8089): Exposes OpenTelemetry metrics in Prometheus format at `/metrics`.

API & MCP Integration:

ATC server hosts a Model Context Protocol (MCP) server over Streamable HTTP transport at the `/mcp` route
for seamless integration with AI models and agents.

Architectural Design Rule:
- NOTE: For every HTTP API endpoint exposed by the ATC server, a corresponding MCP tool MUST be registered.

For more options, run:

	atc server --help
*/
package main
