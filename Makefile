APP-BIN := dist/$(shell basename $(shell pwd))

.PHONY: build build-frontend consul-up consul-down consul-register-test consul-deregister-test darwin fresh lint linux qa release run snapshot tag test watch test-frontend

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
test-frontend:
	cd frontend && npm run test
qa: lint test test-frontend
run: ## Run binary.
	OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 ./${APP-BIN} server --config deploy/strategies.yaml
fresh: build run
consul-up:
	docker compose up -d consul

obs-up:
	docker compose up -d
	@echo "Waiting for services to start..."
	@sleep 3
	@make join-wan

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
	docker compose stop consul || true

obs-down:
	docker compose down


