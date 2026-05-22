VERSION := $(shell git describe --tags --always 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X main.version=$(VERSION)
BINARY := sage-router

.PHONY: build test lint release clean dashboard dev grep-no-static-config grep-no-secrets check-no-artifacts

build: dashboard
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/sage-router

dev:
	go run ./cmd/sage-router

test:
	go test ./... -race -cover -count=1

lint: grep-no-static-config grep-no-secrets check-no-artifacts
	golangci-lint run ./...

# AC7 — Models Discovery M1.13. After M1 ships, the only readers of
# config.{ModelCatalog,EstimateCost,GetModel,GetModelPrice} must be
# the seeder (internal/catalog/seed.go) and the constants themselves
# (internal/config/). config.KnownProviders + config.GetProvider stay
# alive as the static provider-definition source (RC1 resolution).
#
# This target greps the whole repo (minus those exclusions) for the
# forbidden tokens. A non-empty match fails the build with explicit
# guidance pointing to ADR-1 §"Field source" / spec §3.3.
grep-no-static-config:
	@hits=$$(git grep -E 'config\.(ModelCatalog|EstimateCost|GetModel|GetModelPrice)\b' -- \
		':(exclude)internal/config/' \
		':(exclude)internal/catalog/seed.go' \
		':(exclude)Makefile' \
		':(exclude).sage/' || true); \
	if [ -n "$$hits" ]; then \
		echo "AC7 violation: forbidden static-config readers found outside the seeder."; \
		echo "Allowed readers: internal/config/ (definitions), internal/catalog/seed.go."; \
		echo "Forbidden tokens: config.ModelCatalog, config.EstimateCost, config.GetModel, config.GetModelPrice."; \
		echo "config.KnownProviders + config.GetProvider stay alive (RC1) — not in the forbidden list."; \
		echo "Hits:"; \
		echo "$$hits"; \
		exit 1; \
	fi

# AC29 — Models Discovery M3.7. Scan `go test -v` output for any byte
# sequence matching production-provider credential shapes. A match
# indicates a test or production code path logged something resembling
# a real API key / OAuth token. The accompanying Go test at
# internal/server/token_leak_test.go pins the same regex set and runs
# under `go test` directly; this Makefile target is defense-in-depth
# at the CI surface for failure classes the Go test might miss
# (e.g., a leak inside a test in a package the log-leak test doesn't
# instrument).
#
# Patterns (mirror tokenLeakPatterns in token_leak_test.go):
#   sk-...           OpenAI API keys (also matches anthropic sk-ant-)
#   AIza...          Google / Gemini API keys
#   ghp_... / gho_   GitHub personal / classic OAuth tokens
#   github_pat_...   GitHub fine-grained PATs (underscores in body)
#   ghu_... / ghs_   GitHub user-to-server / server-to-server OAuth
#   ya29....         Google OAuth access tokens
grep-no-secrets:
	@echo "M3.7 / AC29: scanning go test output for provider-token leaks..."
	@output=$$(go test -v -count=1 ./... 2>&1); \
	if echo "$$output" | grep -qE 'sk-[a-zA-Z0-9_\-]{20,}|AIza[A-Za-z0-9\-_]{20,}|ghp_[A-Za-z0-9]{36,}|gho_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{36,}|ghu_[A-Za-z0-9]{36,}|ghs_[A-Za-z0-9]{36,}|ya29\.[A-Za-z0-9\-_]{40,}'; then \
		echo "AC29 violation: provider-token pattern matched in test output."; \
		echo "Re-run locally with verbose grep to identify the leaking test:"; \
		echo "  go test -v -count=1 ./... 2>&1 | grep -E 'sk-[a-zA-Z0-9_\\-]{20,}|AIza[A-Za-z0-9\\-_]{20,}|...'"; \
		echo "Do NOT paste the matched lines into CI output — they're real-shaped credentials."; \
		exit 1; \
	fi; \
	echo "OK: no provider-token patterns found in test output"

# Tier 3 hygiene (cycle 20260522-tier3-hygiene, OmniRoute analysis §10 #10).
# Build artifacts must never be committed. bin/, dist/, and *.exe are
# gitignored, but a `git add -f` or a .gitignore regression could still
# slip one in — this gate is the belt to the .gitignore's suspenders: it
# scans the tracked tree (`git ls-files`) and fails on any committed build
# artifact. Deny pattern:
#   *.{exe,dll,so,dylib,a,o,test}     compiled-binary / object extensions
#   bin/* , dist/*                    build-output directories
#   sage-router , sage-router-<arch>  the canonical binary name — the
#                                     extensionless ELF `go build -o
#                                     sage-router` produces. The suffix
#                                     class has no `.`, so the tracked
#                                     sage-router-logo.svg /
#                                     sage-router-dashboard-screenshot.png
#                                     repo assets do NOT match.
check-no-artifacts:
	@hits=$$(git ls-files | grep -E '\.(exe|dll|so|dylib|a|o|test)$$|^bin/|^dist/|(^|/)sage-router(-[A-Za-z0-9_-]+)?$$' || true); \
	if [ -n "$$hits" ]; then \
		echo "artifact-hygiene violation: build artifacts found in the tracked tree."; \
		echo "Compiled binaries / build outputs must never be committed —"; \
		echo "they belong in bin/ or dist/ (both gitignored)."; \
		echo "Tracked artifacts:"; \
		echo "$$hits"; \
		exit 1; \
	fi; \
	echo "OK: no build artifacts in the tracked tree"

dashboard:
	@if [ -d "web/dashboard/node_modules" ]; then \
		cd web/dashboard && npm run build; \
	else \
		echo "Dashboard: using placeholder (run 'cd web/dashboard && npm install && npm run build' for full dashboard)"; \
	fi

release: dashboard
	@mkdir -p dist
	GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-amd64 ./cmd/sage-router
	GOOS=linux   GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-arm64 ./cmd/sage-router
	GOOS=darwin  GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-darwin-amd64 ./cmd/sage-router
	GOOS=darwin  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-darwin-arm64 ./cmd/sage-router
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-windows-amd64.exe ./cmd/sage-router
	cd dist && sha256sum * > checksums.txt 2>/dev/null || true

docker:
	docker build -t $(BINARY):$(VERSION) .

clean:
	rm -rf bin/ dist/
	rm -rf web/dashboard/dist/
