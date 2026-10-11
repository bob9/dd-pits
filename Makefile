VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

TARGETS := darwin/arm64 darwin/amd64 windows/amd64 linux/amd64 linux/arm64

.PHONY: build test release clean

build:
	go build -ldflags "$(LDFLAGS)" -o bin/dd-pits ./cmd/dd-pits

test:
	go vet ./...
	go test ./...

# Cross-compile for every supported platform into ./dist.
release:
	@mkdir -p dist
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "building $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o dist/dd-pits-$$os-$$arch$$ext ./cmd/dd-pits || exit 1; \
	done

clean:
	rm -rf bin dist
