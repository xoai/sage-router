# AGENTS.md

Vendor-neutral onboarding for AI agents and contributors working in the
sage-router repository. For a one-screen project overview see `llm.txt`;
for the full Sage workflow and process rules see `CLAUDE.md`.

## Build

- `make build` — builds the Preact dashboard, then the Go binary.
- `make dashboard` — builds the dashboard alone (Vite).

## Test

- `go test ./... -race -count=1` — the full Go suite under the race
  detector. This is the exact command CI runs.

## Lint

- `make lint` — runs `golangci-lint run ./...` plus the repository's hygiene
  gates: `grep-no-static-config`, `grep-no-secrets`, and `check-no-artifacts`
  (no tracked build artifacts). `golangci-lint` must be installed locally;
  CI invokes the gates directly, not `make lint`.

## Project layout

| Path | What |
|------|------|
| `cmd/sage-router/` | entrypoint + bootstrap wiring |
| `internal/` | implementation packages — server, translate, provider, executor, routing, store, auth, catalog, cost, compress, usage |
| `pkg/canonical/` | the provider-agnostic request/response IR |
| `web/dashboard/` | the Preact dashboard SPA |

## Conventions

- Tests live beside the code they cover — table-driven tests for logic,
  golden-file tests for translation fidelity.
- Database migrations are additive (`ALTER TABLE ... ADD COLUMN`), numbered
  `NNN_name.sql` under `internal/store/migrations/`.
- Secrets never appear in source — use environment variables or the
  encrypted store.
- The default request path stays fast; new pipeline stages are opt-in.
- Build artifacts (compiled binaries, `bin/`, `dist/`) are never committed —
  `make lint`'s `check-no-artifacts` gate enforces this.
