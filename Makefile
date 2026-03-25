.PHONY: fmt lint test build clean

fmt:
	gofmt -w .

lint:
	go vet ./...

test:
	CGO_ENABLED=1 go test -race -count=1 ./...

build:
	CGO_ENABLED=0 go build ./...

clean:
	rm -rf bin/
