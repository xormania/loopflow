# AGENTS.md — control-plane repo operating rules

Generated: 2026-08-06 21:45 EDT

You are developing this repo: a Go workflow control-plane application —
`flowd` (control server) and `wfc` (CLI client).

Status: **local-only.** No remote, no GitHub, no hosted CI for now. Do not
run `gh`, push, PR, or any forge operation against this repo. (This applies
to this repository's own development only — the *product* still builds forge
adapters for its target projects; see `proj/build-plan.md` Phase 7.)

## Ground rules — deliberately minimal

- `proj/` is the untracked handoff package (specs, decisions, plans, tasks).
  Read it; never commit anything under it. Start at `proj/README.md`. The
  active work is defined by the current task file — first:
  `proj/kickoff-task.md`.
- When documents disagree: `proj/spec/` > `proj/decisions.md` > everything
  else. Report conflicts; don't reinterpret.
- Toolchain: Go 1.26. Runtime dependencies are closed per
  `proj/decisions.md` D4 (`modernc.org/sqlite` only); sqlc is dev-time only.
- Before every commit: `go vet ./...` and `go test ./...` green.
- Otherwise unrestricted: edit any repo file outside `proj/`, run any local
  command. No approval loops.

## Commands

```bash
go build ./...
go vet ./...
go test ./...
```
