package tools

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alfa-reza/exiftool-mcp-go/internal/runner"
)

// --- get_exiftool_info ---

type ExiftoolInfoIn struct {
	Format         string `json:"format,omitempty"`          // file extension to check, e.g. "jpg"
	IncludeFormats bool   `json:"include_formats,omitempty"` // include readable/supported lists
	IncludeGroups  bool   `json:"include_groups,omitempty"`  // include family-0 group list
}

type ExiftoolInfoOut struct {
	Status           string         `json:"status"` // ok | unavailable
	Executable       string         `json:"executable,omitempty"`
	Version          string         `json:"version,omitempty"`
	ConfigMode       string         `json:"config_mode"`
	FormatResult     string         `json:"format_result,omitempty"` // readable | listed | not_listed
	ReadableFormats  []string       `json:"readable_formats,omitempty"`
	SupportedFormats []string       `json:"supported_formats,omitempty"`
	Groups           []string       `json:"groups,omitempty"`
	Limits           map[string]any `json:"limits"`
	Errors           []ToolError    `json:"errors,omitempty"`
}

func (a *App) getExiftoolInfo(ctx context.Context, _ *mcp.CallToolRequest, in ExiftoolInfoIn) (*mcp.CallToolResult, ExiftoolInfoOut, error) {
	out := ExiftoolInfoOut{
		Status:     "ok",
		ConfigMode: a.Runner.ConfigMode(),
		Executable: a.Runner.Path(),
		Limits: map[string]any{
			"max_response_bytes": a.Cfg.MaxResponseBytes,
			"default_page_size":  a.Cfg.DefaultPageSize,
			"max_inline_binary":  a.Cfg.MaxInlineBinary,
			"call_timeout_sec":   int(a.Cfg.CallTimeout.Seconds()),
			"result_ttl_hours":   int(a.Cfg.ResultTTL.Hours()),
			"scan_batch_files":   a.Cfg.ScanBatchFiles,
		},
	}
	version, err := runner.Version(ctx, a.Runner)
	if err != nil {
		out.Status = "unavailable"
		out.Errors = append(out.Errors, ToolError{Code: "EXIFTOOL_NOT_FOUND", Message: err.Error()})
		return errResult(), out, nil
	}
	out.Version = version
	if in.Format != "" || in.IncludeFormats || in.IncludeGroups {
		cat, err := a.catalog(ctx)
		if err != nil {
			// The status field must agree with the error: a
			// failed catalog load is "unavailable", never "ok"
			// alongside a tool error.
			out.Status = "unavailable"
			out.Errors = append(out.Errors, ToolError{Code: "EXTRACTION_FAILED", Message: err.Error()})
			return errResult(), out, nil
		}
		if in.Format != "" {
			ext := strings.ToUpper(strings.TrimPrefix(strings.TrimSpace(in.Format), "."))
			switch {
			case contains(cat.Readable, ext):
				out.FormatResult = "readable"
			case contains(cat.Supported, ext):
				out.FormatResult = "listed"
			default:
				out.FormatResult = "not_listed"
			}
		}
		if in.IncludeFormats {
			out.ReadableFormats = cat.Readable
			out.SupportedFormats = cat.Supported
		}
		if in.IncludeGroups {
			out.Groups = cat.Groups
		}
	}
	return okResult(false), out, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
