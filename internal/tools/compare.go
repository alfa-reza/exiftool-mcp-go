package tools

import (
	"context"
	"encoding/json"
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
	Change     string `json:"change"` // added | removed | changed | unchanged
	Key        string `json:"key"`
	Group      string `json:"group,omitempty"`
	Name       string `json:"name"`
	Document   string `json:"document,omitempty"`
	Instance   string `json:"instance,omitempty"`
	LeftValue  any    `json:"left_value,omitempty"`
	RightValue any    `json:"right_value,omitempty"`
	Ambiguous  bool   `json:"ambiguous,omitempty"`
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
	var include, exclude []string
	if in.MetadataFilter != nil && in.MetadataFilter.Mode == "select" {
		include, exclude = in.MetadataFilter.IncludeTags, in.MetadataFilter.ExcludeTags
	}
	spec := in.ReadOptions.Spec(include, exclude)
	if err := spec.Validate(); err != nil {
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
		n := max(len(ls), len(rs))
		ambiguous := len(ls) > 1 || len(rs) > 1
		for i := 0; i < n; i++ {
			var rec ChangeRecord
			var sample meta.Entry
			if i < len(ls) {
				sample = ls[i]
			} else {
				sample = rs[i]
			}
			rec.Key, rec.Group, rec.Name = sample.Key, sample.Group, sample.Name
			rec.Document, rec.Instance = sample.Document, sample.Instance
			rec.Ambiguous = ambiguous
			switch {
			case i >= len(ls):
				rec.Change = "added"
				rec.RightValue = rs[i].Value
			case i >= len(rs):
				rec.Change = "removed"
				rec.LeftValue = ls[i].Value
			default:
				lb, _ := json.Marshal(ls[i].Value)
				rb, _ := json.Marshal(rs[i].Value)
				if string(lb) == string(rb) {
					rec.Change = "unchanged"
					rec.LeftValue = ls[i].Value
					rec.RightValue = rs[i].Value
				} else {
					rec.Change = "changed"
					rec.LeftValue = ls[i].Value
					rec.RightValue = rs[i].Value
				}
			}
			changes = append(changes, rec)
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Key != changes[j].Key {
			return changes[i].Key < changes[j].Key
		}
		return changes[i].Change < changes[j].Change
	})
	// Page over the change list; store the full list when it overflows.
	pageN, records, err := pageByBudget(changes, a.pageSize(in.ResponseOptions), a.maxBytes(in.ResponseOptions))
	if err != nil {
		return fail("EXTRACTION_FAILED", err.Error())
	}
	page := changes[:pageN]
	if pageN < len(changes) {
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
