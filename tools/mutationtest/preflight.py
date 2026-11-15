#!/usr/bin/env python3
"""Preflight: assert every mutation anchor occurs exactly once in mapstructure.go."""

from __future__ import annotations

import pathlib
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import mutate_hook_hierarchy as m  # noqa: E402


def main() -> int:
    repo = pathlib.Path(sys.argv[1])
    src = m.normalize((repo / m.TARGET_FILE).read_text(encoding="utf-8"))
    ok = True
    for mut in m.MUTATIONS:
        n = src.count(m.normalize(mut.old))
        status = "ok" if n == 1 else "AMBIGUOUS"
        if n != 1:
            ok = False
        print(f"{mut.mid:<32} anchors={n} {status}")
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())
