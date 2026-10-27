# Build, test and check gitpull.
#
# Every setting can be overridden on the command line or in the environment, e.g.
#
#   make cover COVER_MIN=60
#   make audit DENYLIST=~/denylist.txt

# ---- config ---------------------------------------------------------------------

# Minimum statement coverage of the whole test suite (make cover).
COVER_MIN ?= 60
# Words no file of this repository may contain (make audit), one per line, # comments allowed.
# Kept out of the repository: local/ is gitignored.
DENYLIST  ?= local/denylist.txt
# Release builds (make release): the version they report, the platforms
# (os/arch) and where they go.
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo devel)
PLATFORMS ?= darwin/amd64 darwin/arm64 linux/amd64 linux/arm64
DIST      ?= bin
SHA256    ?= $(if $(shell command -v sha256sum 2>/dev/null),sha256sum,shasum -a 256)
BIN       ?= tmp/bin/gitpull

# ---- targets --------------------------------------------------------------------

.PHONY: help build test cover audit release clean
.DEFAULT_GOAL := help

help: ## List the targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{ printf "  make %-7s %s\n", $$1, $$2 }'

release: ## Build gitpull for PLATFORMS (macOS and Linux, amd64 and arm64) into DIST (bin/) with SHA256SUMS; VERSION=v1.2.0 names them
	@mkdir -p $(DIST) && rm -f $(DIST)/gitpull-* $(DIST)/SHA256SUMS
	@for p in $(PLATFORMS); do os=$${p%/*}; arch=$${p#*/}; out=$(DIST)/gitpull-$(VERSION)-$$os-$$arch; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $$out ./cmd/gitpull || exit 1; \
		echo "$$out"; done
	@cd $(DIST) && $(SHA256) gitpull-* > SHA256SUMS
	@echo "$(DIST)/SHA256SUMS: upload it with the binaries"

test: ## gofmt, go vet and the tests
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "run gofmt -w ."; exit 1; }
	go vet ./...
	go test ./...

cover: ## Statement coverage of the whole test suite; fails under COVER_MIN %
	@mkdir -p tmp
	@go test ./... -coverpkg=./... -coverprofile=tmp/cover.out > tmp/cover.log 2>&1 || { tail -20 tmp/cover.log; exit 1; }
	@go tool cover -func=tmp/cover.out | awk '/^total:/ { gsub("%", "", $$3); printf "coverage: %s%% of statements (minimum $(COVER_MIN)%%); details: go tool cover -html=tmp/cover.out\n", $$3; if ($$3 + 0 < $(COVER_MIN)) exit 1 }'

audit: ## Check that no file of this repository contains a word of DENYLIST (private names)
	@test -f "$(DENYLIST)" || { echo "no denylist at $(DENYLIST) (set DENYLIST)"; exit 1; }
	@mkdir -p tmp && sed -e 's/\#.*//' -e 's/[[:space:]]*$$//' -e '/^$$/d' $(DENYLIST) > tmp/.denylist
	@files=$$(if git rev-parse --git-dir >/dev/null 2>&1; then git ls-files --cached --others --exclude-standard; \
		else find . -type f -not -path './.git/*' -not -path './tmp/*' -not -path './.idea/*' -not -path './local/*'; fi); \
	test -n "$$files" || { echo "audit: no file to check"; exit 1; }; \
	if grep -n -i -w -F -f tmp/.denylist $$files; then echo "audit: the files above name what $(DENYLIST) lists"; exit 1; \
	else echo "audit: no file names any of the $$(wc -l < tmp/.denylist | tr -d ' ') words of $(DENYLIST)"; fi

clean: ## Remove the coverage files and the built binary
	rm -rf tmp
