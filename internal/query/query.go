// Package query implements the typed search predicate AST and its
// three-valued evaluator (match / no_match / indeterminate).
//
// The evaluator never guesses: values that cannot be converted exactly
// (non-numeric strings for numeric operators, mixed timezone datetimes
// without an explicit assumption, cross-document conditions without
// provenance) evaluate to indeterminate rather than a fabricated verdict.
package query

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/alfa-reza/exiftool-mcp-go/internal/meta"
)

// Tri is a three-valued truth value.
type Tri int

const (
	Indeterminate Tri = iota
	NoMatch
	Match
)

func (t Tri) String() string {
	switch t {
	case Match:
		return "match"
	case NoMatch:
		return "no_match"
	default:
		return "indeterminate"
	}
}

// And combines values with three-valued AND semantics.
func And(a, b Tri) Tri {
	if a == NoMatch || b == NoMatch {
		return NoMatch
	}
	if a == Indeterminate || b == Indeterminate {
		return Indeterminate
	}
	return Match
}

// Or combines values with three-valued OR semantics.
func Or(a, b Tri) Tri {
	if a == Match || b == Match {
		return Match
	}
	if a == Indeterminate || b == Indeterminate {
		return Indeterminate
	}
	return NoMatch
}

// Not negates a three-valued value.
func Not(a Tri) Tri {
	switch a {
	case Match:
		return NoMatch
	case NoMatch:
		return Match
	default:
		return Indeterminate
	}
}

// Node is the public JSON form of a query AST node.
type Node struct {
	Type           string  `json:"type"` // all | any | not | predicate
	Scope          string  `json:"scope,omitempty"`
	Children       []*Node `json:"children,omitempty"`
	Tag            string  `json:"tag,omitempty"`
	Operator       string  `json:"operator,omitempty"`
	Value          any     `json:"value,omitempty"`
	ValueType      string  `json:"value_type,omitempty"` // text | number | datetime
	DuplicateMatch string  `json:"duplicate_match,omitempty"`
	CaseSensitive  *bool   `json:"case_sensitive,omitempty"`
}

// TimeContext carries the agent's explicit timezone assumption used to
// compare naive datetimes with absolute timestamps.
type TimeContext struct {
	Timezone string `json:"timezone,omitempty"` // IANA name or offset like "+07:00"
}

// UnmarshalJSON decodes a node while preserving numeric literals
// exactly: the value is decoded with UseNumber, so a predicate
// value of 9007199254740993 stays that integer instead of
// degrading to the float64 9007199254740992 before the evaluator
// (whose exact-rational comparison could have handled it) ever
// sees it. The same decode serves the checkpoint round-trip — a
// scan stores its query as JSON and re-decodes it on resume, and
// both legs must preserve the literal or a match can silently
// become a no_match across a restart.
func (n *Node) UnmarshalJSON(data []byte) error {
	type alias Node
	aux := &struct {
		*alias
		Value json.RawMessage `json:"value"`
	}{alias: (*alias)(n)}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	n.Value = nil
	if len(aux.Value) > 0 && string(aux.Value) != "null" {
		dec := json.NewDecoder(bytes.NewReader(aux.Value))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return fmt.Errorf("decoding predicate value: %w", err)
		}
		n.Value = v
	}
	return nil
}

const (
	MaxDepth      = 8
	MaxNodes      = 200
	MaxRegexChars = 512
)

var operators = map[string]bool{
	"exists": true, "missing": true,
	"eq": true, "ne": true,
	"contains": true, "starts_with": true, "ends_with": true, "regex": true,
	"gt": true, "gte": true, "lt": true, "lte": true, "between": true,
	"in": true, "not_in": true,
	"before": true, "after": true,
}

// Prepared is a validated, compiled query.
type Prepared struct {
	root *pnode
	tz   *time.Location
}

type pnode struct {
	typ       string
	scope     string
	children  []*pnode
	tag       string
	operator  string
	value     any
	valueType string
	dupAll    bool
	caseSens  bool
	re        *regexp.Regexp
}

// Prepare validates and compiles a query AST.
func Prepare(n *Node, tc *TimeContext) (*Prepared, error) {
	if n == nil {
		return nil, fmt.Errorf("query is required")
	}
	p := &Prepared{}
	if tc != nil && tc.Timezone != "" {
		loc, err := parseTimezone(tc.Timezone)
		if err != nil {
			return nil, fmt.Errorf("time_context.timezone: %w", err)
		}
		p.tz = loc
	}
	count := 0
	root, err := prepareNode(n, 0, &count)
	if err != nil {
		return nil, err
	}
	p.root = root
	return p, nil
}

func parseTimezone(s string) (*time.Location, error) {
	if strings.HasPrefix(s, "+") || strings.HasPrefix(s, "-") {
		t, err := time.Parse("-07:00", s)
		if err != nil {
			return nil, fmt.Errorf("invalid offset %q", s)
		}
		_, off := t.Zone()
		return time.FixedZone(s, off), nil
	}
	return time.LoadLocation(s)
}

func prepareNode(n *Node, depth int, count *int) (*pnode, error) {
	*count++
	if *count > MaxNodes {
		return nil, fmt.Errorf("query has more than %d nodes", MaxNodes)
	}
	if depth > MaxDepth {
		return nil, fmt.Errorf("query nesting deeper than %d", MaxDepth)
	}
	scope := n.Scope
	if scope == "" {
		scope = "file"
	}
	if scope != "file" && scope != "same_document" {
		return nil, fmt.Errorf("invalid scope %q", n.Scope)
	}
	pn := &pnode{typ: n.Type, scope: scope, caseSens: true}
	if n.CaseSensitive != nil {
		pn.caseSens = *n.CaseSensitive
	}
	switch n.Type {
	case "all", "any":
		if len(n.Children) == 0 {
			return nil, fmt.Errorf("%s node needs at least one child", n.Type)
		}
		for _, c := range n.Children {
			pc, err := prepareNode(c, depth+1, count)
			if err != nil {
				return nil, err
			}
			pn.children = append(pn.children, pc)
		}
	case "not":
		if len(n.Children) != 1 {
			return nil, fmt.Errorf("not node needs exactly one child")
		}
		pc, err := prepareNode(n.Children[0], depth+1, count)
		if err != nil {
			return nil, err
		}
		pn.children = []*pnode{pc}
	case "predicate":
		if err := meta.ValidateSelector(n.Tag); err != nil {
			return nil, err
		}
		if !operators[n.Operator] {
			return nil, fmt.Errorf("unknown operator %q", n.Operator)
		}
		pn.tag = n.Tag
		pn.operator = n.Operator
		pn.value = n.Value
		pn.valueType = n.ValueType
		if pn.valueType != "" && pn.valueType != "text" && pn.valueType != "number" && pn.valueType != "datetime" {
			return nil, fmt.Errorf("invalid value_type %q", n.ValueType)
		}
		switch n.DuplicateMatch {
		case "", "any":
		case "all":
			pn.dupAll = true
		default:
			return nil, fmt.Errorf("invalid duplicate_match %q", n.DuplicateMatch)
		}
		if n.Operator != "exists" && n.Operator != "missing" && n.Value == nil {
			return nil, fmt.Errorf("operator %s requires a value", n.Operator)
		}
		if n.Operator == "regex" {
			pat, ok := n.Value.(string)
			if !ok {
				return nil, fmt.Errorf("regex value must be a string")
			}
			if len(pat) > MaxRegexChars {
				return nil, fmt.Errorf("regex longer than %d characters", MaxRegexChars)
			}
			if !pn.caseSens {
				pat = "(?i)" + pat
			}
			re, err := regexp.Compile(pat)
			if err != nil {
				return nil, fmt.Errorf("invalid regex: %w", err)
			}
			pn.re = re
		}
		if n.Operator == "between" || n.Operator == "in" || n.Operator == "not_in" {
			arr, ok := n.Value.([]any)
			if !ok || len(arr) == 0 {
				return nil, fmt.Errorf("operator %s requires a non-empty array value", n.Operator)
			}
			if n.Operator == "between" && len(arr) != 2 {
				return nil, fmt.Errorf("between requires exactly two bounds")
			}
		}
	default:
		return nil, fmt.Errorf("unknown node type %q", n.Type)
	}
	return pn, nil
}

// Eval evaluates the query against one file's entries.
func (p *Prepared) Eval(entries []meta.Entry) (Tri, string) {
	return p.evalNode(p.root, entries)
}

func (p *Prepared) evalNode(n *pnode, entries []meta.Entry) (Tri, string) {
	if n.scope == "same_document" && n.typ != "predicate" {
		return p.evalSameDocument(n, entries)
	}
	switch n.typ {
	case "all":
		result := Match
		var reason string
		for _, c := range n.children {
			t, r := p.evalNode(c, entries)
			result = And(result, t)
			if t == Indeterminate && reason == "" {
				reason = r
			}
			if result == NoMatch {
				return NoMatch, ""
			}
		}
		return result, reason
	case "any":
		result := NoMatch
		var reason string
		for _, c := range n.children {
			t, r := p.evalNode(c, entries)
			result = Or(result, t)
			if t == Indeterminate && reason == "" {
				reason = r
			}
			if result == Match {
				return Match, ""
			}
		}
		return result, reason
	case "not":
		t, r := p.evalNode(n.children[0], entries)
		return Not(t), r
	case "predicate":
		return p.evalPredicate(n, entries)
	}
	return Indeterminate, "unknown node"
}

// hasNegativeConstruct reports whether the group contains a
// negative condition (a missing/ne/not_in predicate, or a not
// node). Negative conditions are grouping-sensitive: a tag whose
// presence defeats "missing" in a pooled evaluation might live in
// a different document than the entries it is pooled with, so a
// pooled failure can never prove that no same-document grouping
// satisfies the group.
func hasNegativeConstruct(n *pnode) bool {
	if n.typ == "not" {
		return true
	}
	if n.typ == "predicate" {
		switch n.operator {
		case "missing", "ne", "not_in":
			return true
		}
	}
	for _, c := range n.children {
		if hasNegativeConstruct(c) {
			return true
		}
	}
	return false
}

// evalSameDocument evaluates a logic node once per document bucket and
// matches if any single document satisfies the whole group.
//
// A match requires provenance: entries are only grouped when ExifTool
// gave them a document (or instance) identity. Entries without any
// identity cannot be proven to share a document, so if pooling them
// would satisfy the group the verdict is indeterminate — never a
// fabricated match.
func (p *Prepared) evalSameDocument(n *pnode, entries []meta.Entry) (Tri, string) {
	neg := hasNegativeConstruct(n)
	buckets := make(map[string][]meta.Entry)
	var order []string
	var unknown []meta.Entry
	for _, e := range entries {
		k := e.Document
		if k == "" {
			k = e.Instance
		}
		if k == "" {
			unknown = append(unknown, e)
			continue
		}
		if _, ok := buckets[k]; !ok {
			order = append(order, k)
		}
		buckets[k] = append(buckets[k], e)
	}
	inner := *n
	inner.scope = "file"
	if len(entries) == 0 {
		// A successful read that produced no tags still has one
		// implicit document scope — the file itself — so evaluate
		// the group against the empty set, exactly as file scope
		// does, instead of falling through to a blanket no_match:
		// "Make missing" over a tagless file is a match.
		return p.evalNode(&inner, nil)
	}
	sawIndet := false
	var reason string
	for _, k := range order {
		t, r := p.evalNode(&inner, buckets[k])
		if t == Match {
			if neg && len(unknown) > 0 {
				// The bucket satisfies the group on its own, but an
				// unprovenanced entry could belong to this very
				// document and defeat a negative condition in it.
				// The match is possible, not proven.
				sawIndet = true
				if reason == "" {
					reason = "same_document cannot be proven: some entries have no document identity"
				}
				continue
			}
			return Match, ""
		}
		if t == Indeterminate {
			sawIndet = true
			reason = r
		}
	}
	if len(unknown) > 0 {
		// Pooling is the most permissive grouping of the unprovenanced
		// entries FOR POSITIVE CONDITIONS: if even pooled they cannot
		// satisfy the group, no same-document grouping of them could
		// either (definite no-match). If pooled they would match, the
		// truth depends on provenance ExifTool did not provide. With
		// a negative condition in the group the pooled failure proves
		// nothing: the defeating entries could belong to a different
		// document than the rest, so the verdict is indeterminate.
		t, r := p.evalNode(&inner, unknown)
		if t == Match {
			return Indeterminate, "same_document cannot be proven: entries have no document identity"
		}
		if t == Indeterminate {
			sawIndet = true
			if reason == "" {
				reason = r
			}
		}
		if t == NoMatch && neg {
			sawIndet = true
			if reason == "" {
				reason = "same_document cannot be proven: entries have no document identity"
			}
		}
		// The mixed case: a leg with known provenance and a leg
		// without any may still share a document — the unprovenanced
		// entries could belong to any of the known documents.
		// Evaluate each provenanced bucket together with the
		// unprovenanced entries; a match there is possible but
		// unprovable, hence indeterminate. Checking the groups only
		// in isolation would fabricate a no_match here.
		for _, k := range order {
			joined := make([]meta.Entry, 0, len(buckets[k])+len(unknown))
			joined = append(joined, buckets[k]...)
			joined = append(joined, unknown...)
			t, r := p.evalNode(&inner, joined)
			if t == Match {
				return Indeterminate, "same_document cannot be proven: some entries have no document identity"
			}
			if t == Indeterminate {
				sawIndet = true
				if reason == "" {
					reason = r
				}
			}
			if t == NoMatch && neg {
				// The unprovenanced entries defeating a negative
				// condition here might not belong to this bucket
				// at all; the failure is unprovable, not definite.
				sawIndet = true
				if reason == "" {
					reason = "same_document cannot be proven: some entries have no document identity"
				}
			}
		}
	}
	if sawIndet {
		return Indeterminate, reason
	}
	return NoMatch, ""
}

// matchesSelector reports whether an entry is selected by a tag selector
// ("Name", "Group:Name" or "Group:All"). Tag and group names compare
// case-insensitively: ExifTool itself resolves tag names without
// regard to case, so a query for "make" must find the entry "Make" —
// an exact-case comparison silently drops real matches. (Values are
// unaffected; their comparison keeps the case_sensitive rule.)
func matchesSelector(e meta.Entry, sel string) bool {
	group, name := "", sel
	if i := strings.Index(sel, ":"); i >= 0 {
		group, name = sel[:i], sel[i+1:]
	}
	if !strings.EqualFold(name, "All") && !strings.EqualFold(e.Name, name) {
		return false
	}
	if group == "" {
		return true
	}
	return groupMatches(e.Group, group)
}

// groupMatches reports whether a selector group refers to an entry
// group: it matches the full group ("EXIF:IFD0") or any single family
// segment of it ("EXIF" or "IFD0"), case-insensitively.
func groupMatches(entryGroup, selGroup string) bool {
	if strings.EqualFold(entryGroup, selGroup) {
		return true
	}
	for _, seg := range strings.Split(entryGroup, ":") {
		if strings.EqualFold(seg, selGroup) {
			return true
		}
	}
	return false
}

func (p *Prepared) evalPredicate(n *pnode, entries []meta.Entry) (Tri, string) {
	var cands []meta.Entry
	for _, e := range entries {
		if matchesSelector(e, n.tag) {
			cands = append(cands, e)
		}
	}
	switch n.operator {
	case "exists":
		if len(cands) > 0 {
			return Match, ""
		}
		return NoMatch, ""
	case "missing":
		if len(cands) == 0 {
			return Match, ""
		}
		return NoMatch, ""
	}
	if len(cands) == 0 {
		// No values: 'all' is explicitly not true; 'any' cannot match.
		return NoMatch, ""
	}
	var values []any
	for _, e := range cands {
		values = append(values, flatten(e.Value)...)
	}
	if len(values) == 0 {
		return NoMatch, ""
	}
	agg := Match
	if !n.dupAll {
		agg = NoMatch
	}
	var reason string
	for _, v := range values {
		t, r := p.compare(n, v)
		if r != "" && reason == "" {
			reason = r
		}
		if n.dupAll {
			agg = And(agg, t)
			if agg == NoMatch {
				return NoMatch, ""
			}
		} else {
			agg = Or(agg, t)
			if agg == Match {
				return Match, ""
			}
		}
	}
	if agg == Indeterminate {
		return Indeterminate, reason
	}
	return agg, ""
}

// flatten expands array values into their elements.
func flatten(v any) []any {
	if arr, ok := v.([]any); ok {
		var out []any
		for _, el := range arr {
			out = append(out, flatten(el)...)
		}
		return out
	}
	return []any{v}
}
