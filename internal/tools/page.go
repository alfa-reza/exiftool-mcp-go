package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	Status       string            `json:"status"` // complete | failed
	Kind         string            `json:"kind,omitempty"`
	Items        []json.RawMessage `json:"items,omitempty"`
	Text         string            `json:"text,omitempty"`
	Data         string            `json:"data,omitempty"` // base64 for binary fragments
	Offset       int64             `json:"offset,omitempty"`
	TotalRecords int               `json:"total_records,omitempty"`
	TotalBytes   int64             `json:"total_bytes,omitempty"`
	MIME         string            `json:"mime,omitempty"`
	SHA256       string            `json:"sha256,omitempty"`
	NextCursor   string            `json:"next_cursor,omitempty"`
	HasMore      bool              `json:"has_more"`
	Complete     bool              `json:"complete"`
	ExpiresAt    string            `json:"expires_at,omitempty"`
	Errors       []ToolError       `json:"errors,omitempty"`
}

func (a *App) getResultPage(ctx context.Context, _ *mcp.CallToolRequest, in GetResultPageIn) (*mcp.CallToolResult, GetResultPageOut, error) {
	out := GetResultPageOut{Status: "complete"}
	fail := func(err error) (*mcp.CallToolResult, GetResultPageOut, error) {
		out.Status = "failed"
		out.Errors = append(out.Errors, ToolError{Code: storeErrorCode(err), Message: err.Error()})
		return errResult(), out, nil
	}
	if in.ResultID == "" {
		return fail(errors.New("result_id is required"))
	}
	offset, err := decodeCursor(in.Cursor)
	if err != nil {
		return fail(err)
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
		out.Items = page.Records
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
