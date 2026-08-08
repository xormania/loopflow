# loopflow

A user-installed CLI that agent harnesses use to coordinate work on one
machine: who holds which packet, which provider session is driving it, what
was attempted and what was refused — backed by an append-only, hash-verified
event chain and a content-addressed artifact store, in SQLite.

One binary, one shared state root (default `~/.local/state/loopflow`). Every
harness that runs `loopflow` sees the same packets, claims, and sessions with
no configuration and no server.

## What it never claims

loopflow records; it does not decide. It issues no acceptance verdicts, does
no scheduling, declares nothing ready, and launches nothing. A stored
artifact, a recorded attempt, or a held claim never makes anything accepted.
Verification here means one thing only: the record you are reading is the
record that was written.

## Install

```
go install github.com/xormania/loopflow/cmd/loopflow@latest
```

## Quickstart

```console
$ loopflow init demo-packet -objective "prove the quickstart"
created packet demo-packet
objective  prove the quickstart
event      seq 1  0e000fb8738ab581b623d6081ff8dfa315e40aff3f432a351baec68477fee43b

$ loopflow claim demo-packet -owner harness-a -ttl 15m
claimed by harness-a until 2026-08-08T21:17:24Z

$ loopflow record demo-packet build -outcome passed
recorded seq 2  build/passed  79fdbff89a4f18e52a503578783bff671fe6cf0a29c6135b37820c23e128f069

$ loopflow verify demo-packet
demo-packet: 2 events, chain verified

$ loopflow release demo-packet -owner harness-a
released demo-packet
```

`loopflow --help` documents every command; `-json` gives machine-readable
output throughout.

## The pieces

- **Packets and events** — `init`, `record`, `log`, `verify`. Each event is
  canonical JSON, hash-chained to its predecessor; `verify` recomputes every
  hash and link. Canonical form is byte-identical to Python's
  `json.dumps(obj, sort_keys=True, separators=(",", ":"))`, so a chain can be
  written or checked from either language.
- **Claims** — `claim`, `release`. "I have this one" as a fact every harness
  can see and none can win twice. TTL-bounded; re-claiming is the heartbeat;
  a dead harness's claim expires instead of wedging the loop. Exit 3 means
  another owner holds it.
- **Sessions** — `session`, `sessions`. A registry of which provider session
  is on which role-task, and the id to resume. Stale means *verify*, never
  dead.
- **Attempts and refusals** — `run`, `attempts`. `run <packet-dir> -- <cmd…>`
  passes output and exit code straight through, so it can sit in front of an
  existing invocation unchanged — and records what happened, including
  refusals, which by design leave no trace in the packet itself.
- **Artifacts** — `put`, `get`. Content-addressed by SHA-256; `get` verifies
  before it writes.
- **In-place check** — `check <packet-dir>` verifies a packet directory's
  `workflow-events.jsonl` where it lies. Reads only; nothing is stored.

## The marker protocol

A tool wrapped by `loopflow run` may print, at the start of a line on stderr:

| Marker             | Meaning                        |
| ------------------ | ------------------------------ |
| `WORKFLOW-ERROR`   | refused; the packet unchanged  |
| `WORKFLOW-FAILED`  | ran and failed                 |
| `WORKFLOW-INFRA`   | could not be judged on merits  |
| `WORKFLOW-BLOCKED` | ran; the packet is blocked     |

Markers are optional. Classification also uses the exit code and whether the
packet's chain grew, and a command that prints nothing is still recorded —
conservatively, never inflated into a claim it didn't make.

## Exit codes

`0` ok · `1` refused or failed · `2` evidence integrity — something did not
re-hash to what was recorded · `3` claim held by another owner · `64` usage.

## State root

`-root DIR`, else `$LOOPFLOW_ROOT`, else `$XDG_STATE_HOME/loopflow`, else
`~/.local/state/loopflow`. Many loopflow processes may share one root: writes
are serialised by SQLite, and appends read the chain tail inside the same
lock, so concurrent recorders queue rather than fork the chain.
