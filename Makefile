# Mirrors .github/workflows/go-ci.yml so `make check` runs the
# same checks CI does, from one command, before a PR goes up. CI is not
# changed to call this file; the two are kept in sync by eye.

SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c

COVERAGE_FLOOR := 85
COVERPROFILE := coverage.out

.PHONY: check build vet test coverage-floor lint fmt fmt-check plan-examples schema-index observability-up observability-down

check: build vet test coverage-floor lint fmt-check

build:
	go build ./...

vet:
	go vet ./...

# Matches go-ci.yml's go-race job.
test:
	go test ./... -race

# Matches go-ci.yml's go-build-test job: coverage without -race, and the
# same ratchet floor. Copied byte for byte from go-ci.yml, including the
# inverted awk exit code (0 means "below floor, fail the build").
coverage-floor:
	go test ./... -coverprofile=$(COVERPROFILE)
	@total=$$(go tool cover -func=$(COVERPROFILE) | awk '/^total:/ { sub("%", "", $$NF); print $$NF }'); \
	echo "total statement coverage: $${total}% (floor $(COVERAGE_FLOOR)%)"; \
	if awk -v total="$$total" -v floor="$(COVERAGE_FLOOR)" 'BEGIN { exit (total + 0 < floor + 0) ? 0 : 1 }'; then \
		echo "statement coverage $${total}% is below the $(COVERAGE_FLOOR)% floor" >&2; \
		exit 1; \
	fi

lint:
	golangci-lint run ./...

fmt:
	gofmt -w .

fmt-check:
	@files=$$(gofmt -l .); \
	if [ -n "$$files" ]; then \
		echo "$$files"; \
		echo "gofmt needs to be run on the above files" >&2; \
		exit 1; \
	fi

# Read-only: runs `kraai plan` against every fixture under examples/, per
# examples/README.md. Skips cloudflare-data without CLOUDFLARE_API_TOKEN
# rather than failing, since that fixture needs live Cloudflare/Neon
# credentials the other two fixtures don't.
plan-examples:
	@for dir in examples/*/; do \
		name=$${dir%/}; \
		name=$${name#examples/}; \
		if [ "$$name" = "cloudflare-data" ] && [ -z "$${CLOUDFLARE_API_TOKEN:-}" ]; then \
			echo "skipping $$name: CLOUDFLARE_API_TOKEN is unset"; \
			continue; \
		fi; \
		echo "==> plan $$name"; \
		go run ./cmd/kraai plan kraai-example --dir "$${dir%/}"; \
	done

# Read-only: regenerates the embedded CloudFormation schema index from the
# public registry. Raw schemas are cached under the user cache directory,
# so a repeat run makes no DescribeType calls.
schema-index:
	go generate ./internal/provider/aws/cfschema

# A local OpenTelemetry backend and the kraai dashboard: one grafana/otel-lgtm
# container (collector, Prometheus, Tempo, Grafana), pinned by digest. Ports
# are published on 127.0.0.1 only, and anonymous Grafana access is reduced
# from the image's Admin to Viewer, which can still explore. Works with
# docker too: make observability-up CONTAINER=docker.
CONTAINER ?= podman
OTEL_LGTM_IMAGE := docker.io/grafana/otel-lgtm@sha256:35da4355c58162b6f27ccbd43c6214d565bc29fc9b18baaf43b59202c354577b

# Starts the stack, or leaves a running one and the data in it alone; only
# observability-down removes it.
observability-up:
	@if $(CONTAINER) container exists kraai-otel-lgtm 2>/dev/null || $(CONTAINER) inspect kraai-otel-lgtm >/dev/null 2>&1; then \
		$(CONTAINER) start kraai-otel-lgtm >/dev/null; \
	else \
		$(CONTAINER) run -d --name kraai-otel-lgtm \
			-p 127.0.0.1:3000:3000 -p 127.0.0.1:4318:4318 \
			-e GF_AUTH_ANONYMOUS_ORG_ROLE=Viewer -e GF_USERS_VIEWERS_CAN_EDIT=true \
			-v "$(CURDIR)/dev/observability/dashboards.yaml:/otel-lgtm/grafana/conf/provisioning/dashboards/kraai.yaml:ro,Z" \
			-v "$(CURDIR)/dev/observability/dashboards:/otel-lgtm/kraai-dashboards:ro,Z" \
			$(OTEL_LGTM_IMAGE) >/dev/null; \
	fi
	@echo "Grafana:  http://localhost:3000/d/kraai"
	@echo "Export:   OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318 kraai plan <environment>"

observability-down:
	$(CONTAINER) rm -f kraai-otel-lgtm
