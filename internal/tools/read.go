package tools

import (
	"context"
	"encoding/json"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alfa-reza/exiftool-mcp-go/internal/meta"
	"github.com/alfa-reza/exiftool-mcp-go/internal/store"
)

// --- read_metadata ---

// MetadataFilter selects tags for a read. Mode "none" (default) applies
// no server-side tag restriction; "select" passes include/exclude
// selectors to ExifTool.
type MetadataFilter struct {
	Mode        string   `json:"mode,omitempty"`
	IncludeTags []string `json:"include_tags,omitempty"`
	ExcludeTags []string `json:"exclude_tags,omitempty"`
}

type ReadMetadataIn struct {
	Paths           []string              `json:"paths"`
	Recursive       bool                  `json:"recursive,omitempty"`
	Extensions      []string              `json:"extensions,omitempty"`
	MetadataFilter  *MetadataFilter       `json:"metadata_filter,omitempty"`
	ReadOptions     *ReadOptionsInput     `json:"read_options,omitempty"`
	ResponseOptions *ResponseOptionsInput `json:"response_options,omitempty"`
}

type ReadMetadataOut struct {
	Status           string            `json:"status"` // complete | partial | failed
	Items            []meta.FileResult `json:"items"`
	ProcessedCount   int               `json:"processed_count"`
	Errors           []ToolError       `json:"errors,omitempty"`
	EffectiveOptions map[string]any    `json:"effective_options,omitempty"`
	ResultID         string            `json:"result_id,omitempty"`
	NextCursor       string            `json:"next_cursor,omitempty"`
	HasMore          bool              `json:"has_more"`
	ExpiresAt        string            `json:"expires_at,omitempty"`
	Complete         bool              `json:"complete"`
}

// largeValueThreshold moves oversized single values into the store as
// text fragments instead of inlining them into the response.
const largeValueThreshold = 32 << 10

func (a *App) readMetadata(ctx context.Context, _ *mcp.CallToolRequest, in ReadMetadataIn) (*mcp.CallToolResult, ReadMetadataOut, error) {
	out := ReadMetadataOut{Status: "complete", Complete: true}
	if len(in.Paths) == 0 {
		out.Status = "failed"
		out.Errors = append(out.Errors, ToolError{Code: "INVALID_ARGUMENT", Message: "paths must contain at least one path"})
		return errResult(), out, nil
	}
	var include, exclude []string
	filterMode := "none"
	if in.MetadataFilter != nil && in.MetadataFilter.Mode == "select" {
		filterMode = "select"
		include = in.MetadataFilter.IncludeTags
		exclude = in.MetadataFilter.ExcludeTags
	}
	spec := in.ReadOptions.Spec(include, exclude)
	if err := spec.Validate(); err != nil {
		out.Status = "failed"
		out.Errors = append(out.Errors, ToolError{Code: "INVALID_ARGUMENT", Message: err.Error()})
		return errResult(), out, nil
	}
	candidates, enumErrs := meta.Enumerate(in.Paths, in.Recursive, in.Extensions)
	for _, e := range enumErrs {
		out.Errors = append(out.Errors, ToolError{Code: e.Code, Message: e.Message, Path: e.Path})
	}
	files := make([]string, len(candidates))
	for i, c := range candidates {
		files[i] = c.Path
	}
	var results []meta.FileResult
	if len(files) > 0 {
		var err error
		results, err = a.Reader.Read(ctx, files, spec)
		if err != nil {
			out.Status = "failed"
			out.Errors = append(out.Errors, ToolError{Code: errorCode(err), Message: err.Error()})
			return errResult(), out, nil
		}
	}
	out.ProcessedCount = len(results)
	// Move oversized values into the store as text fragments.
	for i := range results {
		for j := range results[i].Entries {
			e := &results[i].Entries[j]
			raw, err := json.Marshal(e.Value)
			if err != nil || len(raw) <= largeValueThreshold {
				continue
			}
			text := string(raw)
			if s, ok := e.Value.(string); ok {
				text = s
			}
			m, err := a.Store.SaveText(text)
			if err != nil {
				continue // leave value inline rather than dropping it
			}
			e.ValueRef = &meta.ValueRef{ResultID: m.ID, Kind: string(store.KindText), Size: int64(len(text))}
			e.Value = nil
			e.ValueOmitted = true
		}
	}
	errorCount := 0
	for _, r := range results {
		if r.Status == "error" {
			errorCount++
		}
	}
	if len(results) == 0 || errorCount == len(results) && len(results) > 0 {
		if len(enumErrs) > 0 || errorCount > 0 {
			out.Status = "failed"
		}
	} else if errorCount > 0 || len(enumErrs) > 0 {
		out.Status = "partial"
	}
	// Page the results; overflow goes to the result store.
	pageN, records, err := pageByBudget(results, a.pageSize(in.ResponseOptions), a.maxBytes(in.ResponseOptions))
	if err != nil {
		out.Status = "failed"
		out.Errors = append(out.Errors, ToolError{Code: "EXTRACTION_FAILED", Message: err.Error()})
		return errResult(), out, nil
	}
	out.Items = results[:pageN]
	if pageN < len(results) {
		m, err := a.Store.SaveRecords(records)
		if err != nil {
			out.Status = "failed"
			out.Errors = append(out.Errors, ToolError{Code: storeErrorCode(err), Message: err.Error()})
			return errResult(), out, nil
		}
		out.ResultID = m.ID
		out.NextCursor = encodeCursor(pageN)
		out.HasMore = true
		out.Complete = false
		out.ExpiresAt = m.ExpiresAt.Format(time.RFC3339)
	}
	eff := spec.Describe()
	eff["filter_mode"] = filterMode
	out.EffectiveOptions = eff
	return okResult(out.Status == "failed"), out, nil
}
