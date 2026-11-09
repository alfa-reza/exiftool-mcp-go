package query

import (
	"testing"

	"github.com/alfa-reza/exiftool-mcp-go/internal/meta"
)

func entries(pairs ...any) []meta.Entry {
	var out []meta.Entry
	for i := 0; i < len(pairs); i += 2 {
		key := pairs[i].(string)
		out = append(out, meta.EntryFromPair(key, pairs[i+1]))
	}
	return out
}

func pred(tag, op string, value any) *Node {
	return &Node{Type: "predicate", Tag: tag, Operator: op, Value: value}
}

func eval(t *testing.T, n *Node, tc *TimeContext, es []meta.Entry) Tri {
	t.Helper()
	p, err := Prepare(n, tc)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	got, _ := p.Eval(es)
	return got
}

func TestThreeValuedLogic(t *testing.T) {
	// unknown: numeric comparison against a non-numeric value
	unknown := pred("ISO", "gt", float64(100))
	es := entries("EXIF:ISO", "fast")
	if got := eval(t, unknown, nil, es); got != Indeterminate {
		t.Fatalf("expected indeterminate, got %v", got)
	}
	// false AND unknown = false
	q := &Node{Type: "all", Children: []*Node{pred("Make", "eq", "Nikon"), unknown}}
	if got := eval(t, q, nil, entries("EXIF:Make", "Canon", "EXIF:ISO", "fast")); got != NoMatch {
		t.Fatalf("false AND unknown should be no_match, got %v", got)
	}
	// true OR unknown = true
	q = &Node{Type: "any", Children: []*Node{pred("Make", "eq", "Canon"), unknown}}
	if got := eval(t, q, nil, entries("EXIF:Make", "Canon", "EXIF:ISO", "fast")); got != Match {
		t.Fatalf("true OR unknown should be match, got %v", got)
	}
	// NOT unknown = unknown
	q = &Node{Type: "not", Children: []*Node{unknown}}
	if got := eval(t, q, nil, es); got != Indeterminate {
		t.Fatalf("NOT unknown should be indeterminate, got %v", got)
	}
}

func TestExistsMissing(t *testing.T) {
	es := entries("EXIF:Make", "Canon")
	if got := eval(t, pred("GPSLatitude", "missing", nil), nil, es); got != Match {
		t.Fatalf("missing on absent tag should match, got %v", got)
	}
	if got := eval(t, pred("Make", "exists", nil), nil, es); got != Match {
		t.Fatalf("exists should match, got %v", got)
	}
	if got := eval(t, pred("Make", "missing", nil), nil, es); got != NoMatch {
		t.Fatalf("missing on present tag should be no_match, got %v", got)
	}
	// group-qualified selector
	if got := eval(t, pred("EXIF:Make", "eq", "Canon"), nil, es); got != Match {
		t.Fatalf("EXIF:Make should match group EXIF:IFD0 entry, got %v", got)
	}
	es2 := entries("EXIF:IFD0:Make", "Canon")
	if got := eval(t, pred("EXIF:Make", "eq", "Canon"), nil, es2); got != Match {
		t.Fatalf("EXIF:Make should match EXIF:IFD0 group, got %v", got)
	}
	if got := eval(t, pred("IFD0:Make", "eq", "Canon"), nil, es2); got != Match {
		t.Fatalf("IFD0:Make should match by family segment, got %v", got)
	}
}

func TestNumericAndTextOps(t *testing.T) {
	es := entries("EXIF:ISO", float64(3200), "EXIF:Make", "Canon EOS")
	if got := eval(t, pred("ISO", "gte", float64(3200)), nil, es); got != Match {
		t.Fatalf("gte numeric: %v", got)
	}
	if got := eval(t, pred("ISO", "between", []any{float64(100), float64(3200)}), nil, es); got != Match {
		t.Fatalf("between inclusive upper: %v", got)
	}
	if got := eval(t, pred("Make", "contains", "eos"), nil, es); got != NoMatch {
		t.Fatalf("contains is case-sensitive by default: %v", got)
	}
	n := pred("Make", "contains", "eos")
	n.CaseSensitive = boolPtr(false)
	if got := eval(t, n, nil, es); got != Match {
		t.Fatalf("contains case-insensitive: %v", got)
	}
	if got := eval(t, pred("Make", "regex", "^Canon"), nil, es); got != Match {
		t.Fatalf("regex: %v", got)
	}
	if got := eval(t, pred("ISO", "in", []any{float64(100), float64(3200)}), nil, es); got != Match {
		t.Fatalf("in: %v", got)
	}
	if got := eval(t, pred("ISO", "not_in", []any{float64(100)}), nil, es); got != Match {
		t.Fatalf("not_in: %v", got)
	}
	// unit-bearing string must not be guessed as a number
	es2 := entries("File:FileSize", "3.8 kB")
	if got := eval(t, pred("FileSize", "gt", float64(1)), nil, es2); got != Indeterminate {
		t.Fatalf("unit string numeric compare should be indeterminate, got %v", got)
	}
}

func boolPtr(b bool) *bool { return &b }

func TestDuplicateQuantifier(t *testing.T) {
	es := entries("EXIF:CreateDate", "2023:01:01 00:00:00", "XMP:CreateDate", "2019:01:01 00:00:00")
	any := pred("CreateDate", "after", "2020-01-01")
	any.ValueType = "datetime"
	if got := eval(t, any, nil, es); got != Match {
		t.Fatalf("duplicate any: %v", got)
	}
	all := pred("CreateDate", "after", "2020-01-01")
	all.ValueType = "datetime"
	all.DuplicateMatch = "all"
	if got := eval(t, all, nil, es); got != NoMatch {
		t.Fatalf("duplicate all should fail when one value fails: %v", got)
	}
	// 'all' with no values is NOT true
	if got := eval(t, all, nil, entries("EXIF:Make", "Canon")); got != NoMatch {
		t.Fatalf("duplicate all with no values must be no_match, got %v", got)
	}
}

func TestDatetimeSemantics(t *testing.T) {
	naive := entries("EXIF:DateTimeOriginal", "2023:05:01 10:00:00")
	// naive vs naive: wall-clock comparison
	n := pred("DateTimeOriginal", "between", []any{"2023-05-01", "2023-05-01"})
	n.ValueType = "datetime"
	if got := eval(t, n, nil, naive); got != Match {
		t.Fatalf("date-only between should cover the whole day: %v", got)
	}
	// naive vs aware without time context: indeterminate
	n2 := pred("DateTimeOriginal", "before", "2024-01-01T00:00:00+07:00")
	n2.ValueType = "datetime"
	if got := eval(t, n2, nil, naive); got != Indeterminate {
		t.Fatalf("mixed tz without context should be indeterminate: %v", got)
	}
	// with time context: comparable
	if got := eval(t, n2, &TimeContext{Timezone: "+07:00"}, naive); got != Match {
		t.Fatalf("mixed tz with context: %v", got)
	}
	// invalid date string: indeterminate, never a forced timestamp
	bad := entries("EXIF:DateTimeOriginal", "0000:00:00 00:00:00")
	n3 := pred("DateTimeOriginal", "after", "2020-01-01")
	n3.ValueType = "datetime"
	if got := eval(t, n3, nil, bad); got != Indeterminate {
		t.Fatalf("zero date should be indeterminate: %v", got)
	}
}

func TestSameDocument(t *testing.T) {
	// GPS position and timestamp in DIFFERENT embedded documents must
	// not satisfy a same_document group; in the same document they must.
	es := entries(
		"Doc1:GPS:GPSLatitude", float64(6.2),
		"Doc2:EXIF:DateTimeOriginal", "2023:05:01 10:00:00",
	)
	group := &Node{Type: "all", Scope: "same_document", Children: []*Node{
		pred("GPSLatitude", "exists", nil),
		pred("DateTimeOriginal", "exists", nil),
	}}
	if got := eval(t, group, nil, es); got != NoMatch {
		t.Fatalf("cross-document conditions must not match: %v", got)
	}
	es2 := entries(
		"Doc1:GPS:GPSLatitude", float64(6.2),
		"Doc1:EXIF:DateTimeOriginal", "2023:05:01 10:00:00",
	)
	if got := eval(t, group, nil, es2); got != Match {
		t.Fatalf("same-document conditions should match: %v", got)
	}
}

func TestValidationLimits(t *testing.T) {
	deep := pred("Make", "eq", "x")
	for i := 0; i < MaxDepth+2; i++ {
		deep = &Node{Type: "all", Children: []*Node{deep}}
	}
	if _, err := Prepare(deep, nil); err == nil {
		t.Fatal("over-deep query should be rejected")
	}
	bad := pred("Make", "regex", "([")
	if _, err := Prepare(bad, nil); err == nil {
		t.Fatal("invalid regex should be rejected")
	}
	badOp := pred("Make", "shell", "x")
	if _, err := Prepare(badOp, nil); err == nil {
		t.Fatal("unknown operator should be rejected")
	}
	badTag := pred("-config", "eq", "x")
	if _, err := Prepare(badTag, nil); err == nil {
		t.Fatal("option-like tag should be rejected")
	}
}
