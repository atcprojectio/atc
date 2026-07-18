# ATC Project Agent Guidelines

This file defines code style, architecture constraints, and operational patterns for Active Traffic Control (ATC). Follow these rules when modifying or adding code.

## Concurrency
- **Standard Concurrency**: Use Go standard library concurrency primitives and `golang.org/x/sync/errgroup` for concurrent background reconciliation loops. Do not introduce Grafana `dskit` components.

## Observability, Logging, & Alerting
- **Structured Logging**: Use Go's standard library `log/slog`. Subsystem logs must inject a metadata module attribute (e.g., `slog.String("module", "forwarder")`). Do not introduce third-party wrappers (e.g. zap, logrus).
- **OpenTelemetry**: Emit telemetry using standard OpenTelemetry Go SDK trace spans and metrics. Wrap tracing logic around critical service-resolver writes, deletes, and configuration reloads.
- **External Alerting**: Do not implement direct webhook alerts (Slack, PagerDuty, etc.) in the daemon core. Emit structured JSON audit logs and OTel signals to be consumed by external alerting stacks.

## Control Plane Boundaries
- **No Data Plane Proxying**: ATC is strictly a control-plane orchestrator. Defer all request proxying and active data-plane load-balancing to Consul Connect (Envoy proxies). Do not write proxying or packet-forwarding logic.
- **External Secret Management**: Do not integrate native Vault SDK clients. Defer secret injection (e.g., Consul ACL tokens) to external orchestrators (Kubernetes sidecars, Nomad blocks, envconsul).

## Security & API Design
- **API & MCP Security**: Protect all new REST API and Model Context Protocol (MCP) endpoints using `authMiddleware` (exceptions: `/health` and `/ready`).
- **Audit Logging**: Any administrative override or purge actions must write high-priority JSON audit logs using `logAudit` (masking sensitive tokens).
- **Dedicated MCP Port**: The Model Context Protocol (SSE) server must run on its own isolated listener port (default `:8092`) separate from the main Web UI dashboard (`:8088`).

## Testing Standards
- **GoConvey BDD**: Write integration and E2E acceptance tests using GoConvey's nested `Convey` and `So` block structures.
- **In-Process Consul**: Use HashiCorp's `consul/sdk/testutil` in-process agent for testing Consul catalog transitions. Avoid spawning Docker containers (`testcontainers-go`) within standard test loops.
