# CLAUDE.md

macOS app for Everything-style search over a TB-scale image NAS mounted via SMB (often over Kerio VPN). Go + MyGo native UI (`github.com/egoist/mygo/ui`, no webview). Each Mac runs standalone.

## Commands
- `go test -race ./internal/...` — core tests (no UI deps)
- No Xcode command line tools on the machine? Prefix go commands with `CGO_ENABLED=0` (purego pulls in `runtime/cgo` when cgo is on).
- `go test -bench . ./internal/search` — search benchmark (target: 3M names, p95 < 50ms)
- `go tool mygo dev` — run the app with reload
- `go run ./cmd/nasprobe -path /Volumes/X -lister readdir,bulk` — measure listing throughput

## Architecture rules
- `internal/*` must never import mygo; UI lives in package main only, which is the `app/` directory (`"main": "./app"` in `mygo.json`; `mygo.json`, `resources/` and `icons/` stay at the root).
- All file access goes through `source.Source`; add adapters (e.g. remote host), do not bypass it.
- Network latency is the bottleneck: never stat or read files one-by-one on the UI thread; keep many listings in flight.
- Search runs in RAM over the flat arenas in `internal/search`; do not move search to SQLite. Persistence is a binary snapshot of the arenas (`internal/search/snapshot.go`, driven by `internal/indexer`), stored on local disk, never on the NAS.
- The index is append-only: a rescan builds a new index from the old one (`search.Builder`) and swaps it in; never edit a published index in place.
- The UI talks to `indexer.Service` only (`Current`, `State`, `Scan`, `Stop`, `Close`); it does not drive the scanner or the store's snapshot files itself.
- UI state changes from goroutines must go through `win.Update(func(){...})`.
- Read MyGo docs in the module cache (`docs/ui/*.md`) before using a widget; API is v0.x and changes.

## Style
- No inline comments unless necessary; doc comments on exported identifiers.
- Run `gofmt` and `go vet ./...` before finishing.
