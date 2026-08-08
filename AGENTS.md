# AGENTS.md — loopflow repo operating rules

You are developing `loopflow`: a small Go CLI that tracks workflow packets as an
append-only, hash-verified event chain in SQLite, plus a content-addressed
artifact store.

Status: hosted at `github.com/xormania/loopflow`. Agents work locally under
the `xor-machine` machine identity (collaborator: push, no admin).

## GitHub workflow

- Branch from `main`, push the branch, open a **draft PR against `main`**.
  Never push to `main` directly. Marking a PR ready, approving, and merging
  are xormania's web-UI acts — do not do them.
- **Attribution is xormania only.** Author and committer stay
  `xormania <127287135+xormania@users.noreply.github.com>` (the existing git
  config). No `Co-Authored-By` trailers. No tool names anywhere in branch
  names, commit messages, or PR text — no generated-with footers.
- **Naming:**
  - Commit: `<area>: <imperative summary>` — `<area>` is the command or
    package touched (`check`, `attempts`, `store`, `canonical`, …), or `repo`
    for anything else. The body states the decision and its reason, not the
    story of arriving at it.
  - Branch: `<area>/<short-slug>`.
  - PR title: same `<area>: <summary>` form as its headline commit.
- Forge operations allowed: pushing non-main branches, `gh pr create --draft`,
  reading PRs/issues, commenting on your own PR. Everything else (merge,
  ruleset, repo settings) is not yours.

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
