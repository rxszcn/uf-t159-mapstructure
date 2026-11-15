# DecodeHook Execution-Hierarchy Contract

This document pins down **when, how many times, and in which order** a
`DecoderConfig.DecodeHook` is invoked during a single `Decode` call, across
every level of a nested value. The behaviour is enforced by the regression
suite `decode_hook_hierarchy_test.go` and protected against silent change by
the mutation harness in `tools/mutationtest/`.

## Why this needs a contract

A stateful hook (for example a data-masking hook that drives a small state
machine) is only correct if two things hold for every node it visits:

1. it can tell which structural role a node plays — container, map key, map
   value, slice element, leaf — and
2. each node passes through it exactly once per decode.

Before this contract existed neither property was documented or tested. A
refactor that skipped the map-key channel, or stopped hooking a container
level, would ship silently and corrupt the hook's state. The tests below make
any such change fail.

## The single dispatch point

There is exactly one place that calls a hook: the pre-dispatch block in
`Decoder.decode` (`mapstructure.go`).

```go
if d.cachedDecodeHook != nil {
    input, err = d.cachedDecodeHook(inputVal, outVal)
    if err != nil {
        return newDecodeError(name, err)
    }
}
```

Every kind-specific decoder (`decodeStructFromMap`, `decodeMapFromMap`,
`decodeSlice`, `decodeArray`, `decodePtr`, ...) reaches the hook by recursing
through `decode`. The direct consequences are the contract:

- **One invocation per decoded node.** The hook runs once for a node before
  its kind-specific decoder runs, and again (recursively) for each child node.
- **Depth-first, pre-order walk.** A container is always visited before any of
  its descendants; children are visited in traversal order.
- **Map entries have two independent channels.** Each entry produces one
  invocation for the **key** and a separate one for the **value**, in that
  order, even when key and value share an underlying Go kind.
- **The hook sees the raw node.** A non-transforming hook can observe every
  source value unchanged, including map keys and slice elements.

## The reference walk

The fixture `hhRoot` (`TestDecodeHookHierarchy_FullWalk`) contains one of each
level. A single `Decode` of `hhFullInput()` produces exactly these 13
invocations, in this order:

| # | Level             | Source kind | Target                          | Node |
|---|-------------------|-------------|---------------------------------|------|
| 1 | root container    | map         | `hhRoot`                        | whole input |
| 2 | struct field leaf | string      | `hhFieldLeaf`                   | `title` |
| 3 | nested container  | map         | `hhNested`                      | `nested` |
| 4 | leaf value        | string      | `hhLeaf`                        | `nested.leaf` |
| 5 | slice container   | slice       | `[]hhLeaf`                      | `items` |
| 6 | slice element     | string      | `hhLeaf`                        | `items[0]` |
| 7 | slice element     | string      | `hhLeaf`                        | `items[1]` |
| 8 | slice container   | slice       | `[]hhCont`                      | `conts` |
| 9 | slice element     | map         | `hhCont`                        | `conts[0]` (nested container) |
| 10 | leaf value       | string      | `hhLeaf`                        | `conts[0].leaf` |
| 11 | map container     | map         | `map[hhKey]hhLeaf`              | `mapping` |
| 12 | **map key**       | string      | `hhKey`                         | `mapping` key `k1` |
| 13 | **map value**     | string      | `hhLeaf`                        | `mapping` value `v1` |

Per-level counts: root `1`, containers `5`, struct-field leaves `1`, map keys
`1`, other leaf/map/slice-element values `5`; **13 total**.

Slice elements are visited in ascending index order. For multi-entry maps the
cross-entry order follows Go's randomized map iteration and is therefore not
asserted; instead `TestDecodeHookHierarchy_MultiEntryMap` pins the aggregate
counts and the per-entry key-before-value invariant.

Struct-kind inputs are covered separately: decoding a struct into a struct
(`TestDecodeHookHierarchy_StructKindInput`) still routes the struct node
through the central dispatch before the struct-to-map intermediary runs.

Pointer fields add one level
(`TestDecodeHookHierarchy_PointerLevel`): a non-nil pointer target fires once
for the pointer node (`*hhInner`) and again for the pointed-to element
(`hhInner`), pointer first.

## The zero-value channel

Two gates decide whether a `nil` node reaches the hook at all.

### Default gate (`DecodeNil == false`)

A `nil` input returns from `decode` **before** the hook block, so a nil node
produces zero hook invocations:

- a `nil` top-level input fires the hook **0** times;
- a `nil` struct field, slice, map or pointer fires **0** times (its parent
  container still fires normally);
- a `nil` slice element or `nil` map value is likewise skipped, while the
  non-nil map key next to it still goes through the key channel.

### Open gate (`DecodeNil == true`)

The early return is suppressed. The invalid/`nil` input is materialized into a
valid value and the node fires **once**, at the same structural position, with
a synthesized input whose shape depends on the target kind:

| Target kind   | Synthesized input seen by the hook |
|---------------|------------------------------------|
| leaf/basic    | zero value of the target type (source type equals target type) |
| slice/array   | typed-nil `[]any` |
| map/struct    | typed-nil `map[string]any` (a non-nil interface holding a nil map) |
| pointer/interface | `nil` |

A `nil` top-level input then fires exactly once for the root. The same rule
applies recursively, so a `nil` element inside a slice or a `nil` value inside
a map each produce one synthesized event at their own level once the gate is
open. The matrix is asserted in `TestDecodeHookHierarchy_ZeroChannel` and
`TestDecodeHookHierarchy_NilRoot`.

## Stateful-hook guarantees

`TestDecodeHookHierarchy_StatefulMaskingHook` models the motivating masking
hook and pins the guarantees its state machine relies on:

- exactly one transition per node (13 for the reference walk), never twice;
- the map-key channel is distinguishable by target type and is never masked;
- leaf values are redacted while keys pass through unchanged;
- repeated `Decode` calls on one decoder
  (`TestDecodeHookHierarchy_RepeatedDecodesAreIndependent`) each reproduce the
  identical count, with no invocation leaking between runs.

## Mutation verification

A test that cannot fail under a broken implementation does not protect
anything. The harness `tools/mutationtest/mutate_hook_hierarchy.py` applies one
exact source rewrite to a throwaway copy of the module and runs
`go test -run TestDecodeHookHierarchy ./...`.

Run it (Python 3, interpreter isolated from the working tree):

```sh
python -I tools/mutationtest/mutate_hook_hierarchy.py --repo <path-to-module>
```

Every hook-contract mutation must be **killed**; the one negative-control
mutation (which changes decoding behaviour without adding, removing or
reordering any hook call) must **survive**, proving the suite is specialized
to the hook contract rather than failing on arbitrary changes. The recorded
readings are in `tools/mutationtest/mutation-report.txt`.

| Mutation | Change | Expected | Observed |
|----------|--------|----------|----------|
| M01 | skip map-key `decode()` channel | killed | KILLED |
| M02 | skip map-value `decode()` channel | killed | KILLED |
| M03 | gate hook off for map-kind input | killed | KILLED |
| M04 | gate hook off for slice-kind input | killed | KILLED |
| M05 | gate hook off for struct-kind input | killed | KILLED |
| M06 | set slice elements without `decode()` | killed | KILLED |
| M07 | set struct fields without `decode()` | killed | KILLED |
| M08 | gate hook off for pointer targets | killed | KILLED |
| M09 | force the `DecodeNil` gate closed | killed | KILLED |
| M10 | invoke the root hook a second time | killed | KILLED |
| C01 | invert a weak int→bool conversion (no hook change) | survived | SURVIVED |

Result: **10/10 contract mutations killed, 1/1 negative control survived.**
C01 is a real behaviour change — the full, unfiltered suite catches it through
`TestDecode_TypeConversion` — which is what makes its survival against the
hierarchy-only suite a meaningful negative control rather than a no-op.

## What is intentionally not guaranteed

- Map **cross-entry** iteration order (Go randomizes it); only per-entry
  key-before-value order and aggregate counts are pinned.
- The internal shape of the struct-to-struct and pointer intermediaries,
  beyond the observable invocation levels documented above.
- Behaviour of an `Unmarshaler` target, which takes over decoding after the
  hook has run; this contract covers the hook walk itself.
