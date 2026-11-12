package query

import (
	"encoding/json"
	"testing"
)

// Round-9 regression tests: numeric ordering uses the exact
// rational form before any float64 fallback.

func TestHugeNumbersOrderExactly(t *testing.T) {
	es := entries("EXIF:Big", json.Number("1e400"))
	if got := eval(t, pred("Big", "gt", json.Number("1e399")), nil, es); got != Match {
		t.Fatalf("1e400 gt 1e399 = %v, want Match", got)
	}
	if got := eval(t, pred("Big", "lt", json.Number("1e399")), nil, es); got != NoMatch {
		t.Fatalf("1e400 lt 1e399 = %v, want NoMatch", got)
	}
	if got := eval(t, pred("Big", "eq", json.Number("1e400")), nil, es); got != Match {
		t.Fatalf("1e400 eq 1e400 = %v, want Match", got)
	}
	// A huge value also orders against an ordinary one.
	if got := eval(t, pred("Big", "gt", 20), nil, es); got != Match {
		t.Fatalf("1e400 gt 20 = %v, want Match", got)
	}
	// Non-finite values stay indeterminate on the ordering
	// paths too (the round-6 protection is not weakened).
	nan := entries("EXIF:Big", "NaN")
	if got := eval(t, pred("Big", "gt", 20), nil, nan); got != Indeterminate {
		t.Fatalf("NaN gt 20 = %v, want Indeterminate", got)
	}
	inf := entries("EXIF:Big", "Inf")
	if got := eval(t, pred("Big", "gt", 20), nil, inf); got != Indeterminate {
		t.Fatalf("Inf gt 20 = %v, want Indeterminate", got)
	}
}
