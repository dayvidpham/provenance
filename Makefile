.PHONY: build fmt lint test clean

build: fmt lint test
	CGO_ENABLED=0 go build ./...

fmt:
	gofmt -w .

lint:
	go vet ./...
	ast-grep scan --config sgconfig.yml .

test:
	go test -count=2 ./...
	CGO_ENABLED=1 go test -race -count=2 -timeout=20m ./...

clean:
	rm -rf bin/
