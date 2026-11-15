package mapstructure

// Regression tests for the DecodeHook execution-hierarchy contract.
//
// A DecodeHook is invoked from exactly one place: the pre-dispatch call in
// Decoder.decode (see mapstructure.go). Every decoded node therefore passes
// through the hook exactly once before its kind-specific decoder runs, and
// recursive decoders (struct, map, slice/array, pointer) produce a depth-first
// pre-order walk of the input. The tests below pin down, for a single Decode
// call, how many times and in which order the hook fires at every level:
//
//   - root container
//   - nested container (struct / map / slice)
//   - map keys (the key channel is a real hook invocation, distinct from the
//     value channel of the same map entry)
//   - leaf values
//   - slice containers and their individual elements
//
// They also pin the zero-value channel: by default a nil node returns before
// the hook runs, while DecodeNil flips that gate and the hook still fires with
// synthesized input.
//
// Mutation testing (see docs/hook-execution-hierarchy-contract.md) proves the
// suite actually kills changes that break the contract, e.g. skipping the map
// key channel or bypassing the hook for a container level.

import (
	"reflect"
	"testing"
)

// hhLevel classifies the structural role a hook invocation plays.
type hhLevel string

const (
	hhLevelRoot        hhLevel = "root"         // root container
	hhLevelContainer   hhLevel = "container"    // nested struct/map/slice container
	hhLevelMapKey      hhLevel = "map-key"      // key channel of a map entry
	hhLevelMapValue    hhLevel = "map-value"    // value channel of a map entry or slice element
	hhLevelStructField hhLevel = "struct-field" // non-container struct field value
)

// hhEvent is one observed DecodeHook invocation.
type hhEvent struct {
	seq      int
	level    hhLevel
	fromType string
	toType   string
	fromKind reflect.Kind
	toKind   reflect.Kind
	nilInput bool
	// synth is true when the input was materialized for a nil node under
	// DecodeNil rather than coming from the caller's input.
	synth bool
	data  any
}

// hhExpect is one expected position in a hierarchy trace.
type hhExpect struct {
	level hhLevel
	synth bool
}

// hhRecorder is a DecodeHookFuncType that records every invocation in order.
// It never transforms data, which keeps the decode result identical to a
// hook-less run while the call trace is fully observable.
type hhRecorder struct {
	t      *testing.T
	events []hhEvent
}

func newHHRecorder(t *testing.T) *hhRecorder {
	t.Helper()

	return &hhRecorder{t: t}
}

func (r *hhRecorder) hook(f, t reflect.Type, data any) (any, error) {
	ev := hhEvent{
		seq:      len(r.events) + 1,
		fromType: "<invalid>",
		toType:   t.String(),
		toKind:   t.Kind(),
		nilInput: data == nil,
		data:     data,
	}
	if f != nil {
		ev.fromType = f.String()
		ev.fromKind = f.Kind()
	}
	r.events = append(r.events, ev)

	return data, nil
}

func (r *hhRecorder) count() int {
	return len(r.events)
}

// levels projects the trace onto its ordered level labels.
func (r *hhRecorder) levels() []hhLevel {
	out := make([]hhLevel, len(r.events))
	for i, ev := range r.events {
		out[i] = ev.level
	}

	return out
}

// classify annotates each recorded event with its structural level. It uses
// the target type as the stable identity of a node: every level below is
// backed by a dedicated named test type, so classification cannot be
// ambiguous, including for DecodeNil-synthesized events whose target type is
// unchanged.
func (r *hhRecorder) classify() {
	for i := range r.events {
		ev := &r.events[i]
		switch {
		case ev.toType == "mapstructure.hhRoot":
			ev.level = hhLevelRoot
		case ev.toType == "mapstructure.hhNested" ||
			ev.toType == "mapstructure.hhCont" ||
			ev.toType == "[]mapstructure.hhCont" ||
			ev.toType == "[]mapstructure.hhLeaf" ||
			ev.toType == "map[mapstructure.hhKey]mapstructure.hhLeaf":
			ev.level = hhLevelContainer
		case ev.toType == "mapstructure.hhKey":
			ev.level = hhLevelMapKey
		case ev.toType == "mapstructure.hhLeaf":
			ev.level = hhLevelMapValue
		case ev.toType == "mapstructure.hhFieldLeaf":
			ev.level = hhLevelStructField
		default:
			r.t.Fatalf("unclassifiable hook event #%d: %+v", ev.seq, ev)
		}
		ev.synth = hhIsSynthesized(*ev)
	}
}

// hhIsSynthesized reports whether an event feeds on materialized nil input.
// The decoder synthesizes different shapes per kind:
//   - leaf: a zero value of the target type, so from and to types coincide;
//   - slice/array: a typed-nil []any;
//   - map/struct: a non-nil interface holding a typed-nil map[string]any;
//   - pointer/interface: a nil value.
//
// reflect.ValueOf on a typed-nil slice/map wrapped in an interface stays
// reportable via IsNil, which is why the container cases need IsNil rather
// than the event's nilInput flag.
func hhIsSynthesized(ev hhEvent) bool {
	if ev.nilInput {
		return true
	}
	if ev.fromKind == reflect.String && ev.fromType == ev.toType {
		return true
	}
	// Reference kinds: a synthesized container is a typed-nil slice/map (or
	// nil pointer/interface); real input at these positions has a value. The
	// nil-root case feeds a typed-nil map into a struct target, so the IsNil
	// probe has to run on the input regardless of the destination kind.
	if v := reflect.ValueOf(ev.data); v.IsValid() {
		switch v.Kind() {
		case reflect.Slice, reflect.Map, reflect.Ptr, reflect.Interface:
			return v.IsNil()
		}
	}

	return false
}

// hhKey and hhLeaf give map keys and map/struct leaf values distinct target
// identities, letting the tests prove the key channel is hooked separately.
type hhKey string

type hhLeaf string

type hhFieldLeaf string

type hhNested struct {
	Leaf hhLeaf `mapstructure:"leaf"`
}

type hhCont struct {
	Leaf hhLeaf `mapstructure:"leaf"`
}

// hhInner and hhOuter add a pointer level so the hierarchy tests cover the
// pointer dispatch path: a non-nil pointer field hooks once for the pointer
// node and again for the pointed-to element.
type hhInner struct {
	Leaf hhLeaf `mapstructure:"leaf"`
}

type hhOuter struct {
	Inner *hhInner `mapstructure:"inner"`
}

// hhStructSrc is a plain struct input. Decoding a struct into a struct routes
// a struct-kind value through the central hook dispatch (via the
// struct-to-map intermediary), which a map-only fixture could never exercise.
type hhStructSrc struct {
	Leaf string `mapstructure:"leaf"`
}

type hhRoot struct {
	Title   hhFieldLeaf      `mapstructure:"title"`
	Nested  hhNested         `mapstructure:"nested"`
	Items   []hhLeaf         `mapstructure:"items"`
	Conts   []hhCont         `mapstructure:"conts"`
	Mapping map[hhKey]hhLeaf `mapstructure:"mapping"`
}

// hhFullInput exercises every level in a single Decode call. The map literals
// hold a single entry on purpose: map iteration order is randomized in Go, so
// ordering assertions are only sound for one-entry maps; count-only coverage
// of multi-entry maps lives in TestDecodeHookHierarchy_MultiEntryMap.
func hhFullInput() map[string]any {
	return map[string]any{
		"title":  "root-title",
		"nested": map[string]any{"leaf": "inner-leaf"},
		"items":  []any{"i1", "i2"},
		"conts": []any{
			map[string]any{"leaf": "c0-leaf"},
		},
		"mapping": map[string]any{"k1": "v1"},
	}
}

// TestDecodeHookHierarchy_FullWalk pins the complete pre-order trigger
// sequence and per-level trigger counts for one decode spanning root, nested
// container, map keys, leaf values, slice container and slice elements.
func TestDecodeHookHierarchy_FullWalk(t *testing.T) {
	t.Parallel()

	rec := newHHRecorder(t)
	var result hhRoot
	dec, err := NewDecoder(&DecoderConfig{
		DecodeHook: rec.hook,
		Result:     &result,
	})
	if err != nil {
		t.Fatalf("NewDecoder error: %s", err)
	}
	if err := dec.Decode(hhFullInput()); err != nil {
		t.Fatalf("Decode error: %s", err)
	}

	rec.classify()

	wantLevels := []hhLevel{
		hhLevelRoot,        // 1: root struct container, before any field
		hhLevelStructField, // 2: title leaf
		hhLevelContainer,   // 3: nested struct container
		hhLevelMapValue,    // 4: nested.leaf leaf
		hhLevelContainer,   // 5: items slice container
		hhLevelMapValue,    // 6: items[0] element
		hhLevelMapValue,    // 7: items[1] element
		hhLevelContainer,   // 8: conts slice container
		hhLevelContainer,   // 9: conts[0] struct element
		hhLevelMapValue,    // 10: conts[0].leaf
		hhLevelContainer,   // 11: mapping map container
		hhLevelMapKey,      // 12: mapping key channel
		hhLevelMapValue,    // 13: mapping value channel
	}
	if got := rec.levels(); !hhLevelsEqual(got, wantLevels) {
		t.Fatalf("hook trigger order mismatch:\n got: %v\nwant: %v", got, wantLevels)
	}

	wantCounts := map[hhLevel]int{
		hhLevelRoot:        1,
		hhLevelContainer:   5, // nested struct, 2 slice containers, slice-element struct, map
		hhLevelStructField: 1,
		hhLevelMapKey:      1,
		hhLevelMapValue:    5, // nested.leaf, 2 items, conts[0].leaf, mapping value
	}
	for level, want := range wantCounts {
		if got := hhCountLevel(rec.events, level); got != want {
			t.Errorf("level %q triggered %d time(s), want %d", level, got, want)
		}
	}

	// Every node passes through the hook exactly once: 13 nodes, 13 events.
	if rec.count() != 13 {
		t.Fatalf("total hook invocations = %d, want 13", rec.count())
	}

	// The key channel must observe the raw key and the immediately following
	// value channel of the same entry the decoded value, proving keys are
	// hooked independently and in key-before-value order.
	keyIdx := hhIndexOf(rec.events, hhLevelMapKey)
	if keyIdx < 0 {
		t.Fatal("map-key event missing")
	}
	if rec.events[keyIdx].data != "k1" {
		t.Errorf("map-key channel saw %#v, want %q", rec.events[keyIdx].data, "k1")
	}
	valEv := rec.events[keyIdx+1]
	if valEv.level != hhLevelMapValue || valEv.data != "v1" {
		t.Errorf("event after map key = %+v, want map-value %q", valEv, "v1")
	}

	// Slice elements must fire in index order, right after the slice container.
	if rec.events[4].toKind != reflect.Slice || rec.events[5].data != "i1" || rec.events[6].data != "i2" {
		t.Errorf("slice channel/element order broken: %+v, %+v, %+v",
			rec.events[4], rec.events[5], rec.events[6])
	}
}

// TestDecodeHookHierarchy_MultiEntryMap pins counts for a map with several
// entries. Order across entries is unspecified by Go, so this asserts the
// per-entry key-before-value invariant and the aggregate counts instead of a
// full sequence.
func TestDecodeHookHierarchy_MultiEntryMap(t *testing.T) {
	t.Parallel()

	rec := newHHRecorder(t)
	var result map[hhKey]hhLeaf
	dec, err := NewDecoder(&DecoderConfig{
		DecodeHook: rec.hook,
		Result:     &result,
	})
	if err != nil {
		t.Fatalf("NewDecoder error: %s", err)
	}
	input := map[string]any{"a": "1", "b": "2", "c": "3"}
	if err := dec.Decode(input); err != nil {
		t.Fatalf("Decode error: %s", err)
	}

	// 1 map container + 3 key channels + 3 value channels.
	if rec.count() != 7 {
		t.Fatalf("total hook invocations = %d, want 7 (1 container + 3 keys + 3 values)", rec.count())
	}
	if got := hhCountByType(rec.events, "mapstructure.hhKey"); got != 3 {
		t.Errorf("map-key channel fired %d time(s), want 3", got)
	}
	if got := hhCountByType(rec.events, "mapstructure.hhLeaf"); got != 3 {
		t.Errorf("map-value channel fired %d time(s), want 3", got)
	}

	// For each entry the key event immediately precedes the value event.
	seen := map[string]string{}
	for i := 1; i < len(rec.events); i++ {
		if rec.events[i].toType != "mapstructure.hhKey" {
			continue
		}
		key, _ := rec.events[i].data.(string)
		next := rec.events[i+1]
		if next.toType != "mapstructure.hhLeaf" {
			t.Fatalf("key event %q not immediately followed by its value event, got %s", key, next.toType)
		}
		seen[key] = next.data.(string)
	}
	if len(seen) != 3 {
		t.Fatalf("observed %d key/value pairs, want 3: %#v", len(seen), seen)
	}
	for k, v := range map[string]string{"a": "1", "b": "2", "c": "3"} {
		if seen[k] != v {
			t.Errorf("entry %q mapped to %q, want %q", k, seen[k], v)
		}
	}
}

// TestDecodeHookHierarchy_ContainerPreOrder proves a container node itself is
// hooked before any of its descendants. A mutation that skips the container
// level and only hooks leaves removes the leading container event here.
func TestDecodeHookHierarchy_ContainerPreOrder(t *testing.T) {
	t.Parallel()

	t.Run("nested-struct", func(t *testing.T) {
		t.Parallel()

		rec := newHHRecorder(t)
		var result hhNested
		dec, err := NewDecoder(&DecoderConfig{DecodeHook: rec.hook, Result: &result})
		if err != nil {
			t.Fatalf("NewDecoder error: %s", err)
		}
		if err := dec.Decode(map[string]any{"leaf": "x"}); err != nil {
			t.Fatalf("Decode error: %s", err)
		}
		if rec.count() != 2 {
			t.Fatalf("invocations = %d, want 2 (container then leaf)", rec.count())
		}
		if rec.events[0].toType != "mapstructure.hhNested" {
			t.Errorf("first event target = %q, want nested container", rec.events[0].toType)
		}
		if rec.events[1].toType != "mapstructure.hhLeaf" {
			t.Errorf("second event target = %q, want leaf", rec.events[1].toType)
		}
	})

	t.Run("slice", func(t *testing.T) {
		t.Parallel()

		rec := newHHRecorder(t)
		var result []hhLeaf
		dec, err := NewDecoder(&DecoderConfig{DecodeHook: rec.hook, Result: &result})
		if err != nil {
			t.Fatalf("NewDecoder error: %s", err)
		}
		if err := dec.Decode([]any{"a", "b"}); err != nil {
			t.Fatalf("Decode error: %s", err)
		}
		if rec.count() != 3 {
			t.Fatalf("invocations = %d, want 3 (slice container then 2 elements)", rec.count())
		}
		if rec.events[0].toKind != reflect.Slice {
			t.Errorf("first event target kind = %s, want slice", rec.events[0].toKind)
		}
		if rec.events[1].data != "a" || rec.events[2].data != "b" {
			t.Errorf("element order = %#v, %#v, want a, b", rec.events[1].data, rec.events[2].data)
		}
	})

	t.Run("map", func(t *testing.T) {
		t.Parallel()

		rec := newHHRecorder(t)
		var result map[hhKey]hhLeaf
		dec, err := NewDecoder(&DecoderConfig{DecodeHook: rec.hook, Result: &result})
		if err != nil {
			t.Fatalf("NewDecoder error: %s", err)
		}
		if err := dec.Decode(map[string]any{"k": "v"}); err != nil {
			t.Fatalf("Decode error: %s", err)
		}
		if rec.count() != 3 {
			t.Fatalf("invocations = %d, want 3 (map container, key, value)", rec.count())
		}
		if rec.events[0].toKind != reflect.Map {
			t.Errorf("first event target kind = %s, want map", rec.events[0].toKind)
		}
	})
}

// TestDecodeHookHierarchy_StructKindInput proves the hook fires for a
// struct-kind node at the central dispatch, not only for map-kind roots. A
// mutation that skips the hook when inputVal.Kind() == Struct removes the
// leading event here.
func TestDecodeHookHierarchy_StructKindInput(t *testing.T) {
	t.Parallel()

	rec := newHHRecorder(t)
	var result hhNested
	dec, err := NewDecoder(&DecoderConfig{DecodeHook: rec.hook, Result: &result})
	if err != nil {
		t.Fatalf("NewDecoder error: %s", err)
	}
	// Struct in -> struct out. The struct-to-map intermediary still routes the
	// original struct node through the hook first.
	if err := dec.Decode(hhStructSrc{Leaf: "s"}); err != nil {
		t.Fatalf("Decode error: %s", err)
	}
	if rec.count() < 2 {
		t.Fatalf("invocations = %d, want at least struct node then leaf", rec.count())
	}
	if rec.events[0].fromKind != reflect.Struct {
		t.Errorf("first event from-kind = %s, want struct", rec.events[0].fromKind)
	}
	if rec.events[0].toType != "mapstructure.hhNested" {
		t.Errorf("first event target = %q, want hhNested container", rec.events[0].toType)
	}
}

// TestDecodeHookHierarchy_PointerLevel proves a non-nil pointer node passes
// through the hook at its own level, followed by a separate invocation for the
// pointed-to element. A mutation that strips the hook on the pointer dispatch
// collapses the pair into a single element-level event.
func TestDecodeHookHierarchy_PointerLevel(t *testing.T) {
	t.Parallel()

	rec := newHHRecorder(t)
	var result hhOuter
	dec, err := NewDecoder(&DecoderConfig{DecodeHook: rec.hook, Result: &result})
	if err != nil {
		t.Fatalf("NewDecoder error: %s", err)
	}
	if err := dec.Decode(map[string]any{"inner": map[string]any{"leaf": "p"}}); err != nil {
		t.Fatalf("Decode error: %s", err)
	}
	if result.Inner == nil {
		t.Fatal("pointer field was not populated")
	}

	// Root struct, then pointer node, then pointed-to struct, then leaf.
	var ptrEvents, elemEvents int
	for _, ev := range rec.events {
		if ev.toType == "*mapstructure.hhInner" {
			ptrEvents++
		}
		if ev.toType == "mapstructure.hhInner" {
			elemEvents++
		}
	}
	if ptrEvents != 1 {
		t.Errorf("pointer-level hook fired %d time(s), want 1", ptrEvents)
	}
	if elemEvents != 1 {
		t.Errorf("pointed-to element hook fired %d time(s), want 1", elemEvents)
	}

	// The pointer node must be visited before its element.
	ptrIdx, elemIdx := hhIndexOfType(rec.events, "*mapstructure.hhInner"),
		hhIndexOfType(rec.events, "mapstructure.hhInner")
	if ptrIdx < 0 || elemIdx < 0 || ptrIdx > elemIdx {
		t.Errorf("pointer node (idx %d) must precede element (idx %d)", ptrIdx, elemIdx)
	}
}

// hhZeroCase is one row of the zero-value channel matrix.
type hhZeroCase struct {
	name      string
	decodeNil bool
	input     map[string]any
	wantTrace []hhExpect
}

// TestDecodeHookHierarchy_ZeroChannel pins how nil nodes interact with the
// hook gate. Without DecodeNil a nil node returns before the hook (no call at
// that node); with DecodeNil the gate stays open and the hook fires once with
// synthesized input at the same structural position.
func TestDecodeHookHierarchy_ZeroChannel(t *testing.T) {
	t.Parallel()

	cases := []hhZeroCase{
		{
			name:      "nil-fields/default-gate-closed",
			decodeNil: false,
			input: map[string]any{
				"nested":  nil,
				"title":   nil,
				"items":   nil,
				"mapping": nil,
			},
			// Only the non-nil root container is hooked; every nil field bails
			// out before reaching the hook.
			wantTrace: []hhExpect{{level: hhLevelRoot}},
		},
		{
			name:      "nil-fields/DecodeNil-gate-open",
			decodeNil: true,
			input: map[string]any{
				"nested":  nil,
				"title":   nil,
				"items":   nil,
				"mapping": nil,
			},
			// Root plus one synthesized event per nil field, in struct field
			// declaration order.
			wantTrace: []hhExpect{
				{level: hhLevelRoot},
				{level: hhLevelStructField, synth: true}, // title leaf
				{level: hhLevelContainer, synth: true},   // nested struct
				{level: hhLevelContainer, synth: true},   // items slice
				{level: hhLevelContainer, synth: true},   // mapping map
			},
		},
		{
			name:      "nil-inside-collections/default-gate-closed",
			decodeNil: false,
			input: map[string]any{
				"nested":  map[string]any{"leaf": nil},
				"title":   "",
				"items":   []any{nil, "x"},
				"mapping": map[string]any{"k": nil},
			},
			// Containers and non-nil nodes still hook; the nil leaf, nil slice
			// element and nil map value do not. The map key "k" is non-nil and
			// its key channel still fires.
			wantTrace: []hhExpect{
				{level: hhLevelRoot},
				{level: hhLevelStructField}, // title ""
				{level: hhLevelContainer},   // nested struct; nested.leaf nil -> no event
				{level: hhLevelContainer},   // items slice; items[0] nil -> no event
				{level: hhLevelMapValue},    // items[1] "x"
				{level: hhLevelContainer},   // mapping map
				{level: hhLevelMapKey},      // key "k"; its nil value -> no event
			},
		},
		{
			name:      "nil-inside-collections/DecodeNil-gate-open",
			decodeNil: true,
			input: map[string]any{
				"nested":  map[string]any{"leaf": nil},
				"title":   "",
				"items":   []any{nil, "x"},
				"mapping": map[string]any{"k": nil},
			},
			// Same walk, but every nil node now produces a synthesized event at
			// its own level.
			wantTrace: []hhExpect{
				{level: hhLevelRoot},
				{level: hhLevelStructField},           // title "" (real value)
				{level: hhLevelContainer},             // nested struct
				{level: hhLevelMapValue, synth: true}, // nested.leaf nil
				{level: hhLevelContainer},             // items slice
				{level: hhLevelMapValue, synth: true}, // items[0] nil
				{level: hhLevelMapValue},              // items[1] "x"
				{level: hhLevelContainer},             // mapping map
				{level: hhLevelMapKey},                // key "k"
				{level: hhLevelMapValue, synth: true}, // mapping["k"] nil value
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := newHHRecorder(t)
			var result hhRoot
			dec, err := NewDecoder(&DecoderConfig{
				DecodeHook: rec.hook,
				DecodeNil:  tc.decodeNil,
				Result:     &result,
			})
			if err != nil {
				t.Fatalf("NewDecoder error: %s", err)
			}
			if err := dec.Decode(tc.input); err != nil {
				t.Fatalf("Decode error: %s", err)
			}

			rec.classify()

			if rec.count() != len(tc.wantTrace) {
				t.Errorf("total invocations = %d, want %d", rec.count(), len(tc.wantTrace))
			}
			for i, want := range tc.wantTrace {
				if i >= len(rec.events) {
					t.Errorf("missing event at position %d: want %+v", i, want)

					continue
				}
				got := rec.events[i]
				if got.level != want.level || got.synth != want.synth {
					t.Errorf("position %d = (level=%s synth=%v), want (level=%s synth=%v)",
						i, got.level, got.synth, want.level, want.synth)
				}
			}
		})
	}
}

// TestDecodeHookHierarchy_NilRoot covers the extreme zero-value case: a nil
// top-level input. With the gate closed no hook may run at all; with DecodeNil
// the root fires exactly once with a synthesized value.
func TestDecodeHookHierarchy_NilRoot(t *testing.T) {
	t.Parallel()

	t.Run("default-gate-closed", func(t *testing.T) {
		t.Parallel()

		rec := newHHRecorder(t)
		var result hhRoot
		dec, err := NewDecoder(&DecoderConfig{DecodeHook: rec.hook, Result: &result})
		if err != nil {
			t.Fatalf("NewDecoder error: %s", err)
		}
		if err := dec.Decode(nil); err != nil {
			t.Fatalf("Decode error: %s", err)
		}
		if rec.count() != 0 {
			t.Fatalf("nil root fired the hook %d time(s), want 0", rec.count())
		}
	})

	t.Run("DecodeNil-gate-open", func(t *testing.T) {
		t.Parallel()

		rec := newHHRecorder(t)
		var result hhRoot
		dec, err := NewDecoder(&DecoderConfig{
			DecodeHook: rec.hook,
			DecodeNil:  true,
			Result:     &result,
		})
		if err != nil {
			t.Fatalf("NewDecoder error: %s", err)
		}
		if err := dec.Decode(nil); err != nil {
			t.Fatalf("Decode error: %s", err)
		}
		rec.classify()
		if rec.count() != 1 {
			t.Fatalf("nil root fired the hook %d time(s), want 1", rec.count())
		}
		if rec.events[0].toType != "mapstructure.hhRoot" || !rec.events[0].synth {
			t.Errorf("root event = %+v, want synthesized hhRoot", rec.events[0])
		}
	})
}

// TestDecodeHookHierarchy_StatefulMaskingHook models the production failure
// that motivated this suite: a stateful data-masking hook whose state machine
// is only well-defined if (a) map keys are distinguishable from values and
// (b) every node passes through the hook exactly once. It asserts the hook can
// mask leaf values while leaving keys untouched, and that its per-node
// transition bookkeeping matches the recorded hierarchy.
func TestDecodeHookHierarchy_StatefulMaskingHook(t *testing.T) {
	t.Parallel()

	type maskEvent struct {
		toType string
		masked bool
	}

	var transitions []maskEvent
	maskingHook := func(f, t reflect.Type, data any) (any, error) {
		// The key channel targets the key type: never mask keys.
		if t == reflect.TypeOf(hhKey("")) {
			transitions = append(transitions, maskEvent{t.String(), false})
			return data, nil
		}
		// Only string-ish leaf values are masked; containers pass through.
		if t.Kind() == reflect.String {
			if s, ok := data.(string); ok && s != "" {
				transitions = append(transitions, maskEvent{t.String(), true})
				return "***", nil
			}
		}
		transitions = append(transitions, maskEvent{t.String(), false})

		return data, nil
	}

	var result hhRoot
	dec, err := NewDecoder(&DecoderConfig{DecodeHook: maskingHook, Result: &result})
	if err != nil {
		t.Fatalf("NewDecoder error: %s", err)
	}
	if err := dec.Decode(hhFullInput()); err != nil {
		t.Fatalf("Decode error: %s", err)
	}

	// One state-machine transition per node, no node visited twice.
	if len(transitions) != 13 {
		t.Fatalf("stateful hook saw %d transitions, want 13", len(transitions))
	}
	// The key transition must be an unmasked one.
	var keyTransitions int
	for _, tr := range transitions {
		if tr.toType == "mapstructure.hhKey" {
			keyTransitions++
			if tr.masked {
				t.Error("map key was masked, keys must pass through unchanged")
			}
		}
	}
	if keyTransitions != 1 {
		t.Fatalf("key channel transitions = %d, want exactly 1", keyTransitions)
	}

	// End-to-end masking result: every non-empty leaf redacted, key intact.
	if result.Title != "***" || result.Nested.Leaf != "***" {
		t.Errorf("struct leaves not masked: %+v", result)
	}
	if result.Items[0] != "***" || result.Items[1] != "***" {
		t.Errorf("slice leaves not masked: %+v", result.Items)
	}
	if _, ok := result.Mapping["k1"]; !ok {
		t.Errorf("map key k1 missing after decode: %+v", result.Mapping)
	}
	if result.Mapping["k1"] != "***" {
		t.Errorf("map value not masked: %+v", result.Mapping)
	}
}

// TestDecodeHookHierarchy_RepeatedDecodesAreIndependent guards another
// stateful-hook concern: hierarchy counts must be identical across separate
// Decode calls on the same decoder, with no invocation leaking between runs.
func TestDecodeHookHierarchy_RepeatedDecodesAreIndependent(t *testing.T) {
	t.Parallel()

	rec := newHHRecorder(t)
	var result hhRoot
	dec, err := NewDecoder(&DecoderConfig{DecodeHook: rec.hook, Result: &result})
	if err != nil {
		t.Fatalf("NewDecoder error: %s", err)
	}
	for i := 0; i < 3; i++ {
		before := rec.count()
		if err := dec.Decode(hhFullInput()); err != nil {
			t.Fatalf("Decode #%d error: %s", i, err)
		}
		if got := rec.count() - before; got != 13 {
			t.Fatalf("Decode #%d produced %d hook events, want 13", i, got)
		}
	}
}

func hhLevelsEqual(a, b []hhLevel) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

func hhCountLevel(events []hhEvent, level hhLevel) int {
	n := 0
	for _, ev := range events {
		if ev.level == level {
			n++
		}
	}

	return n
}

func hhCountByType(events []hhEvent, toType string) int {
	n := 0
	for _, ev := range events {
		if ev.toType == toType {
			n++
		}
	}

	return n
}

func hhIndexOf(events []hhEvent, level hhLevel) int {
	for i, ev := range events {
		if ev.level == level {
			return i
		}
	}

	return -1
}

func hhIndexOfType(events []hhEvent, toType string) int {
	for i, ev := range events {
		if ev.toType == toType {
			return i
		}
	}

	return -1
}
