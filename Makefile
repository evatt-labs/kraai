# Mirrors .github/workflows/go-ci.yml so `make check` runs the
# same checks CI does, from one command, before a PR goes up. CI is not
# changed to call this file; the two are kept in sync by eye.

SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c

COVERAGE_FLOOR := 85
COVERPROFILE := coverage.out

.PHONY: check build vet test coverage-floor lint fmt fmt-check plan-examples

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
