/*
Package main represents the entrypoint for the ATC (Active Traffic Control) command-line utility.

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
  - server: Spins up the internal HTTP handler (health, services, API endpoints).
  - all: Resolves to 'consul' (runs all services).

HTTP Endpoints:

ATC server hosts two separate HTTP port listeners:
  - Main Port (default :8088): Serves the React frontend dashboard at `/`, exposes /ready,
    /services, JSON API service list (/api/services), manual overrides (/api/overrides), leader status (/api/leader), WAN federation status (/api/federation), and the MCP server interface.
  - Metrics Port (default :8089): Exposes OpenTelemetry metrics in Prometheus format at `/metrics`.

API & MCP Integration:

ATC server hosts a Model Context Protocol (MCP) server over Streamable HTTP transport at the `/mcp` route
for seamless integration with AI models and agents. Exposed tools include check_readiness, check_leadership,
list_atc_enabled_services, list_wan_federation_status, purge_redirect_config, apply_failover_override, and trigger_manual_redirect.

Predefined Strategies:

ATC supports predefined failover and redirect strategies defined by admins in a YAML config file.
Teams can assign these strategies to their Consul services using tags (e.g., `atc.failover=strategy-name`
and `atc.redirect=strategy-name`). ATC's forwarder and redirector apply these strategies dynamically and persist
them in the service-resolver configuration entry metadata.

Oscillation Dampening (Hysteresis):

ATC protects Consul from excessive write operations by debouncing rapid health check changes.
It supports a global default dampening period (e.g., `5s`), a tag-based override (`atc.dampening=duration`
such as `atc.dampening=0s` for immediate mode), and an operator safety boundary (`min_dampening_period`)
to prevent users from bypassing stability safeguards.

Active-Passive High Availability:

ATC can run in active-passive HA mode coordinated via Consul KV session locks. A single active leader
handles catalog watches and write reconciliation, while standby instances keep their HTTP/metrics servers active
but suspend reconciler watches. Failover is automatic when the active session lock expires.

Architectural Design Rule:
- NOTE: For every HTTP API endpoint exposed by the ATC server, a corresponding MCP tool MUST be registered.

For more options, run:

	atc server --help
*/
package main
