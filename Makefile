.PHONY: build test lint generate clean

build:
	go build ./...

test:
	go test ./...

lint:
	golangci-lint run ./...

generate:
	go generate ./...

clean:
	rm -rf bin/
