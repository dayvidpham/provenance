.PHONY: build fmt lint test test-local clean

build: fmt lint test
	CGO_ENABLED=0 go build ./...

fmt:
	gofmt -w .

lint:
	go vet ./...
	ast-grep scan --config sgconfig.yml .

test:
	go test -count=1 -shuffle=on -fullpath -timeout=10m ./...
	CGO_ENABLED=1 go test -race -count=1 -shuffle=on -fullpath -timeout=20m ./...

test-local:
	go test ./...

clean:
	rm -rf bin/
