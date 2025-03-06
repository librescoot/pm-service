VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.1.0")
LDFLAGS := -X main.version=$(VERSION)
GOFLAGS := -trimpath
BINARY_NAME := librescoot-pm
INSTALL_PATH := /usr/bin

# Target architecture: ARMv7 for iMX6UL
GOOS := linux
GOARCH := arm
GOARM := 7
CGO_ENABLED := 0

# Target device for development
DEV_TARGET_HOST := root@192.168.7.1
DEV_TARGET_PATH := /tmp

.PHONY: all build clean install test deploy-dev

all: build

build:
	@echo "Building $(BINARY_NAME) version $(VERSION) for $(GOARCH)v$(GOARM)..."
	GOOS=$(GOOS) GOARCH=$(GOARCH) GOARM=$(GOARM) CGO_ENABLED=$(CGO_ENABLED) \
	go build $(GOFLAGS) -ldflags "$(LDFLAGS) -s -w" -o $(BINARY_NAME) ./cmd/pm-service

build-local:
	@echo "Building $(BINARY_NAME) version $(VERSION) for local architecture..."
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY_NAME) ./cmd/pm-service

deploy-dev: build
	@echo "Deploying $(BINARY_NAME) to development device..."
	scp $(BINARY_NAME) $(DEV_TARGET_HOST):$(DEV_TARGET_PATH)/
	@echo "To run the service on the device, use:"
	@echo "  ssh $(DEV_TARGET_HOST) $(DEV_TARGET_PATH)/$(BINARY_NAME)"

deploy-prod: build
	@echo "Deploying $(BINARY_NAME) to production path on device..."
	scp $(BINARY_NAME) $(DEV_TARGET_HOST):$(INSTALL_PATH)/
	ssh $(DEV_TARGET_HOST) "chmod +x $(INSTALL_PATH)/$(BINARY_NAME)"
	@echo "To start the service, run:"
	@echo "  ssh $(DEV_TARGET_HOST) 'systemctl restart librescoot-pm.service'"

clean:
	@echo "Cleaning..."
	rm -f $(BINARY_NAME)
	go clean

install: build
	@echo "Installing $(BINARY_NAME) to $(DEV_TARGET_HOST):$(INSTALL_PATH)..."
	ssh $(DEV_TARGET_HOST) "systemctl stop librescoot-pm.service"
	scp $(BINARY_NAME) $(DEV_TARGET_HOST):$(INSTALL_PATH)/
	ssh $(DEV_TARGET_HOST) "chmod +x $(INSTALL_PATH)/$(BINARY_NAME)"
	@echo "Installing systemd service..."
	scp librescoot-pm.service $(DEV_TARGET_HOST):/etc/systemd/system/
	@echo "Reloading systemd and enabling service..."
	ssh $(DEV_TARGET_HOST) "systemctl daemon-reload && systemctl enable --now librescoot-pm.service"
	@echo "Installation complete. Service should be running on target device."

test:
	@echo "Running tests..."
	go test -v ./...

deps:
	@echo "Installing dependencies..."
	go mod tidy
