.PHONY: build lint test test-race integration fuzz-short fuzz-long chaos load-smoke verify-new

# VERSION is stamped into cmd/bifrost's main.version via -ldflags -X; git
# describe gives a real release tag on a tagged commit and a
# commit-ish+dirty marker everywhere else, so `-version` always reports
# something traceable back to a commit.
VERSION ?= $(shell git describe --tags --always --dirty)

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/ ./cmd/...

lint:
	@if command -v gofumpt >/dev/null 2>&1; then \
		gofumpt -l . | (! grep .); \
	elif [ -x "$$(go env GOPATH)/bin/gofumpt" ]; then \
		"$$(go env GOPATH)/bin/gofumpt" -l . | (! grep .); \
	else \
		echo "warning: gofumpt not found on PATH or GOPATH/bin; falling back to gofmt -l (CI installs gofumpt)"; \
		gofmt -l . | (! grep .); \
	fi
	go vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "warning: golangci-lint not found; skipping (CI installs it)"; \
	fi

test:
	go test -count=1 ./...

test-race:
	go test -race -count=1 ./...

# ./... (not ./test/integration/...) is load-bearing: several epics put
# integration-tagged tests inside internal/ packages too; scoping this
# narrower would silently stop running them after their authoring task.
integration:
	go test -race -count=1 -tags=integration ./...

fuzz-short:
	@if ! ls internal/smtpwire/*.go >/dev/null 2>&1; then \
		echo "fuzz-short: internal/smtpwire not present yet, skipping"; \
	else \
		for T in FuzzCommandReader FuzzReplyParser FuzzDataFramer; do \
			go test -run='^$$' -fuzz=$$T -fuzztime=30s ./internal/smtpwire || exit 1; \
		done; \
	fi

fuzz-long:
	@if ! ls internal/smtpwire/*.go >/dev/null 2>&1; then \
		echo "fuzz-long: internal/smtpwire not present yet, skipping"; \
	else \
		for T in FuzzCommandReader FuzzReplyParser FuzzDataFramer; do \
			go test -run='^$$' -fuzz=$$T -fuzztime=10m ./internal/smtpwire || exit 1; \
		done; \
	fi

chaos:
	@if ! ls test/chaos/*.go >/dev/null 2>&1; then \
		echo "chaos: test/chaos has no Go files yet, skipping"; \
	else \
		go test -race -count=1 -tags=chaos -timeout 10m ./test/chaos/...; \
	fi

load-smoke:
	@bash scripts/load_smoke.sh

# verify-new is the anti-silent-pass gate: plain `go test -run` exits 0 even
# when the pattern matches zero tests (e.g. a wrong name, or a name behind a
# build tag nobody passed). This target additionally greps the verbose
# output for an explicit "--- PASS: <name>" line per named test, so a typo
# or a missing TAGS fails loudly instead of passing silently.
verify-new:
	@test -n "$(PKG)" && test -n "$(TESTS)" || { echo "usage: make verify-new PKG=./internal/x TESTS='TestA TestB' [TAGS=integration]"; exit 2; }
	@go test -race -count=1 $(if $(TAGS),-tags=$(TAGS)) -run "^($(shell echo $(TESTS) | tr ' ' '|'))$$" -v $(PKG) | tee /tmp/verify-new.out
	@for t in $(TESTS); do grep -q -- "--- PASS: $$t" /tmp/verify-new.out || { echo "FAIL: test $$t missing or not passing"; exit 1; }; done
