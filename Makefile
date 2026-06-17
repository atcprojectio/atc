APP-BIN := dist/$(shell basename $(shell pwd))

.PHONY: build build-frontend consul-up consul-down consul-register-test consul-deregister-test darwin fresh lint linux qa release run snapshot tag test watch

build-frontend:
	cd frontend && npm install && npm run build

build: build-frontend
	goreleaser build --id $(shell go env GOOS) --single-target --snapshot --clean -o ${APP-BIN}
darwin: build-frontend
	goreleaser build --id darwin --snapshot --clean
linux: build-frontend
	goreleaser build --id linux --snapshot --clean
snapshot: build-frontend
	goreleaser release --snapshot --clean
tag:
	git tag $(shell svu next)
	git push --tags
release: tag build-frontend
	goreleaser --clean

watch:
	gotestsum --watch --format testname
lint:
	pre-commit run --files $(shell git ls-files -m)
test:
	gotestsum --format testname
qa: lint test
run: ## Run binary.
	OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 ./${APP-BIN} server --config deploy/observability/strategies.yaml
fresh: build run
consul-up:
	docker compose up -d consul

obs-up:
	docker compose up -d

consul-register-test:
	curl --request PUT \
		--data '{"ID": "test-service", "Name": "test-service", "Tags": ["atc.enabled=true", "primary", "atc.failover=multi-region-failover", "atc.redirect=geo-redirect"], "Address": "127.0.0.1", "Port": 8080}' \
		http://localhost:8500/v1/agent/service/register
consul-deregister-test:
	curl --request PUT \
		http://localhost:8500/v1/agent/service/deregister/test-service

consul-down:
	docker compose stop consul || true

obs-down:
	docker compose down


