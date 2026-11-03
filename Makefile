VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
LDFLAGS := -s -w -X github.com/old-stock/open-gpu-mesh/internal/api.Version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: build test vet dist dev docker clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/ogm ./cmd/ogm

test:
	go test ./...

vet:
	go vet ./...

# Cross-compiled binaries served by `ogm server --dist-dir dist` at /dl/{os}-{arch}.
dist:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; out=dist/ogm-$$os-$$arch; \
	  echo "==> $$out"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o $$out ./cmd/ogm || exit 1; \
	done

dev: build
	./bin/ogm dev

docker:
	docker build -t ghcr.io/old-stock/open-gpu-mesh:$(VERSION) .

clean:
	rm -rf bin dist ogm-data
