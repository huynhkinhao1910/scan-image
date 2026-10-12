# CLAUDE.md

macOS app for Everything-style search over a TB-scale image NAS mounted via SMB (often over Kerio VPN). Go + MyGo native UI (`github.com/egoist/mygo/ui`, no webview). Each Mac runs standalone.

## Commands
- `go test ./internal/...` — core tests (no UI deps)
- `go test -bench . ./internal/search` — search benchmark (target: 3M names, p95 < 50ms)
- `go tool mygo dev` — run the app with reload
- `go run ./cmd/nasprobe -path /Volumes/X` — measure listing throughput

## Architecture rules
- `internal/*` must never import mygo; UI lives in package main only.
- All file access goes through `source.Source`; add adapters (e.g. remote host), do not bypass it.
- Network latency is the bottleneck: never stat or read files one-by-one on the UI thread; keep many listings in flight.
- Search runs in RAM over the flat arenas in `internal/search`; do not move search to SQLite. SQLite (planned) is persistence only, stored on local disk, never on the NAS.
- UI state changes from goroutines must go through `win.Update(func(){...})`.
- Read MyGo docs in the module cache (`docs/ui/*.md`) before using a widget; API is v0.x and changes.

## Style
- No inline comments unless necessary; doc comments on exported identifiers.
- Run `gofmt` and `go vet ./...` before finishing.
