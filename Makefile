.PHONY: build fmt lint test test-local clean

build: fmt lint test
	CGO_ENABLED=0 go build ./...

fmt:
	gofmt -w .

lint:
	go vet ./...
	ast-grep scan --config sgconfig.yml --globs '!vendor/**' --globs '!worktree/**' .

# One authoritative race suite, matching flake.nix and TESTING.md. There is no
# non-race wave: running the suite twice covers no extra configuration. No
# invocation specifies -count, -p, or -parallel. CGO_ENABLED=0 is build-only.
test:
	CGO_ENABLED=1 go test -race -shuffle=on -fullpath -timeout=20m ./...

# Focused local iteration under the same race-only policy. Narrow with PKG/RUN
# rather than by dropping -race, e.g.
#   make test-local PKG=./internal/sqlite RUN='^TestPool'
# A focused diagnostic does not replace `make test`.
PKG ?= ./...
RUN ?= .
test-local:
	CGO_ENABLED=1 go test -race -shuffle=on -fullpath -timeout=20m -run '$(RUN)' $(PKG)

clean:
	rm -rf bin/
