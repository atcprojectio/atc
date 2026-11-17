APP-BIN := dist/$(shell basename $(shell pwd))
# Go packages to build/test. Listed explicitly so ./... does not pick up Go code
# vendored inside frontend/node_modules.
GO_PKGS := . ./cmd/... ./pkg/...
GOVULNCHECK_VERSION ?= v1.1.4

.PHONY: vuln build build-frontend consul-up consul-down consul-register-test consul-deregister-test darwin fresh lint lint-frontend lint-backend linux qa release run snapshot tag test test-integration test-frontend

build-frontend:
	cd frontend && npm ci && npm run build

build: build-frontend
	goreleaser build --id $(shell go env GOOS) --single-target --snapshot --clean -o ${APP-BIN}
darwin: build-frontend
	goreleaser build --id darwin --snapshot --clean
linux: build-frontend
	goreleaser build --id linux --snapshot --clean
snapshot: build-frontend
	goreleaser release --snapshot --clean
tag:
	git tag $(shell svu next --v0)
	git push --tags
release: tag build-frontend
	goreleaser --clean

watch:
	gotestsum --watch --format testname
lint-frontend:
	cd frontend && npm ci && npm run lint
lint-backend:
	golangci-lint run ./...
lint: lint-frontend lint-backend
test:
	gotestsum --format testname -- -race -coverprofile=coverage.out $(GO_PKGS)
	@go tool cover -func=coverage.out | tail -1
test-integration:
	go test -race -v -tags=integration ./pkg/atc/integration/...
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) $(GO_PKGS)
test-frontend:
	cd frontend && npm run test
qa: lint test test-integration test-frontend vuln
run: ## Run binary.
	OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 ./${APP-BIN} server --config deploy/strategies.yaml
fresh: build run
consul-up:
	@echo "Local Docker Compose environment has been moved to the atc-demo repository."
	@echo "Please run 'make up-infra' or 'make up' inside the atc-demo repository instead."
	@exit 1

obs-up:
	@echo "Local Docker Compose environment has been moved to the atc-demo repository."
	@echo "Please run 'make up-obs' inside the atc-demo repository instead."
	@exit 1

join-wan:
	docker exec consul-dc2 consul join -wan consul-dc1 || true

consul-register-test:
	curl -s --request PUT \
		--data '{"ID": "payment-service-dc1-1", "Name": "payment-service", "Tags": ["atc.enabled=true", "atc.failover=standard-failover", "atc.redirect=standard-redirect"], "Address": "payment-service-dc1", "Port": 8080}' \
		http://localhost:8500/v1/agent/service/register
	@echo "\nRegistered payment-service-dc1-1 in dc1 (pointing to port 8080 mock)"
consul-deregister-test:
	curl -s --request PUT \
		http://localhost:8500/v1/agent/service/deregister/payment-service-dc1-1
	@echo "\nDeregistered payment-service-dc1-1 from dc1"
consul-register-test-dc2:
	curl -s --request PUT \
		--data '{"ID": "payment-service-dc2-1", "Name": "payment-service", "Address": "payment-service-dc2", "Port": 8082}' \
		http://localhost:8501/v1/agent/service/register
	@echo "\nRegistered payment-service-dc2-1 in dc2 (pointing to port 8082 mock)"

consul-down:
	@echo "Local Docker Compose environment has been moved to the atc-demo repository."
	@echo "Please run 'make down-infra' inside the atc-demo repository instead."
	@exit 1

obs-down:
	@echo "Local Docker Compose environment has been moved to the atc-demo repository."
	@echo "Please run 'make down-obs' inside the atc-demo repository instead."
	@exit 1
