package query

import (
	"encoding/json"
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

func TestDatetimeInclusiveBounds(t *testing.T) {
	// gte/lte must accept a value exactly equal to the bound; gt/lt
	// must not.
	es := entries("EXIF:DateTimeOriginal", "2023:05:01 10:00:00")
	bound := "2023:05:01 10:00:00"
	for _, tc := range []struct {
		op   string
		want Tri
	}{
		{"gte", Match}, {"lte", Match}, {"gt", NoMatch}, {"lt", NoMatch},
		{"before", NoMatch}, {"after", NoMatch},
	} {
		n := pred("DateTimeOriginal", tc.op, bound)
		n.ValueType = "datetime"
		if got := eval(t, n, nil, es); got != tc.want {
			t.Fatalf("%s with equal bound: got %v, want %v", tc.op, got, tc.want)
		}
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
	// Without any document identity, the same conditions pooled
	// together would match, but provenance is unproven: the verdict
	// must be indeterminate, never a match.
	es3 := entries(
		"GPS:GPSLatitude", float64(6.2),
		"EXIF:DateTimeOriginal", "2023:05:01 10:00:00",
	)
	if got := eval(t, group, nil, es3); got != Indeterminate {
		t.Fatalf("same_document without provenance must be indeterminate: %v", got)
	}
}

func TestTagMatchCaseInsensitive(t *testing.T) {
	// ExifTool resolves tag names case-insensitively; the evaluator
	// must do the same or real matches silently disappear.
	es := entries("EXIF:Make", "Canon", "EXIF:ISO", float64(100))
	for _, tag := range []string{"make", "MAKE", "exif:make", "EXIF:MAKE"} {
		if got := eval(t, pred(tag, "eq", "Canon"), nil, es); got != Match {
			t.Fatalf("tag %q should match Make=Canon, got %v", tag, got)
		}
	}
	if got := eval(t, pred("make", "exists", nil), nil, es); got != Match {
		t.Fatalf("exists should match case-insensitively, got %v", got)
	}
	// Values keep their own rule: default comparison is
	// case-sensitive, so a differently-cased VALUE must not match.
	if got := eval(t, pred("make", "eq", "canon"), nil, es); got != NoMatch {
		t.Fatalf("value comparison must stay case-sensitive by default, got %v", got)
	}
}

func TestSameDocumentMixedProvenance(t *testing.T) {
	group := &Node{Type: "all", Scope: "same_document", Children: []*Node{
		pred("Make", "eq", "Canon"), pred("ISO", "eq", float64(100)),
	}}
	// Make is provenanced to Doc1, ISO has no provenance at all. ISO
	// could belong to Doc1, so the honest verdict is indeterminate —
	// evaluating the groups in isolation fabricated a no_match here.
	es := entries("Doc1:EXIF:Make", "Canon", "EXIF:ISO", float64(100))
	if got := eval(t, group, nil, es); got != Indeterminate {
		t.Fatalf("mixed provenance should be indeterminate, got %v", got)
	}
	// The unprovenanced value cannot satisfy its leg under any
	// grouping: a definite no_match.
	es2 := entries("Doc1:EXIF:Make", "Canon", "EXIF:ISO", float64(200))
	if got := eval(t, group, nil, es2); got != NoMatch {
		t.Fatalf("unsatisfiable leg should be no_match, got %v", got)
	}
	// The provenanced leg itself fails: no_match under any grouping.
	es3 := entries("Doc1:EXIF:Make", "Nikon", "EXIF:ISO", float64(100))
	if got := eval(t, group, nil, es3); got != NoMatch {
		t.Fatalf("failed provenanced leg should be no_match, got %v", got)
	}
}

func TestLargeIntegersCompareExactly(t *testing.T) {
	// 2^53+1 is not representable as float64; comparing through
	// float64 would call it equal to 2^53.
	es := entries("EXIF:Big", json.Number("9007199254740993"))
	if got := eval(t, pred("Big", "eq", json.Number("9007199254740992")), nil, es); got != NoMatch {
		t.Fatalf("9007199254740993 must not equal 9007199254740992, got %v", got)
	}
	if got := eval(t, pred("Big", "eq", json.Number("9007199254740993")), nil, es); got != Match {
		t.Fatalf("equal big integers should match, got %v", got)
	}
	if got := eval(t, pred("Big", "gt", json.Number("9007199254740992")), nil, es); got != Match {
		t.Fatalf("big integer ordering should be exact, got %v", got)
	}
	if got := eval(t, pred("Big", "between", []any{json.Number("9007199254740992"), json.Number("9007199254740994")}), nil, es); got != Match {
		t.Fatalf("big integer between should be exact, got %v", got)
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

func TestStringIdentifiersCompareAsText(t *testing.T) {
	// Two numeric-looking STRINGS compare as text: leading zeros in
	// identifiers are significant, so "00123" != "123".
	es := entries("EXIF:SerialNumber", "00123")
	if got := eval(t, pred("SerialNumber", "eq", "123"), nil, es); got != NoMatch {
		t.Fatalf("SerialNumber eq \"123\" = %v, want no_match", got)
	}
	if got := eval(t, pred("SerialNumber", "eq", "00123"), nil, es); got != Match {
		t.Fatalf("SerialNumber eq \"00123\" = %v, want match", got)
	}
	if got := eval(t, pred("SerialNumber", "ne", "123"), nil, es); got != Match {
		t.Fatalf("SerialNumber ne \"123\" = %v, want match", got)
	}
	// A genuinely numeric query literal still compares numerically
	// against a numeric-looking string entry.
	es2 := entries("EXIF:ISO", "3200")
	if got := eval(t, pred("ISO", "eq", 3200), nil, es2); got != Match {
		t.Fatalf("ISO eq 3200 = %v, want match", got)
	}
	// An explicit value_type opts back into numeric comparison.
	n := pred("SerialNumber", "eq", "123")
	n.ValueType = "number"
	if got := eval(t, n, nil, es); got != Match {
		t.Fatalf("SerialNumber eq \"123\" with value_type=number = %v, want match", got)
	}
}

func TestSameDocumentNegativeNeedsProvenance(t *testing.T) {
	// No provenance at all, and the group fails only because ISO is
	// present in the pool: Make and ISO could live in different
	// documents, and the Make document would satisfy the group.
	// no_match is unprovable — the verdict is indeterminate.
	es := entries("EXIF:Make", "Canon", "EXIF:ISO", 100)
	n := &Node{Type: "all", Scope: "same_document", Children: []*Node{
		{Type: "predicate", Tag: "Make", Operator: "eq", Value: "Canon"},
		{Type: "predicate", Tag: "ISO", Operator: "missing"},
	}}
	if got := eval(t, n, nil, es); got != Indeterminate {
		t.Fatalf("same_document negative without provenance = %v, want indeterminate", got)
	}
	// A POSITIVE failure stays a definite no_match: no grouping of
	// these entries can produce a Nikon Make.
	n2 := &Node{Type: "all", Scope: "same_document", Children: []*Node{
		{Type: "predicate", Tag: "Make", Operator: "eq", Value: "Nikon"},
		{Type: "predicate", Tag: "ISO", Operator: "eq", Value: 100},
	}}
	if got := eval(t, n2, nil, es); got != NoMatch {
		t.Fatalf("same_document positive failure = %v, want no_match", got)
	}
	// Mixed provenance: the known bucket fails only because the
	// unprovenanced ISO entry was joined into it — the entry might
	// belong to another document, so this is indeterminate too.
	esMixed := entries("Doc1:EXIF:Make", "Canon", "EXIF:ISO", 100)
	if got := eval(t, n, nil, esMixed); got != Indeterminate {
		t.Fatalf("same_document negative with mixed provenance = %v, want indeterminate", got)
	}
}

func TestDateLikeIdentifiersCompareAsText(t *testing.T) {
	// Two strings that merely parse as datetimes compare as text
	// for equality: "2026-11-10" is not the same identifier as
	// "2026:11:10". Datetime equality needs an explicit value_type.
	es := entries("EXIF:SerialNumber", "2026-11-10")
	if got := eval(t, pred("SerialNumber", "eq", "2026:11:10"), nil, es); got != NoMatch {
		t.Fatalf("SerialNumber eq \"2026:11:10\" = %v, want no_match", got)
	}
	if got := eval(t, pred("SerialNumber", "eq", "2026-11-10"), nil, es); got != Match {
		t.Fatalf("SerialNumber eq \"2026-11-10\" = %v, want match", got)
	}
	n := pred("SerialNumber", "eq", "2026:11:10")
	n.ValueType = "datetime"
	if got := eval(t, n, nil, es); got != Match {
		t.Fatalf("SerialNumber eq with value_type=datetime = %v, want match", got)
	}
}

func TestSameDocumentEmptyEntries(t *testing.T) {
	// A read that produced no tags still has the file itself as
	// its one implicit document: "Make missing" matches, exactly
	// as in file scope, instead of a blanket no_match.
	n := &Node{Type: "all", Scope: "same_document", Children: []*Node{
		{Type: "predicate", Tag: "Make", Operator: "missing"},
	}}
	if got := eval(t, n, nil, nil); got != Match {
		t.Fatalf("same_document Make missing over zero entries = %v, want match", got)
	}
	n2 := &Node{Type: "all", Scope: "same_document", Children: []*Node{
		{Type: "predicate", Tag: "Make", Operator: "eq", Value: "Canon"},
	}}
	if got := eval(t, n2, nil, nil); got != NoMatch {
		t.Fatalf("same_document Make eq Canon over zero entries = %v, want no_match", got)
	}
}

func TestNonFiniteNumbersAreIndeterminate(t *testing.T) {
	// ParseFloat accepts "NaN"/"Inf", but NaN is unordered: in
	// float fallback comparison it read as "equal" to every
	// number, in both directions. Non-finite operands are not
	// numbers; verdicts over them are indeterminate.
	num := func(tag, op string, v any) *Node {
		p := pred(tag, op, v)
		p.ValueType = "number"
		return p
	}
	if got := eval(t, num("ISO", "eq", 20), nil, entries("EXIF:ISO", "NaN")); got != Indeterminate {
		t.Fatalf("ISO \"NaN\" eq 20 = %v, want indeterminate", got)
	}
	if got := eval(t, num("ISO", "eq", "NaN"), nil, entries("EXIF:ISO", 100)); got != Indeterminate {
		t.Fatalf("ISO 100 eq \"NaN\" = %v, want indeterminate", got)
	}
	if got := eval(t, num("ISO", "gt", 1000000), nil, entries("EXIF:ISO", "Inf")); got != Indeterminate {
		t.Fatalf("ISO \"Inf\" gt 1000000 = %v, want indeterminate", got)
	}
	if got := eval(t, num("ISO", "eq", 20), nil, entries("EXIF:ISO", "20")); got != Match {
		t.Fatalf("ISO \"20\" eq 20 = %v, want match (finite control)", got)
	}
}

func TestNullIsNotEmptyText(t *testing.T) {
	// A null value means the tag is present without a value; it
	// is not the empty string, so value comparisons are
	// indeterminate while presence still counts.
	es := entries("File:Comment", nil)
	if got := eval(t, pred("Comment", "eq", ""), nil, es); got != Indeterminate {
		t.Fatalf("Comment null eq \"\" = %v, want indeterminate", got)
	}
	if got := eval(t, pred("Comment", "exists", nil), nil, es); got != Match {
		t.Fatalf("Comment null exists = %v, want match", got)
	}
}

func TestNodeDecodePreservesBigNumbers(t *testing.T) {
	// A plain decode turns 9007199254740993 into the float64
	// 9007199254740992 before the evaluator sees it; the literal
	// must survive decoding, evaluation, and the checkpoint
	// round-trip (marshal + re-decode, as resume does).
	raw := []byte(`{"type":"predicate","tag":"Big","operator":"eq","value":9007199254740993}`)
	var node Node
	if err := json.Unmarshal(raw, &node); err != nil {
		t.Fatal(err)
	}
	num, ok := node.Value.(json.Number)
	if !ok || num.String() != "9007199254740993" {
		t.Fatalf("decoded value = %T %v, want json.Number 9007199254740993", node.Value, node.Value)
	}
	es := []meta.Entry{meta.EntryFromPair("File:Big", json.Number("9007199254740993"))}
	if got := eval(t, &node, nil, es); got != Match {
		t.Fatalf("identical big literals = %v, want match", got)
	}
	raw2, err := json.Marshal(&node)
	if err != nil {
		t.Fatal(err)
	}
	var node2 Node
	if err := json.Unmarshal(raw2, &node2); err != nil {
		t.Fatal(err)
	}
	if got := eval(t, &node2, nil, es); got != Match {
		t.Fatalf("after checkpoint round-trip = %v, want match", got)
	}
	// Nested numbers (in-operator lists) keep their precision too.
	var node3 Node
	if err := json.Unmarshal([]byte(`{"type":"predicate","tag":"Big","operator":"in","value":[9007199254740993,7]}`), &node3); err != nil {
		t.Fatal(err)
	}
	list, ok := node3.Value.([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("in-list decoded as %T, want []any of 2", node3.Value)
	}
	if n, ok := list[0].(json.Number); !ok || n.String() != "9007199254740993" {
		t.Fatalf("in-list[0] = %T %v, want json.Number 9007199254740993", list[0], list[0])
	}
}
