BINARY := c-txn

.PHONY: all build test lint lint-actions fmt clean

all: lint lint-actions test build

build:
	go build -o $(BINARY) .

test:
	go test ./...

lint:
	golangci-lint run ./...

lint-actions:
	actionlint

fmt:
	go fmt ./...

clean:
	rm -f $(BINARY)
