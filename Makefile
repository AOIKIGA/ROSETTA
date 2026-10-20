.PHONY: build test check

build:
	mkdir -p bin
	go build -o bin/rosetta ./cmd/rosetta

test:
	go test -race ./...

check:
	go vet ./...
	go test -race ./...
