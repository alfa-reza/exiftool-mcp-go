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

// readChunkFiles bounds how many files one streaming step of
// read_metadata processes (the reader itself invokes ExifTool in
// smaller chunks).
const readChunkFiles = 200

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
	// Files are processed in chunks and the response page is filled
	// incrementally; results beyond the page are appended to the
	// result store chunk by chunk. A read of a huge directory never
	// holds more than one chunk plus the first page in memory.
	pageLimit := a.pageSize(in.ResponseOptions)
	byteBudget := a.maxBytes(in.ResponseOptions)
	var pageItems []meta.FileResult
	var pageRecords []json.RawMessage
	pageBytes := 0
	var stored *store.ResultMeta
	processed, errorCount := 0, 0
	failWith := func(code, msg string) (*mcp.CallToolResult, ReadMetadataOut, error) {
		out.Status = "failed"
		out.Errors = append(out.Errors, ToolError{Code: code, Message: msg})
		return errResult(), out, nil
	}
	for start := 0; start < len(files); start += readChunkFiles {
		chunk := files[start:min(start+readChunkFiles, len(files))]
		results, err := a.Reader.Read(ctx, chunk, spec)
		if err != nil {
			return failWith(errorCode(err), err.Error())
		}
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
		processed += len(results)
		for _, r := range results {
			if r.Status == "error" {
				errorCount++
			}
		}
		records, err := marshalRecords(results)
		if err != nil {
			return failWith("EXTRACTION_FAILED", err.Error())
		}
		if stored != nil {
			if _, err := a.Store.AppendRecords(stored.ID, records); err != nil {
				return failWith(storeErrorCode(err), err.Error())
			}
			continue
		}
		// Fill the first page from this chunk; the first record that
		// does not fit starts the stored overflow.
		take := 0
		for take < len(results) {
			if len(pageItems) >= pageLimit {
				break
			}
			if len(pageItems) > 0 && pageBytes+len(records[take]) > byteBudget {
				break
			}
			pageItems = append(pageItems, results[take])
			pageRecords = append(pageRecords, records[take])
			pageBytes += len(records[take])
			take++
		}
		if take < len(results) {
			// Overflow: store the complete result set, page prefix
			// first, so cursor offsets line up with the whole set.
			m, err := a.Store.CreateResult(store.KindRecords)
			if err != nil {
				return failWith(storeErrorCode(err), err.Error())
			}
			stored = m
			all := append(pageRecords, records[take:]...)
			if _, err := a.Store.AppendRecords(stored.ID, all); err != nil {
				return failWith(storeErrorCode(err), err.Error())
			}
		}
	}
	out.ProcessedCount = processed
	if processed == 0 || (errorCount == processed && processed > 0) {
		if len(enumErrs) > 0 || errorCount > 0 {
			out.Status = "failed"
		}
	} else if errorCount > 0 || len(enumErrs) > 0 {
		out.Status = "partial"
	}
	out.Items = pageItems
	if stored != nil {
		_ = a.Store.MarkComplete(stored.ID)
		out.ResultID = stored.ID
		out.NextCursor = encodeCursor(len(pageItems))
		out.HasMore = true
		out.Complete = false
		out.ExpiresAt = stored.ExpiresAt.Format(time.RFC3339)
	}
	eff := spec.Describe()
	eff["filter_mode"] = filterMode
	out.EffectiveOptions = eff
	return okResult(out.Status == "failed"), out, nil
}
