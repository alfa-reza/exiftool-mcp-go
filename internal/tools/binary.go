package tools

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alfa-reza/exiftool-mcp-go/internal/meta"
	"github.com/alfa-reza/exiftool-mcp-go/internal/runner"
)

// --- extract_binary ---

type ExtractBinaryIn struct {
	Path           string `json:"path"`
	Tag            string `json:"tag"`
	Delivery       string `json:"delivery,omitempty"` // auto | inline | reference
	MaxInlineBytes int    `json:"max_inline_bytes,omitempty"`
}

type ExtractBinaryOut struct {
	Status    string      `json:"status"` // complete | failed
	Path      string      `json:"path,omitempty"`
	Tag       string      `json:"tag,omitempty"`
	SizeBytes int64       `json:"size_bytes,omitempty"`
	SHA256    string      `json:"sha256,omitempty"`
	MIME      string      `json:"mime,omitempty"`
	Delivery  string      `json:"delivery,omitempty"` // image | inline | reference
	Data      string      `json:"data,omitempty"`     // base64, inline delivery only
	ResultID  string      `json:"result_id,omitempty"`
	ExpiresAt string      `json:"expires_at,omitempty"`
	Errors    []ToolError `json:"errors,omitempty"`
}

func (a *App) extractBinary(ctx context.Context, _ *mcp.CallToolRequest, in ExtractBinaryIn) (*mcp.CallToolResult, ExtractBinaryOut, error) {
	out := ExtractBinaryOut{Status: "complete", Tag: in.Tag, Path: absOf(in.Path)}
	fail := func(code, msg string) (*mcp.CallToolResult, ExtractBinaryOut, error) {
		out.Status = "failed"
		out.Errors = append(out.Errors, ToolError{Code: code, Message: msg})
		return errResult(), out, nil
	}
	if in.Path == "" {
		return fail("INVALID_ARGUMENT", "path is required")
	}
	// One specific tag only: no wildcards, no group-only selectors.
	if err := meta.ValidateSelector(in.Tag); err != nil {
		return fail("INVALID_ARGUMENT", err.Error())
	}
	name := in.Tag
	if i := strings.Index(in.Tag, ":"); i >= 0 {
		name = in.Tag[i+1:]
	}
	if strings.EqualFold(name, "All") || name == "*" {
		return fail("INVALID_ARGUMENT", "tag must name one specific tag")
	}
	op, err := runner.Operand(out.Path)
	if err != nil {
		return fail("INVALID_ARGUMENT", err.Error())
	}
	res, err := a.Runner.Run(ctx, "-b", "-"+in.Tag, op)
	if err != nil {
		return fail(errorCode(err), err.Error())
	}
	if res.ExitCode != 0 {
		// A nonzero exit means the extraction failed; whatever
		// partial bytes reached stdout are not the tag's data and
		// must not be delivered as a complete extraction with a
		// size and checksum.
		return fail("EXTRACTION_FAILED", strings.TrimSpace(res.Stderr))
	}
	if len(res.Stdout) == 0 {
		return fail("BINARY_NOT_FOUND", "tag "+in.Tag+" has no extractable binary data in this file")
	}
	data := res.Stdout
	sum := sha256.Sum256(data)
	out.SizeBytes = int64(len(data))
	out.SHA256 = hex.EncodeToString(sum[:])
	out.MIME = http.DetectContentType(data)
	inlineLimit := a.Cfg.MaxInlineBinary
	if in.MaxInlineBytes > 0 && in.MaxInlineBytes < inlineLimit {
		inlineLimit = in.MaxInlineBytes
	}
	delivery := in.Delivery
	if delivery == "" {
		delivery = "auto"
	}
	isImage := strings.HasPrefix(out.MIME, "image/")
	switch {
	case delivery == "reference" || int64(len(data)) > int64(inlineLimit):
		if delivery == "inline" {
			return fail("BINARY_TOO_LARGE", "binary exceeds the inline limit; use delivery=reference")
		}
		m, err := a.Store.SaveBlob(data, out.MIME)
		if err != nil {
			return fail(storeErrorCode(err), err.Error())
		}
		out.Delivery = "reference"
		out.ResultID = m.ID
		out.ExpiresAt = m.ExpiresAt.Format(time.RFC3339)
		return okResult(false), out, nil
	case isImage:
		out.Delivery = "image"
		result := &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.ImageContent{Data: data, MIMEType: out.MIME},
				&mcp.TextContent{Text: "extracted " + in.Tag + " from " + out.Path},
			},
		}
		return result, out, nil
	default:
		out.Delivery = "inline"
		out.Data = base64.StdEncoding.EncodeToString(data)
		return okResult(false), out, nil
	}
}
