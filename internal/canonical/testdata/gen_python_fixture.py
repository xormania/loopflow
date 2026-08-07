#!/usr/bin/env python3
"""Regenerate python-canonical.json — the CPython cross-check fixture.

The Go encoder in internal/canonical must reproduce, byte for byte, what the
native Flow tooling produces with

    json.dumps(obj, sort_keys=True, separators=(",", ":"))

This script records CPython's actual output for a set of cases so the Go test
compares against a real interpreter's bytes rather than against a Go
programmer's belief about them. Tests read only the generated JSON; python3 is
not required to run the test suite.

    python3 internal/canonical/testdata/gen_python_fixture.py

Cases that cannot round-trip through a JSON fixture — lone surrogates,
duplicate keys, float rejection — are asserted in Go directly.

Control characters are written as chr() rather than as literals so this file
stays free of raw control bytes.
"""

import hashlib
import json
import os
import platform

NUL = chr(0x00)
UNIT_SEP = chr(0x1F)
DEL = chr(0x7F)
NBSP = chr(0xA0)
EMOJI = chr(0x1F600)  # non-BMP: surrogate pair under ensure_ascii
CLEF = chr(0x1D11E)  # non-BMP
E_ACUTE = chr(0xE9)
CJK = chr(0x4E2D) + chr(0x6587)

CASES = [
    ("empty-object", {}),
    ("empty-array", []),
    ("scalars", {"t": True, "f": False, "n": None}),
    # Key ordering: insertion order is deliberately not sorted order, and the
    # set spans ASCII case boundaries, digits, and non-ASCII keys.
    (
        "key-order-ascii",
        {"b": 1, "A": 2, "a": 3, "B": 4, "_": 5, "0": 6, "~": 7, "-": 8},
    ),
    ("key-order-non-ascii", {"z": 1, E_ACUTE: 2, CJK: 3, "a": 4, EMOJI: 5}),
    ("key-order-prefix", {"ab": 1, "a": 2, "abc": 3, "": 4}),
    # Integer rendering: no exponent, no decimal point, exact beyond float64.
    (
        "integers",
        {
            "zero": 0,
            "neg": -1,
            "small": 42,
            "max_i64": 9223372036854775807,
            "min_i64": -9223372036854775808,
            "beyond_float64": 9007199254740993,
            "bignum": 123456789012345678901234567890,
            "neg_bignum": -123456789012345678901234567890,
        },
    ),
    # HTML-significant characters must stay literal: Python does not escape
    # them and Go's encoder escapes < > & by default.
    ("html-chars", {"s": "<script>&amp;</script>", "slash": "a/b", "amp": "&"}),
    # ensure_ascii=True escaping, including non-BMP runes as surrogate pairs
    # and DEL, which is outside Python's literal range but inside Go's.
    (
        "escapes",
        {
            "quote": '"',
            "backslash": "\\",
            "newline": "\n",
            "tab": "\t",
            "cr": "\r",
            "backspace": "\b",
            "formfeed": "\f",
            "nul": NUL,
            "unit-sep": UNIT_SEP,
            "del": DEL,
            "nbsp": NBSP,
            "accented": "caf" + E_ACUTE,
            "cjk": CJK,
            "emoji": EMOJI,
            "astral-pair": CLEF,
            "mixed": "a" + NUL + "b" + DEL + "c" + EMOJI,
        },
    ),
    ("nested", {"b": [1, {"y": 2, "x": [3, [], {}]}, None], "a": {"k": "v"}}),
    # The real production event from environment-facts 3c, minus its own hash
    # field — the exact object the golden vector hashes.
    (
        "golden-vector-event-without-hash",
        {
            "issue_key": "initialized",
            "outcome": "passed",
            "phase": "init",
            "prev": "0" * 64,
            "seq": 1,
            "state_sha256": "0e251dcac955cf58eede51cfd535ea5419c31dc811f5b93b1d24f6588d63886e",
            "time": "2026-08-05T20:32:30.615324Z",
        },
    ),
]


def main() -> None:
    out = []
    for name, value in CASES:
        text = json.dumps(value, sort_keys=True, separators=(",", ":"))
        out.append(
            {
                "name": name,
                "input": value,
                "canonical": text,
                "sha256": hashlib.sha256(text.encode("utf-8")).hexdigest(),
            }
        )

    path = os.path.join(
        os.path.dirname(os.path.abspath(__file__)), "python-canonical.json"
    )
    with open(path, "w", encoding="utf-8") as fh:
        json.dump(
            {
                "generator": "internal/canonical/testdata/gen_python_fixture.py",
                "form": 'json.dumps(obj, sort_keys=True, separators=(",", ":"))',
                "python": platform.python_version(),
                "cases": out,
            },
            fh,
            indent=2,
            sort_keys=True,
        )
        fh.write("\n")
    print(f"wrote {len(out)} cases to {path}")


if __name__ == "__main__":
    main()
