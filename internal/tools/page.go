package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alfa-reza/exiftool-mcp-go/internal/runner"
	"github.com/alfa-reza/exiftool-mcp-go/internal/store"
)

// errorCode maps a core error to a stable tool error code.
func errorCode(err error) string {
	switch {
	case errors.Is(err, runner.ErrNotFound):
		return "EXIFTOOL_NOT_FOUND"
	case errors.Is(err, context.DeadlineExceeded):
		return "TIMEOUT"
	case errors.Is(err, context.Canceled):
		return "CANCELLED"
	default:
		return "EXTRACTION_FAILED"
	}
}

// storeErrorCode maps a store error to a stable tool error code.
func storeErrorCode(err error) string {
	switch {
	case errors.Is(err, store.ErrExpired):
		return "RESULT_EXPIRED"
	case errors.Is(err, store.ErrNotFound):
		return "INVALID_CURSOR"
	case errors.Is(err, store.ErrCorrupt):
		return "CHECKPOINT_CORRUPT"
	case errors.Is(err, store.ErrFull):
		return "RESULT_STORE_FULL"
	case errors.Is(err, store.ErrRecordTooLarge):
		// The caller's byte limit cannot hold the record; the
		// remedy is a larger limit, which makes this an
		// argument problem, not a store failure.
		return "INVALID_ARGUMENT"
	default:
		return "EXTRACTION_FAILED"
	}
}

// --- get_result_page ---

type GetResultPageIn struct {
	ResultID string `json:"result_id"`
	Cursor   string `json:"cursor,omitempty"`
	PageSize int    `json:"page_size,omitempty"`
	MaxBytes int    `json:"max_bytes,omitempty"`
}

type GetResultPageOut struct {
	Status string `json:"status"` // complete | failed
	Kind   string `json:"kind,omitempty"`
	// Items holds the page's records as decoded JSON values.
	// []json.RawMessage cannot be used here: the SDK infers the
	// output schema from the Go type, RawMessage reflects as an
	// array of bytes, and every records response then fails
	// output validation over the real transport (an array of
	// objects where an array of arrays was advertised).
	Items        []any       `json:"items,omitempty"`
	Text         string      `json:"text,omitempty"`
	Data         string      `json:"data,omitempty"` // base64 for binary fragments
	Offset       int64       `json:"offset,omitempty"`
	TotalRecords int         `json:"total_records,omitempty"`
	TotalBytes   int64       `json:"total_bytes,omitempty"`
	MIME         string      `json:"mime,omitempty"`
	SHA256       string      `json:"sha256,omitempty"`
	NextCursor   string      `json:"next_cursor,omitempty"`
	HasMore      bool        `json:"has_more"`
	Complete     bool        `json:"complete"`
	ExpiresAt    string      `json:"expires_at,omitempty"`
	Errors       []ToolError `json:"errors,omitempty"`
}

func (a *App) getResultPage(ctx context.Context, _ *mcp.CallToolRequest, in GetResultPageIn) (*mcp.CallToolResult, GetResultPageOut, error) {
	out := GetResultPageOut{Status: "complete"}
	fail := func(err error) (*mcp.CallToolResult, GetResultPageOut, error) {
		out.Status = "failed"
		out.Errors = append(out.Errors, ToolError{Code: storeErrorCode(err), Message: err.Error()})
		return errResult(), out, nil
	}
	// Argument-shape problems are INVALID_ARGUMENT, not store
	// failures: routing them through storeErrorCode reported a
	// malformed cursor as EXTRACTION_FAILED.
	failArg := func(msg string) (*mcp.CallToolResult, GetResultPageOut, error) {
		out.Status = "failed"
		out.Errors = append(out.Errors, ToolError{Code: "INVALID_ARGUMENT", Message: msg})
		return errResult(), out, nil
	}
	if in.ResultID == "" {
		return failArg("result_id is required")
	}
	offset, err := decodeCursor(in.Cursor)
	if err != nil {
		return failArg(err.Error())
	}
	meta, err := a.Store.LoadMeta(in.ResultID)
	if err != nil {
		return fail(err)
	}
	out.Kind = string(meta.Kind)
	out.Complete = meta.Complete
	out.ExpiresAt = meta.ExpiresAt.Format(time.RFC3339)
	out.TotalRecords = meta.TotalRecords
	out.TotalBytes = meta.TotalBytes
	out.MIME = meta.MIME
	out.SHA256 = meta.SHA256
	switch meta.Kind {
	case store.KindRecords:
		size := in.PageSize
		if size <= 0 {
			size = a.Cfg.DefaultPageSize
		}
		if size > 1000 {
			size = 1000
		}
		maxB := in.MaxBytes
		if maxB <= 0 {
			maxB = 256 << 10
		}
		if maxB > 4<<20 {
			maxB = 4 << 20
		}
		_, page, err := a.Store.RecordsPage(in.ResultID, offset, size, maxB)
		if err != nil {
			return fail(err)
		}
		// Records are stored as raw JSON lines; decode them into
		// values for the response (UseNumber, so a big integer in
		// a stored record re-marshals as the same literal).
		items := make([]any, len(page.Records))
		for i, raw := range page.Records {
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			var v any
			if err := dec.Decode(&v); err != nil {
				return fail(fmt.Errorf("%w: stored record is not valid JSON: %v", store.ErrCorrupt, err))
			}
			items[i] = v
		}
		out.Items = items
		out.Offset = int64(offset)
		out.HasMore = page.HasMore
		if page.HasMore {
			out.NextCursor = encodeCursor(page.NextOffset)
		}
	case store.KindText, store.KindBinary:
		maxB := in.MaxBytes
		if maxB <= 0 {
			maxB = 256 << 10
		}
		if maxB > 4<<20 {
			maxB = 4 << 20
		}
		_, chunk, next, err := a.Store.Fragment(in.ResultID, int64(offset), maxB)
		if err != nil {
			return fail(err)
		}
		out.Offset = int64(offset)
		if meta.Kind == store.KindText {
			out.Text = string(chunk)
		} else {
			out.Data = base64.StdEncoding.EncodeToString(chunk)
		}
		out.HasMore = next < meta.TotalBytes
		if out.HasMore {
			out.NextCursor = encodeCursor(int(next))
		}
	}
	return okResult(false), out, nil
}
