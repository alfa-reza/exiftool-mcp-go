package query

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// compare evaluates one predicate operator against one scalar value.
func (p *Prepared) compare(n *pnode, v any) (Tri, string) {
	switch n.operator {
	case "contains", "starts_with", "ends_with", "regex":
		s, ok := toText(v)
		if !ok {
			return Indeterminate, "value is not text"
		}
		want, ok := toText(n.value)
		if !ok {
			return Indeterminate, "query value is not text"
		}
		if n.operator == "regex" {
			if n.re.MatchString(s) {
				return Match, ""
			}
			return NoMatch, ""
		}
		a, b := s, want
		if !n.caseSens {
			a, b = strings.ToLower(a), strings.ToLower(b)
		}
		var hit bool
		switch n.operator {
		case "contains":
			hit = strings.Contains(a, b)
		case "starts_with":
			hit = strings.HasPrefix(a, b)
		case "ends_with":
			hit = strings.HasSuffix(a, b)
		}
		if hit {
			return Match, ""
		}
		return NoMatch, ""
	case "in", "not_in":
		arr := n.value.([]any)
		agg := NoMatch
		var reason string
		for _, el := range arr {
			sub := *n
			sub.operator = "eq"
			sub.value = el
			t, r := p.compare(&sub, v)
			agg = Or(agg, t)
			if agg == Match {
				break
			}
			if t == Indeterminate && r != "" && reason == "" {
				reason = r
			}
		}
		if n.operator == "not_in" {
			agg = Not(agg)
		}
		if agg == Indeterminate {
			return Indeterminate, reason
		}
		return agg, ""
	case "before", "after":
		return p.compareDateOrder(n, v)
	case "gt", "gte", "lt", "lte":
		if n.valueType == "datetime" {
			return p.compareDateOrder(n, v)
		}
		return p.compareNumberOrder(n, v)
	case "between":
		return p.compareBetween(n, v)
	case "eq", "ne":
		t, r := p.compareEqual(n, v)
		if n.operator == "ne" {
			return Not(t), r
		}
		return t, r
	}
	return Indeterminate, "unsupported operator"
}

// compareEqual compares for equality using the declared or inferred kind.
func (p *Prepared) compareEqual(n *pnode, v any) (Tri, string) {
	kind := n.valueType
	if kind == "" {
		kind = inferKind(n.value, v)
	}
	switch kind {
	case "number":
		a, okA := toNumber(v)
		b, okB := toNumber(n.value)
		if !okA || !okB {
			return Indeterminate, "value is not numeric"
		}
		if a == b {
			return Match, ""
		}
		return NoMatch, ""
	case "datetime":
		return p.compareDate(v, n.value, func(c int) bool { return c == 0 })
	default: // text
		a, okA := toText(v)
		b, okB := toText(n.value)
		if !okA || !okB {
			return Indeterminate, "value is not comparable as text"
		}
		eq := a == b
		if !n.caseSens {
			eq = strings.EqualFold(a, b)
		}
		if eq {
			return Match, ""
		}
		return NoMatch, ""
	}
}

// inferKind picks a comparison kind from the query value and entry value.
func inferKind(queryVal, entryVal any) string {
	if _, ok := toNumber(queryVal); ok {
		if _, ok := toNumber(entryVal); ok {
			return "number"
		}
	}
	qs, qok := queryVal.(string)
	es, eok := entryVal.(string)
	if qok && eok {
		if _, ok := parseDateTime(qs); ok {
			if _, ok := parseDateTime(es); ok {
				return "datetime"
			}
		}
	}
	return "text"
}

func (p *Prepared) compareNumberOrder(n *pnode, v any) (Tri, string) {
	a, okA := toNumber(v)
	b, okB := toNumber(n.value)
	if !okA || !okB {
		return Indeterminate, "value is not numeric"
	}
	var hit bool
	switch n.operator {
	case "gt":
		hit = a > b
	case "gte":
		hit = a >= b
	case "lt":
		hit = a < b
	case "lte":
		hit = a <= b
	}
	if hit {
		return Match, ""
	}
	return NoMatch, ""
}

func (p *Prepared) compareBetween(n *pnode, v any) (Tri, string) {
	bounds := n.value.([]any)
	kind := n.valueType
	if kind == "" {
		kind = inferKind(bounds[0], v)
	}
	if kind == "datetime" {
		lo, ok := bounds[0].(string)
		hi, ok2 := bounds[1].(string)
		if !ok || !ok2 {
			return Indeterminate, "datetime bounds must be strings"
		}
		loT, okL := parseDateTime(lo)
		hiT, okH := parseDateTime(hi)
		evT, okE := parseDateTime(mustText(v))
		if !okL || !okH || !okE {
			return Indeterminate, "value or bound is not a valid datetime"
		}
		// Compare lower bound, then upper bound (whole day if date-only).
		cLo, r := p.cmpDate(evT, loT)
		if r != "" {
			return Indeterminate, r
		}
		hiCmp := hiT
		if hiT.dateOnly {
			hiCmp.t = hiT.t.Add(24*time.Hour - time.Nanosecond)
		}
		cHi, r := p.cmpDate(evT, hiCmp)
		if r != "" {
			return Indeterminate, r
		}
		if cLo >= 0 && cHi <= 0 {
			return Match, ""
		}
		return NoMatch, ""
	}
	// numeric between (inclusive)
	a, okA := toNumber(v)
	lo, okL := toNumber(bounds[0])
	hi, okH := toNumber(bounds[1])
	if !okA {
		return Indeterminate, "value is not numeric"
	}
	if !okL || !okH {
		return Indeterminate, "between bounds are not numeric"
	}
	if a >= lo && a <= hi {
		return Match, ""
	}
	return NoMatch, ""
}

// compareDateOrder applies the node's ordering operator to a datetime
// value. The inclusive operators (gte/lte) accept an exact match with
// the bound; before/after are strict, like gt/lt.
func (p *Prepared) compareDateOrder(n *pnode, v any) (Tri, string) {
	bound, ok := n.value.(string)
	if !ok {
		return Indeterminate, "datetime bound must be a string"
	}
	return p.compareDate(v, bound, func(c int) bool {
		switch n.operator {
		case "before", "lt":
			return c < 0
		case "after", "gt":
			return c > 0
		case "lte":
			return c <= 0
		case "gte":
			return c >= 0
		}
		return false
	})
}

// compareDate parses both sides and applies the comparator to their
// ordering (-1, 0, 1).
func (p *Prepared) compareDate(entryVal, queryVal any, test func(int) bool) (Tri, string) {
	es, ok := entryVal.(string)
	if !ok {
		return Indeterminate, "value is not a datetime string"
	}
	qs, ok := queryVal.(string)
	if !ok {
		return Indeterminate, "query value is not a datetime string"
	}
	e, okE := parseDateTime(es)
	q, okQ := parseDateTime(qs)
	if !okE || !okQ {
		return Indeterminate, "value or bound is not a valid datetime"
	}
	c, reason := p.cmpDate(e, q)
	if reason != "" {
		return Indeterminate, reason
	}
	if test(c) {
		return Match, ""
	}
	return NoMatch, ""
}

// cmpDate orders two parsed datetimes. Wall-clock values compare as
// wall clock; aware values compare as absolute instants; a mix needs
// the explicit time context, otherwise it reports a reason.
func (p *Prepared) cmpDate(a, b parsedDate) (int, string) {
	if a.hasTZ == b.hasTZ {
		return a.t.Compare(b.t), ""
	}
	if p.tz == nil {
		return 0, "mixed timezone comparison without time_context"
	}
	naive, aware, flipped := a, b, false
	if a.hasTZ {
		naive, aware, flipped = b, a, true
	}
	nt := naive.t
	rebased := time.Date(nt.Year(), nt.Month(), nt.Day(), nt.Hour(), nt.Minute(), nt.Second(), nt.Nanosecond(), p.tz)
	c := rebased.Compare(aware.t)
	if flipped {
		c = -c
	}
	return c, ""
}

type parsedDate struct {
	t        time.Time
	hasTZ    bool
	dateOnly bool
}

var dateLayoutsTZ = []string{
	"2006:01:02 15:04:05.999999999Z07:00",
	"2006:01:02 15:04:05Z07:00",
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05Z07:00",
}

var dateLayoutsNaive = []string{
	"2006:01:02 15:04:05.999999999",
	"2006:01:02 15:04:05",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
}

var dateOnlyLayouts = []string{"2006:01:02", "2006-01-02"}

// parseDateTime parses the datetime formats ExifTool commonly emits.
func parseDateTime(s string) (parsedDate, bool) {
	s = strings.TrimSpace(s)
	for _, l := range dateLayoutsTZ {
		if t, err := time.Parse(l, s); err == nil {
			return parsedDate{t: t, hasTZ: true}, true
		}
	}
	for _, l := range dateLayoutsNaive {
		if t, err := time.ParseInLocation(l, s, time.UTC); err == nil {
			return parsedDate{t: t}, true
		}
	}
	for _, l := range dateOnlyLayouts {
		if t, err := time.ParseInLocation(l, s, time.UTC); err == nil {
			return parsedDate{t: t, dateOnly: true}, true
		}
	}
	return parsedDate{}, false
}

// toNumber converts a JSON-ish value to float64 without guessing:
// numeric strings must parse in full; strings with units do not convert.
func toNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// toText renders a scalar as text for text operators and equality.
func toText(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case json.Number:
		return x.String(), true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case bool:
		if x {
			return "true", true
		}
		return "false", true
	case nil:
		return "", true
	}
	return "", false
}

func mustText(v any) string {
	s, _ := toText(v)
	return s
}
