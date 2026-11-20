SHELL := /bin/bash
VERSION ?= 0.1.0
LDFLAGS := -s -w -X main.version=$(VERSION)
PKG := ./cmd/generic-deploy

# Build for this machine's usual server target (Linux x86-64).
build:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/generic-deploy $(PKG)

# Build every supported platform into dist/.
build-all:
	@mkdir -p dist
	GOOS=linux  GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/generic-deploy-linux-amd64  $(PKG)
	GOOS=linux  GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/generic-deploy-linux-arm64  $(PKG)
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/generic-deploy-darwin-amd64 $(PKG)
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/generic-deploy-darwin-arm64 $(PKG)
	@ls -lh dist

test:
	go test ./...

run: build
	./bin/generic-deploy -config deploy.yaml

clean:
	rm -rf bin dist

.PHONY: build build-all test run clean
