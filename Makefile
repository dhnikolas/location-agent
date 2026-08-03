APP        := location-agent
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILD_DIR  := build
DIST_DIR   := dist
LDFLAGS    := -s -w -X main.Version=$(VERSION)
GOFLAGS    := -trimpath
CGO_ENABLED ?= 0

# The published artifacts. macOS only for now: the agent drives the local docker
# daemon on someone's laptop, and that is the only place it has been run.
PLATFORMS := darwin/arm64 darwin/amd64

.PHONY: all
all: build

.PHONY: build
build:
	go build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o $(BUILD_DIR)/$(APP) ./cmd/app

.PHONY: dist
dist: clean-dist
	@mkdir -p $(DIST_DIR)
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "  $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=$(CGO_ENABLED) \
			go build $(GOFLAGS) -ldflags="$(LDFLAGS)" \
			-o $(DIST_DIR)/$(APP)-$$os-$$arch ./cmd/app || exit 1; \
	done
	@cd $(DIST_DIR) && shasum -a 256 $(APP)-* > SHA256SUMS
	@echo "built $(VERSION):" && ls -la $(DIST_DIR)

# Upload what dist built to a GitHub release. Split from dist on purpose: the
# build needs the private module proxy, publishing needs a GitHub token, and
# the machine that has one is not always the machine that has the other.
.PHONY: release
release: dist
	VERSION=$(VERSION) ./scripts/release.sh

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test:
	go test ./...

.PHONY: clean-dist
clean-dist:
	rm -rf $(DIST_DIR)

.PHONY: clean
clean: clean-dist
	rm -rf $(BUILD_DIR)/$(APP)

.PHONY: print-version
print-version:
	@echo $(VERSION)
