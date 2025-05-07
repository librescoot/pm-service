.PHONY: build clean build-arm build-amd64 dist lint test

BINARY_NAME=pm-service
VERSION=$(shell git describe --tags --always --dirty 2>/dev/null || echo "0.1.0")
LDFLAGS=-ldflags "-w -s -X main.version=$(VERSION) -extldflags '-static'"

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build $(LDFLAGS) -o $(BINARY_NAME) ./cmd/pm-service

clean:
	rm -f $(BINARY_NAME) $(BINARY_NAME)-amd64 $(BINARY_NAME)-dist

build-arm:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build $(LDFLAGS) -o $(BINARY_NAME) ./cmd/pm-service

build-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BINARY_NAME)-amd64 ./cmd/pm-service

dist:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -tags netgo,osusergo $(LDFLAGS) -o $(BINARY_NAME)-dist ./cmd/pm-service

lint:
	golangci-lint run

test:
	go test -v ./...