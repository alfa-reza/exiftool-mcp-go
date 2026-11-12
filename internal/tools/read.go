package tools

import (
	"context"
	"encoding/json"
	"fmt"
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

// externalizeEntryValue moves an oversized entry value into the
// store as a text fragment and replaces it with a value_ref, so a
// response never carries a huge value inline. It reports whether
// the value was externalized. The fragment is created PENDING
// (incomplete): cleanup never evicts incomplete results, so the
// fragment survives every subsequent write of the owning call —
// including the commit of the records that reference it — until
// finalizeValueRefs completes it after that commit.
// finalizeValueRefs verifies that every fragment created during
// the current call still exists, then marks each one complete.
// A successful response must never carry a reference that
// already reads back as not found, so a missing fragment is a
// store-full failure, not a response.
func (a *App) finalizeValueRefs(ids []string) error {
	unique := make([]string, 0, len(ids))
	seen := make(map[string]bool)
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
		if _, err := a.Store.LoadMeta(id); err != nil {
			return fmt.Errorf("%w: a value reference created during this call no longer exists (%v); the result store quota is too small for the oversized values in one call",
				store.ErrFull, err)
		}
	}
	for _, id := range unique {
		if err := a.Store.MarkComplete(id); err != nil {
			return err
		}
	}
	return nil
}

// discardRefs removes pending fragments a failed call created
// and never committed references to. Errors are ignored on
// purpose: the call is already failing, and a fragment that
// cannot be removed is still bounded by its TTL.
func (a *App) discardRefs(ids []string) {
	for _, id := range ids {
		_ = a.Store.DiscardResult(id)
	}
}

func (a *App) externalizeEntryValue(e *meta.Entry) (bool, error) {
	raw, err := json.Marshal(e.Value)
	if err != nil || len(raw) <= largeValueThreshold {
		return false, nil
	}
	text := string(raw)
	if s, ok := e.Value.(string); ok {
		text = s
	}
	m, err := a.Store.SaveTextPending(text)
	if err != nil {
		return false, err
	}
	e.ValueRef = &meta.ValueRef{ResultID: m.ID, Kind: string(store.KindText), Size: int64(len(text))}
	e.Value = nil
	e.ValueOmitted = true
	return true, nil
}

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
	include, exclude, filterMode, ferr := filterSelectors(in.MetadataFilter)
	if ferr != nil {
		out.Status = "failed"
		out.Errors = append(out.Errors, ToolError{Code: "INVALID_ARGUMENT", Message: ferr.Error()})
		return errResult(), out, nil
	}
	spec := in.ReadOptions.Spec(include, exclude)
	if err := spec.Validate(); err != nil {
		out.Status = "failed"
		out.Errors = append(out.Errors, ToolError{Code: "INVALID_ARGUMENT", Message: err.Error()})
		return errResult(), out, nil
	}
	candidates, enumErrs, err := meta.Enumerate(ctx, in.Paths, in.Recursive, in.Extensions)
	if err != nil {
		out.Status = "failed"
		out.Errors = append(out.Errors, ToolError{Code: errorCode(err), Message: err.Error()})
		return errResult(), out, nil
	}
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
	var madeRefs []string
	finalized := false
	processed, errorCount := 0, 0
	failWith := func(code, msg string) (*mcp.CallToolResult, ReadMetadataOut, error) {
		// A failed call discards the pending fragments it
		// created: nothing the agent can reach references
		// them (this call's stored result is completed only
		// on the success path), and as incomplete results
		// they would otherwise occupy quota until their TTL.
		// After finalization the fragments are complete and
		// referenced by committed records, so they stay.
		if !finalized {
			a.discardRefs(madeRefs)
		}
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
				if _, err := a.externalizeEntryValue(&results[i].Entries[j]); err != nil {
					// Never leave the oversized value inline: the
					// response would silently exceed its byte
					// budget. Failing loudly beats a wrong reply.
					return failWith(storeErrorCode(err), err.Error())
				}
				if ref := results[i].Entries[j].ValueRef; ref != nil {
					madeRefs = append(madeRefs, ref.ResultID)
				}
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
			if len(pageItems) == 0 && len(records[take]) > byteBudget {
				// One file's record can never fit the byte budget,
				// even after oversized values are externalized, and
				// paging cannot split a record: fail explicitly
				// instead of returning a response that silently
				// exceeds the budget the caller set.
				return failWith("INVALID_ARGUMENT", fmt.Sprintf(
					"metadata for one file is %d bytes, over max_response_bytes %d even after oversized values are externalized; raise max_response_bytes or narrow the read",
					len(records[take]), byteBudget))
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
	if err := a.finalizeValueRefs(madeRefs); err != nil {
		return failWith(storeErrorCode(err), err.Error())
	}
	finalized = true
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
		if err := a.Store.MarkComplete(stored.ID); err != nil {
			return failWith(storeErrorCode(err), err.Error())
		}
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
