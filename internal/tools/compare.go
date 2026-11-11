package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alfa-reza/exiftool-mcp-go/internal/meta"
)

// --- compare_metadata ---

type CompareMetadataIn struct {
	LeftPath         string                `json:"left_path"`
	RightPath        string                `json:"right_path"`
	MetadataFilter   *MetadataFilter       `json:"metadata_filter,omitempty"`
	IgnoreTags       []string              `json:"ignore_tags,omitempty"`
	IncludeUnchanged bool                  `json:"include_unchanged,omitempty"`
	ReadOptions      *ReadOptionsInput     `json:"read_options,omitempty"`
	ResponseOptions  *ResponseOptionsInput `json:"response_options,omitempty"`
}

// ChangeRecord is one compared tag identity.
type ChangeRecord struct {
	Change        string         `json:"change"` // added | removed | changed | unchanged
	Key           string         `json:"key"`
	Group         string         `json:"group,omitempty"`
	Name          string         `json:"name"`
	Document      string         `json:"document,omitempty"`
	Instance      string         `json:"instance,omitempty"`
	Copy          string         `json:"copy,omitempty"`
	LeftValue     any            `json:"left_value,omitempty"`
	RightValue    any            `json:"right_value,omitempty"`
	LeftValueRef  *meta.ValueRef `json:"left_value_ref,omitempty"`
	RightValueRef *meta.ValueRef `json:"right_value_ref,omitempty"`
	Ambiguous     bool           `json:"ambiguous,omitempty"`
}

type CompareMetadataOut struct {
	Status           string         `json:"status"` // complete | partial | failed
	Added            []ChangeRecord `json:"added,omitempty"`
	Removed          []ChangeRecord `json:"removed,omitempty"`
	Changed          []ChangeRecord `json:"changed,omitempty"`
	Unchanged        []ChangeRecord `json:"unchanged,omitempty"`
	UnchangedCount   int            `json:"unchanged_count"`
	Errors           []ToolError    `json:"errors,omitempty"`
	EffectiveOptions map[string]any `json:"effective_options,omitempty"`
	ResultID         string         `json:"result_id,omitempty"`
	NextCursor       string         `json:"next_cursor,omitempty"`
	HasMore          bool           `json:"has_more"`
	ExpiresAt        string         `json:"expires_at,omitempty"`
	Complete         bool           `json:"complete"`
}

func identityOf(e meta.Entry) string {
	return e.Group + "\x00" + e.Name + "\x00" + e.Document + "\x00" + e.Instance
}

// pairBucket pairs the entries of one tag identity between the two
// sides of a comparison. Duplicate copies pair by VALUE first:
// ExifTool renumbers copies when one is deleted, so purely
// positional pairing would report a deleted first copy as a value
// change of the (renumbered) survivor plus a removal of the second
// copy. Copies left over after value pairing pair positionally as
// changes; any excess is a removal or an addition. Each record
// carries the Copy identity of the entry it came from.
func pairBucket(ls, rs []meta.Entry, ambiguous bool) []ChangeRecord {
	mk := func(change string, sample meta.Entry) ChangeRecord {
		return ChangeRecord{
			Change: change, Key: sample.Key, Group: sample.Group, Name: sample.Name,
			Document: sample.Document, Instance: sample.Instance, Copy: sample.Copy,
			Ambiguous: ambiguous,
		}
	}
	valueKey := func(e meta.Entry) string {
		b, _ := json.Marshal(e.Value)
		return string(b)
	}
	rightByValue := make(map[string][]int)
	for j, r := range rs {
		k := valueKey(r)
		rightByValue[k] = append(rightByValue[k], j)
	}
	usedR := make([]bool, len(rs))
	var out []ChangeRecord
	var looseL []int
	for i, l := range ls {
		k := valueKey(l)
		if queue := rightByValue[k]; len(queue) > 0 {
			j := queue[0]
			rightByValue[k] = queue[1:]
			usedR[j] = true
			rec := mk("unchanged", l)
			rec.LeftValue, rec.RightValue = l.Value, rs[j].Value
			out = append(out, rec)
			continue
		}
		looseL = append(looseL, i)
	}
	var looseR []int
	for j := range rs {
		if !usedR[j] {
			looseR = append(looseR, j)
		}
	}
	for x := 0; x < max(len(looseL), len(looseR)); x++ {
		switch {
		case x >= len(looseL):
			rec := mk("added", rs[looseR[x]])
			rec.RightValue = rs[looseR[x]].Value
			out = append(out, rec)
		case x >= len(looseR):
			rec := mk("removed", ls[looseL[x]])
			rec.LeftValue = ls[looseL[x]].Value
			out = append(out, rec)
		default:
			l, r := ls[looseL[x]], rs[looseR[x]]
			rec := mk("changed", l)
			rec.LeftValue, rec.RightValue = l.Value, r.Value
			out = append(out, rec)
		}
	}
	return out
}

func (a *App) compareMetadata(ctx context.Context, _ *mcp.CallToolRequest, in CompareMetadataIn) (*mcp.CallToolResult, CompareMetadataOut, error) {
	out := CompareMetadataOut{Status: "complete", Complete: true}
	fail := func(code, msg string) (*mcp.CallToolResult, CompareMetadataOut, error) {
		out.Status = "failed"
		out.Errors = append(out.Errors, ToolError{Code: code, Message: msg})
		return errResult(), out, nil
	}
	if in.LeftPath == "" || in.RightPath == "" {
		return fail("INVALID_ARGUMENT", "left_path and right_path are required")
	}
	for _, t := range in.IgnoreTags {
		if err := meta.ValidateSelector(t); err != nil {
			return fail("INVALID_ARGUMENT", err.Error())
		}
	}
	include, exclude, _, ferr := filterSelectors(in.MetadataFilter)
	if ferr != nil {
		return fail("INVALID_ARGUMENT", ferr.Error())
	}
	spec := in.ReadOptions.Spec(include, exclude)
	// Diffs pair duplicate copies by value; without family 4,
	// ExifTool's JSON silently drops same-group duplicates and
	// the diff would compare a subset of the values. Compare
	// therefore always reads duplicates, like search.
	spec.IncludeDuplicates = true
	if err := spec.Validate(); err != nil {
		return fail("INVALID_ARGUMENT", err.Error())
	}
	if err := guardFastLevel(spec); err != nil {
		return fail("INVALID_ARGUMENT", err.Error())
	}
	leftAbs := absOf(in.LeftPath)
	rightAbs := absOf(in.RightPath)
	results, err := a.Reader.Read(ctx, []string{leftAbs, rightAbs}, spec)
	if err != nil {
		return fail(errorCode(err), err.Error())
	}
	if len(results) != 2 {
		return fail("EXTRACTION_FAILED", "unexpected read result count")
	}
	left, right := results[0], results[1]
	if left.Status == "error" || right.Status == "error" {
		// No fake diff: if either side failed, report and stop.
		out.Status = "failed"
		if left.Status == "error" {
			out.Errors = append(out.Errors, ToolError{Code: "EXTRACTION_FAILED", Message: left.Error, Path: left.Path})
		}
		if right.Status == "error" {
			out.Errors = append(out.Errors, ToolError{Code: "EXTRACTION_FAILED", Message: right.Error, Path: right.Path})
		}
		return errResult(), out, nil
	}
	ignored := func(e meta.Entry) bool {
		for _, sel := range in.IgnoreTags {
			if selectorMatches(e, sel) {
				return true
			}
		}
		return false
	}
	groupByIdentity := func(entries []meta.Entry) map[string][]meta.Entry {
		m := make(map[string][]meta.Entry)
		for _, e := range entries {
			if ignored(e) {
				continue
			}
			k := identityOf(e)
			m[k] = append(m[k], e)
		}
		return m
	}
	lm := groupByIdentity(left.Entries)
	rm := groupByIdentity(right.Entries)
	identities := make(map[string]bool)
	for k := range lm {
		identities[k] = true
	}
	for k := range rm {
		identities[k] = true
	}
	var changes []ChangeRecord
	for id := range identities {
		ls, rs := lm[id], rm[id]
		ambiguous := len(ls) > 1 || len(rs) > 1
		changes = append(changes, pairBucket(ls, rs, ambiguous)...)
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Key != changes[j].Key {
			return changes[i].Key < changes[j].Key
		}
		if changes[i].Copy != changes[j].Copy {
			return changes[i].Copy < changes[j].Copy
		}
		return changes[i].Change < changes[j].Change
	})
	// The returned set is decided BEFORE paging: when unchanged
	// records are not requested they must not consume page capacity
	// or leak back through get_result_page. UnchangedCount below is
	// still computed over the full comparison.
	returned := changes
	if !in.IncludeUnchanged {
		returned = nil
		for _, c := range changes {
			if c.Change != "unchanged" {
				returned = append(returned, c)
			}
		}
	}
	// Externalize oversized side values before pagination: a
	// single change record was previously returned whole, so one
	// large value silently blew through max_response_bytes (a
	// ~160 KB value against a 1024-byte budget). With the value
	// in the result store as a fragment, the record itself is
	// small and the budget binds again.
	var madeRefs []string
	for i := range returned {
		for _, side := range []struct {
			val *any
			ref **meta.ValueRef
		}{
			{&returned[i].LeftValue, &returned[i].LeftValueRef},
			{&returned[i].RightValue, &returned[i].RightValueRef},
		} {
			if *side.val == nil {
				continue
			}
			e := meta.Entry{Value: *side.val}
			if _, err := a.externalizeEntryValue(&e); err != nil {
				return fail(storeErrorCode(err), err.Error())
			}
			if e.ValueRef != nil {
				*side.val = nil
				*side.ref = e.ValueRef
				madeRefs = append(madeRefs, e.ValueRef.ResultID)
			}
		}
	}
	if err := a.verifyValueRefs(madeRefs); err != nil {
		return fail(storeErrorCode(err), err.Error())
	}
	pageN, records, err := pageByBudget(returned, a.pageSize(in.ResponseOptions), a.maxBytes(in.ResponseOptions))
	if err != nil {
		return fail("EXTRACTION_FAILED", err.Error())
	}
	if pageN > 0 && len(records[0]) > a.maxBytes(in.ResponseOptions) {
		// Still too large after externalization: paging cannot
		// split a record, so fail explicitly instead of returning
		// a response over the caller's budget.
		return fail("INVALID_ARGUMENT", fmt.Sprintf(
			"one change record is %d bytes, over max_response_bytes %d even after oversized values are externalized; raise max_response_bytes or narrow the comparison",
			len(records[0]), a.maxBytes(in.ResponseOptions)))
	}
	page := returned[:pageN]
	if pageN < len(returned) {
		m, err := a.Store.SaveRecords(records)
		if err != nil {
			return fail(storeErrorCode(err), err.Error())
		}
		out.ResultID = m.ID
		out.NextCursor = encodeCursor(pageN)
		out.HasMore = true
		out.Complete = false
		out.ExpiresAt = m.ExpiresAt.Format(time.RFC3339)
	}
	for _, c := range changes {
		if c.Change == "unchanged" {
			out.UnchangedCount++
		}
	}
	for _, c := range page {
		switch c.Change {
		case "added":
			out.Added = append(out.Added, c)
		case "removed":
			out.Removed = append(out.Removed, c)
		case "changed":
			out.Changed = append(out.Changed, c)
		case "unchanged":
			if in.IncludeUnchanged {
				out.Unchanged = append(out.Unchanged, c)
			}
		}
	}
	out.EffectiveOptions = spec.Describe()
	return okResult(false), out, nil
}

func absOf(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}
