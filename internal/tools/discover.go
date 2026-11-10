package tools

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alfa-reza/exiftool-mcp-go/internal/meta"
)

// groupSegmentMatch reports whether sel names the full group or any
// family segment of it.
func groupSegmentMatch(group, sel string) bool {
	if group == sel {
		return true
	}
	for _, seg := range strings.Split(group, ":") {
		if seg == sel {
			return true
		}
	}
	return false
}

// --- discover_tags ---

type DiscoverTagsIn struct {
	Scope      string   `json:"scope"` // catalog | present_in_files
	Query      string   `json:"query,omitempty"`
	Group      string   `json:"group,omitempty"`
	Paths      []string `json:"paths,omitempty"`
	Recursive  bool     `json:"recursive,omitempty"`
	Extensions []string `json:"extensions,omitempty"`
	PageSize   int      `json:"page_size,omitempty"`
}

// TagInfo is one discovered tag.
type TagInfo struct {
	Name        string `json:"name"`
	Group       string `json:"group,omitempty"`
	Table       string `json:"table,omitempty"`
	Type        string `json:"type,omitempty"`
	Writable    bool   `json:"writable,omitempty"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source"` // catalog | observed
	Count       int    `json:"count,omitempty"`
}

type DiscoverTagsOut struct {
	Status       string      `json:"status"` // complete | partial | failed
	Items        []TagInfo   `json:"items"`
	TotalCount   int         `json:"total_count"`
	ScanComplete bool        `json:"scan_complete"`
	Errors       []ToolError `json:"errors,omitempty"`
	ResultID     string      `json:"result_id,omitempty"`
	NextCursor   string      `json:"next_cursor,omitempty"`
	HasMore      bool        `json:"has_more"`
	ExpiresAt    string      `json:"expires_at,omitempty"`
	Complete     bool        `json:"complete"`
}

func (a *App) discoverTags(ctx context.Context, _ *mcp.CallToolRequest, in DiscoverTagsIn) (*mcp.CallToolResult, DiscoverTagsOut, error) {
	out := DiscoverTagsOut{Status: "complete", Complete: true, ScanComplete: true}
	fail := func(code, msg string) (*mcp.CallToolResult, DiscoverTagsOut, error) {
		out.Status = "failed"
		out.Errors = append(out.Errors, ToolError{Code: code, Message: msg})
		return errResult(), out, nil
	}
	pageSize := in.PageSize
	if pageSize <= 0 {
		pageSize = a.Cfg.DefaultPageSize
	}
	var items []TagInfo
	switch in.Scope {
	case "catalog":
		cat, err := a.catalog(ctx)
		if err != nil {
			return fail(errorCode(err), err.Error())
		}
		for _, d := range cat.Search(in.Query, in.Group) {
			items = append(items, TagInfo{
				Name: d.Name, Group: d.Group, Table: d.Table, Type: d.Type,
				Writable: d.Writable, Description: d.Description, Source: "catalog",
			})
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].Group != items[j].Group {
				return items[i].Group < items[j].Group
			}
			return items[i].Name < items[j].Name
		})
	case "present_in_files":
		if len(in.Paths) == 0 {
			return fail("INVALID_ARGUMENT", "paths is required for scope=present_in_files")
		}
		candidates, enumErrs := meta.Enumerate(in.Paths, in.Recursive, in.Extensions)
		for _, e := range enumErrs {
			out.Errors = append(out.Errors, ToolError{Code: e.Code, Message: e.Message, Path: e.Path})
		}
		files := make([]string, len(candidates))
		for i, c := range candidates {
			files[i] = c.Path
		}
		counts := make(map[string]*TagInfo)
		// Aggregate chunk by chunk: memory stays bounded by one
		// chunk of reads plus the tag table, never by the whole
		// collection's metadata at once.
		for start := 0; start < len(files); start += readChunkFiles {
			chunk := files[start:min(start+readChunkFiles, len(files))]
			results, err := a.Reader.Read(ctx, chunk, meta.ReadSpec{})
			if err != nil {
				return fail(errorCode(err), err.Error())
			}
			for _, fr := range results {
				if fr.Status == "error" {
					out.ScanComplete = false
					out.Errors = append(out.Errors, ToolError{Code: "EXTRACTION_FAILED", Message: fr.Error, Path: fr.Path})
					continue
				}
				seenInFile := make(map[string]bool)
				for _, e := range fr.Entries {
					k := e.Group + "\x00" + e.Name
					if seenInFile[k] {
						continue
					}
					seenInFile[k] = true
					ti, ok := counts[k]
					if !ok {
						ti = &TagInfo{Name: e.Name, Group: e.Group, Source: "observed"}
						counts[k] = ti
					}
					ti.Count++
				}
			}
		}
		q := strings.ToLower(in.Query)
		for _, ti := range counts {
			if in.Group != "" && !groupSegmentMatch(ti.Group, in.Group) {
				continue
			}
			if q != "" && !strings.Contains(strings.ToLower(ti.Name), q) {
				continue
			}
			items = append(items, *ti)
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].Count != items[j].Count {
				return items[i].Count > items[j].Count
			}
			return items[i].Name < items[j].Name
		})
		if !out.ScanComplete || len(out.Errors) > 0 {
			out.Status = "partial"
		}
	default:
		return fail("INVALID_ARGUMENT", "scope must be \"catalog\" or \"present_in_files\"")
	}
	out.TotalCount = len(items)
	pageN, records, err := pageByBudget(items, pageSize, a.Cfg.MaxResponseBytes)
	if err != nil {
		return fail("EXTRACTION_FAILED", err.Error())
	}
	out.Items = items[:pageN]
	if out.Items == nil {
		out.Items = []TagInfo{}
	}
	if pageN < len(items) {
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
	return okResult(false), out, nil
}
