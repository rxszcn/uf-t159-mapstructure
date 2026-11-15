#!/usr/bin/env python3
"""Mutation harness for the DecodeHook execution-hierarchy contract.

This is *not* part of the Go package and never runs under ``go test``: it lives
under tools/ so the build ignores it. For every defined mutation it copies the
repository into an isolated temporary working tree, applies one exact source
rewrite to ``mapstructure.go``, runs the hierarchy regression suite, and
records whether the mutation is killed.

Contract guarantee (see docs/hook-execution-hierarchy-contract.md):

* every hook-level mutation (skip a key/container/element channel, or close the
  DecodeNil gate) MUST be killed by the hierarchy tests;
* a non-hook control mutation that leaves every hook invocation intact
  (reversing slice element order) MUST survive, proving the assertions really
  observe hook behaviour rather than incidental decode output.

Run with the interpreter isolated from the working tree:

    python -I tools/mutationtest/mutate_hook_hierarchy.py \
        --repo D:/Projects/.../t159-mapstructure

The machine-readable result is written next to this file as
``mutation-report.txt`` and printed to stdout.
"""

from __future__ import annotations

import argparse
import shutil
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path

TARGET_FILE = "mapstructure.go"

# A targeted, fast subset that must fail for every hook-contract mutation.
TARGET_TEST = "TestDecodeHookHierarchy"


@dataclass(frozen=True)
class Mutation:
    mid: str
    description: str
    old: str
    new: str
    # "kill"  -> hierarchy tests are expected to fail (contract defended);
    # "survive" -> negative control that must not be blamed on the hook contract.
    expectation: str


# Exact, uniquely-identifiable rewrites. Each ``old`` block is asserted to
# occur exactly once, so a future refactor fails the harness loudly instead of
# silently mutating the wrong spot.

# The single central hook-dispatch block in Decoder.decode. Several mutations
# only change the guard on this block, so it is defined once and kept in sync
# with mapstructure.go; the preflight check fails loudly if they drift apart.
DISPATCH_OLD = """\tif d.cachedDecodeHook != nil {
\t\t// We have a DecodeHook, so let's pre-process the input.
\t\t//
\t\t// This is the single dispatch point of the DecodeHook execution
\t\t// hierarchy: every decoded node (root and nested containers, map keys
\t\t// and values, slice elements, leaf values) reaches the hook exactly
\t\t// once here, in a depth-first pre-order walk driven by the recursive
\t\t// decoders below. Do not bypass this block for a particular kind: that
\t\t// silently removes a hook level. The contract and its regression tests
\t\t// live in docs/hook-execution-hierarchy-contract.md and
\t\t// decode_hook_hierarchy_test.go.
\t\tvar err error
\t\tinput, err = d.cachedDecodeHook(inputVal, outVal)
\t\tif err != nil {
\t\t\treturn newDecodeError(name, err)
\t\t}
\t}"""


def dispatch_with_guard(guard: str) -> str:
    """Return the dispatch block with its opening guard replaced by ``guard``."""
    return DISPATCH_OLD.replace(
        "if d.cachedDecodeHook != nil {", f"if {guard} {{", 1
    )


MUTATIONS: tuple[Mutation, ...] = (
    Mutation(
        mid="M01-skip-map-key-channel",
        description="Do not recurse through decode() for map keys (skip key hook channel).",
        old="""\t// First decode the key into the proper type
\t\tcurrentKey := reflect.Indirect(reflect.New(valKeyType))
\t\tif err := d.decode(fieldName, k.Interface(), currentKey); err != nil {
\t\t\terrs = append(errs, err)
\t\t\tcontinue
\t\t}""",
        new="""\t// MUTATION: key channel bypasses decode(), so the hook never sees keys.
\t\tcurrentKey := reflect.Indirect(reflect.New(valKeyType))
\t\tcurrentKey.Set(reflect.ValueOf(k.Convert(valKeyType).Interface()))""",
        expectation="kill",
    ),
    Mutation(
        mid="M02-skip-map-value-channel",
        description="Do not recurse through decode() for map element values (skip value hook channel).",
        old="""\t// Next decode the data into the proper type
\t\tv := dataVal.MapIndex(k).Interface()
\t\tcurrentVal := reflect.Indirect(reflect.New(valElemType))
\t\tif err := d.decode(fieldName, v, currentVal); err != nil {
\t\t\terrs = append(errs, err)
\t\t\tcontinue
\t\t}""",
        new="""\t// MUTATION: value channel bypasses decode(), so the hook never sees map values.
\t\tv := dataVal.MapIndex(k).Interface()
\t\tcurrentVal := reflect.Indirect(reflect.New(valElemType))
\t\tif cv, ok := v.(reflect.Value); ok {
\t\t\tcurrentVal.Set(cv)
\t\t} else {
\t\t\tcurrentVal.Set(reflect.ValueOf(v))
\t\t}""",
        expectation="kill",
    ),
    Mutation(
        mid="M03-skip-map-container-level",
        description="Bypass the hook for map-kind inputs at the central dispatch point.",
        old=DISPATCH_OLD,
        new=dispatch_with_guard(
            "d.cachedDecodeHook != nil && inputVal.Kind() != reflect.Map"
        ),
        expectation="kill",
    ),
    Mutation(
        mid="M04-skip-slice-container-level",
        description="Bypass the hook for slice-kind inputs at the central dispatch point.",
        old=DISPATCH_OLD,
        new=dispatch_with_guard(
            "d.cachedDecodeHook != nil && inputVal.Kind() != reflect.Slice"
        ),
        expectation="kill",
    ),
    Mutation(
        mid="M05-skip-struct-container-level",
        description="Bypass the hook for struct-kind inputs at the central dispatch point.",
        old=DISPATCH_OLD,
        new=dispatch_with_guard(
            "d.cachedDecodeHook != nil && inputVal.Kind() != reflect.Struct"
        ),
        expectation="kill",
    ),
    Mutation(
        mid="M06-skip-slice-element-channel",
        description="Skip the per-element decode() recursion inside decodeSlice.",
        old="""\tfor i := 0; i < dataVal.Len(); i++ {
\t\tcurrentData := dataVal.Index(i).Interface()
\t\tfor valSlice.Len() <= i {
\t\t\tvalSlice = reflect.Append(valSlice, reflect.Zero(valElemType))
\t\t}
\t\tcurrentField := valSlice.Index(i)

\t\tfieldName := name + "[" + strconv.Itoa(i) + "]"
\t\tif err := d.decode(fieldName, currentData, currentField); err != nil {
\t\t\terrs = append(errs, err)
\t\t}""",
        new="""\tfor i := 0; i < dataVal.Len(); i++ {
\t\tcurrentData := dataVal.Index(i).Interface()
\t\tfor valSlice.Len() <= i {
\t\t\tvalSlice = reflect.Append(valSlice, reflect.Zero(valElemType))
\t\t}
\t\tcurrentField := valSlice.Index(i)

\t\tfieldName := name + "[" + strconv.Itoa(i) + "]"
\t\t_ = fieldName
\t\t// MUTATION: slice elements are set directly, never passing the hook.
\t\tif cd, ok := currentData.(reflect.Value); ok {
\t\t\tcurrentField.Set(cd)
\t\t} else {
\t\t\tcurrentField.Set(reflect.ValueOf(currentData))
\t\t}""",
        expectation="kill",
    ),
    Mutation(
        mid="M07-skip-struct-field-channel",
        description="Skip the per-field decode() recursion inside decodeStructFromMap.",
        old="""\t\tif err := d.decode(fieldName, rawMapVal.Interface(), fieldValue); err != nil {
\t\t\terrs = append(errs, err)
\t\t}""",
        new="""\t\t// MUTATION: struct fields are set directly, never passing the hook.
\t\tif rmv, ok := rawMapVal.Interface().(reflect.Value); ok {
\t\t\tfieldValue.Set(rmv)
\t\t} else {
\t\t\tfieldValue.Set(reflect.ValueOf(rawMapVal.Interface()))
\t\t}
\t\t_ = fieldName""",
        expectation="kill",
    ),
    Mutation(
        mid="M08-skip-pointer-level",
        description=(
            "Bypass the hook when the destination is a pointer. The pointer node's "
            "hook event is produced at central dispatch by the parent's decode() "
            "call, so the gate has to key on the target kind rather than decodePtr."
        ),
        old=DISPATCH_OLD,
        new=dispatch_with_guard(
            "d.cachedDecodeHook != nil && outputKind != reflect.Ptr"
        ),
        expectation="kill",
    ),
    Mutation(
        mid="M09-close-decode-nil-gate",
        description="Never invoke the hook for nil nodes even when DecodeNil is enabled.",
        old="""\t\tif !decodeNil {
\t\t\treturn nil
\t\t}
\t}
\tif !inputVal.IsValid() {
\t\tif !decodeNil {""",
        new="""\t\tif !decodeNil {
\t\t\treturn nil
\t\t}
\t\treturn nil // MUTATION: DecodeNil gate forced closed.
\t}
\tif !inputVal.IsValid() {
\t\tif !decodeNil {""",
        expectation="kill",
    ),
    Mutation(
        mid="M10-duplicate-root-hook",
        description="Invoke the hook twice at the root (name == \"\") node.",
        old=DISPATCH_OLD,
        new=DISPATCH_OLD.replace(
            """\t\tif err != nil {
\t\t\treturn newDecodeError(name, err)
\t\t}
\t}""",
            """\t\tif err != nil {
\t\t\treturn newDecodeError(name, err)
\t\t}
\t\tif name == "" {
\t\t\t// MUTATION: root container hooks a second time.
\t\t\tif _, err = d.cachedDecodeHook(reflect.ValueOf(input), outVal); err != nil {
\t\t\t\treturn newDecodeError(name, err)
\t\t\t}
\t\t}
\t}""",
            1,
        ),
        expectation="kill",
    ),
    Mutation(
        mid="C01-invert-weak-bool-conversion",
        description=(
            "Negative control: flip a weak int->bool conversion result. It changes "
            "decoding behaviour but never adds, removes or reorders a hook "
            "invocation, and the hierarchy fixtures have no bool target. Its "
            "survival proves the suite is specialized to the hook contract rather "
            "than incidentally failing on arbitrary behaviour changes."
        ),
        old="""\tcase dataKind == reflect.Int && d.config.WeaklyTypedInput:
\t\tval.SetBool(dataVal.Int() != 0)""",
        new="""\tcase dataKind == reflect.Int && d.config.WeaklyTypedInput:
\t\t// MUTATION: invert the weak int->bool result; the hook walk is untouched.
\t\tval.SetBool(dataVal.Int() == 0)""",
        expectation="survive",
    ),
)


def normalize(text: str) -> str:
    """Normalize CRLF/CR to LF so exact matches are line-ending independent."""
    return text.replace("\r\n", "\n").replace("\r", "\n")


def apply_mutation(source: str, mutation: Mutation) -> str:
    source = normalize(source)
    old = normalize(mutation.old)
    count = source.count(old)
    if count != 1:
        raise RuntimeError(
            f"{mutation.mid}: expected exactly 1 anchor, found {count}"
        )
    return source.replace(old, normalize(mutation.new), 1)


def run(cmd: list[str], cwd: Path) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        cmd,
        cwd=str(cwd),
        capture_output=True,
        text=True,
        timeout=300,
    )


def evaluate(repo: Path, go_bin: str, workroot: Path) -> list[dict[str, str]]:
    results: list[dict[str, str]] = []
    for mutation in MUTATIONS:
        # Keep each mutated tree in an explicit, per-mutation directory instead
        # of TemporaryDirectory: on Windows the freshly compiled test binary can
        # still be memory-mapped when the context exits, making cleanup fail
        # with a sharing violation even after the test process has returned.
        tree = workroot / mutation.mid
        if tree.exists():
            shutil.rmtree(tree, ignore_errors=True)
        shutil.copytree(repo, tree, ignore=shutil.ignore_patterns(".git"))

        target = tree / TARGET_FILE
        target.write_text(
            apply_mutation(target.read_text(encoding="utf-8"), mutation),
            encoding="utf-8",
        )

        targeted = run(
            [go_bin, "test", "-run", TARGET_TEST, "./..."], tree
        )
        killed = targeted.returncode != 0

        verdict = "KILLED" if killed else "SURVIVED"
        satisfied = (
            (mutation.expectation == "kill" and killed)
            or (mutation.expectation == "survive" and not killed)
        )
        results.append(
            {
                "id": mutation.mid,
                "expectation": mutation.expectation,
                "verdict": verdict,
                "satisfied": "yes" if satisfied else "NO",
                "description": mutation.description,
                "tail": targeted.stdout[-400:] + targeted.stderr[-400:],
            }
        )
    return results


def render(results: list[dict[str, str]]) -> str:
    lines = [
        "DecodeHook hierarchy mutation report",
        "====================================",
        f"target suite: go test -run {TARGET_TEST} ./...",
        "",
        f"{'MUTATION':<32}{'EXPECT':<9}{'VERDICT':<10}SATISFIED",
    ]
    for r in results:
        lines.append(
            f"{r['id']:<32}{r['expectation']:<9}{r['verdict']:<10}{r['satisfied']}"
        )
    killed = sum(1 for r in results if r["verdict"] == "KILLED")
    expected_kills = sum(1 for r in results if r["expectation"] == "kill")
    controls = sum(1 for r in results if r["expectation"] == "survive")
    surviving_controls = sum(
        1
        for r in results
        if r["expectation"] == "survive" and r["verdict"] == "SURVIVED"
    )
    lines += [
        "",
        f"hook-contract mutations killed: {killed}/{expected_kills}",
        f"negative controls survived:     {surviving_controls}/{controls}",
    ]
    unsatisfied = [r["id"] for r in results if r["satisfied"] == "NO"]
    lines.append(
        "overall: "
        + ("PASS - contract defended" if not unsatisfied else f"FAIL - {unsatisfied}")
    )
    lines.append("")
    lines.append("Mutation details:")
    for r in results:
        lines.append(f"- {r['id']} ({r['verdict']}): {r['description']}")
        if r["satisfied"] == "NO" and r.get("tail"):
            lines.append("  test output tail:")
            for ln in r["tail"].strip().splitlines()[-8:]:
                lines.append(f"    {ln}")
    return "\n".join(lines) + "\n"


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--repo", type=Path, required=True)
    parser.add_argument("--go", default="go")
    parser.add_argument(
        "--report",
        type=Path,
        default=Path(__file__).resolve().parent / "mutation-report.txt",
    )
    args = parser.parse_args()

    if not (args.repo / TARGET_FILE).is_file():
        print(f"repo path does not contain {TARGET_FILE}: {args.repo}", file=sys.stderr)
        return 2

    # ignore_cleanup_errors only matters on Windows, where a just-finished go
    # test may still hold the compiled test binary open for a beat.
    with tempfile.TemporaryDirectory(prefix="hh-mutation-", ignore_cleanup_errors=True) as work:
        results = evaluate(args.repo.resolve(), args.go, Path(work))
    report = render(results)
    args.report.write_text(report, encoding="utf-8")
    print(report)
    return 0 if all(r["satisfied"] == "yes" for r in results) else 1


if __name__ == "__main__":
    raise SystemExit(main())
