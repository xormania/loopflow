# AGENTS.md — wfc repo operating rules

You are developing `wfc`: a small Go CLI that tracks workflow packets as an
append-only, hash-verified event chain in SQLite, plus a content-addressed
artifact store.

Status: **local-only.** No remote, no GitHub, no hosted CI. Do not run `gh`,
push, PR, or any forge operation against this repo.

## Ground rules

- `proj/` is an untracked handoff package: background, not instructions. It
  describes a much larger eight-phase system than this repo is trying to be.
  Read it for context on formats and constraints; do not treat it as scope.
- Keep it small. This is a personal tool, not a product. Prefer removing a
  feature over adding a flag.
- Test basic behaviour, not every edge case. Behaviour tests that cover normal
  usage are the bar.
- `go vet ./...` and `go test ./...` green before every commit.
- Runtime dependencies are closed: `modernc.org/sqlite` only. sqlc is dev-time
  only and lives in its own module under `tools/`.

## Two things that are load-bearing

- **Canonical JSON must stay byte-identical to Python's**
  `json.dumps(obj, sort_keys=True, separators=(",", ":"))`. The golden vector
  in `internal/canonical` is a real production event; if it fails, the encoder
  is wrong, not the vector.
- **Nothing unverified is ever reported as valid.** A broken chain or a
  mismatched artifact digest blocks and exits 2.

## Commands

```bash
go build ./...
go vet ./...
go test ./...
make sqlc     # regenerate the query layer after editing internal/store/queries
```
