// Package tools implements the seven MCP tools of exiftool-mcp-go.
// Handlers are thin adapters: they validate input, delegate to the
// internal core packages, and shape the structured output contract.
package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alfa-reza/exiftool-mcp-go/internal/catalog"
	"github.com/alfa-reza/exiftool-mcp-go/internal/config"
	"github.com/alfa-reza/exiftool-mcp-go/internal/meta"
	"github.com/alfa-reza/exiftool-mcp-go/internal/runner"
	"github.com/alfa-reza/exiftool-mcp-go/internal/store"
)

// App wires the shared dependencies used by all tool handlers.
type App struct {
	Cfg    config.Config
	Runner runner.Runner
	Reader *meta.Reader
	Store  *store.Store

	catMu  sync.Mutex
	cat    *catalog.Catalog
	catKey string

	// scanLocks holds one mutex per scan id so that only one batch of
	// a given scan runs at a time; a concurrent resume waits and then
	// continues from the updated checkpoint instead of reprocessing
	// the same batch and duplicating records.
	scanLocks sync.Map // scan id -> *sync.Mutex
}

// scanLock returns the mutex guarding the scan with the given id.
func (a *App) scanLock(id string) *sync.Mutex {
	m, _ := a.scanLocks.LoadOrStore(id, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// ToolError is a structured error entry in tool output.
type ToolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Path    string `json:"path,omitempty"`
}

// ReadOptionsInput is the typed read-options object shared by tools.
type ReadOptionsInput struct {
	IncludeDuplicates  bool `json:"include_duplicates,omitempty"`
	UnknownTags        int  `json:"unknown_tags,omitempty"`
	EmbeddedLevel      int  `json:"embedded_level,omitempty"`
	NumericValues      bool `json:"numeric_values,omitempty"`
	PreserveStructures bool `json:"preserve_structures,omitempty"`
	FastScanLevel      int  `json:"fast_scan_level,omitempty"`
}

// Spec converts input read options plus tag filters into a ReadSpec.
func (o *ReadOptionsInput) Spec(include, exclude []string) meta.ReadSpec {
	s := meta.ReadSpec{IncludeTags: include, ExcludeTags: exclude}
	if o != nil {
		s.IncludeDuplicates = o.IncludeDuplicates
		s.UnknownTags = o.UnknownTags
		s.EmbeddedLevel = o.EmbeddedLevel
		s.NumericValues = o.NumericValues
		s.PreserveStructures = o.PreserveStructures
		s.FastScanLevel = o.FastScanLevel
	}
	return s
}

// filterSelectors resolves a MetadataFilter into include/exclude
// selectors. An unrecognized mode is a caller error: silently
// ignoring a mistyped mode ("slect") would return unfiltered
// metadata the caller explicitly tried to restrict.
func filterSelectors(f *MetadataFilter) (include, exclude []string, mode string, err error) {
	if f == nil || f.Mode == "" || f.Mode == "none" {
		return nil, nil, "none", nil
	}
	if f.Mode == "select" {
		return f.IncludeTags, f.ExcludeTags, "select", nil
	}
	return nil, nil, "", fmt.Errorf("metadata_filter.mode %q is not supported (use \"none\" or \"select\")", f.Mode)
}

// guardFastLevel rejects read options whose extraction level cannot
// support conclusions. At fast_scan_level >= 3 ExifTool extracts no
// metadata at all (system tags only), so a query verdict or a diff
// computed over such a read would mistake "not read" for "not
// present". Plain reads keep the option: there the caller asked for
// the extraction itself, and the effective options disclose it.
func guardFastLevel(spec meta.ReadSpec) error {
	if spec.FastScanLevel >= 3 {
		return fmt.Errorf("fast_scan_level %d extracts no metadata and cannot support conclusions about tag presence or values", spec.FastScanLevel)
	}
	return nil
}

// ResponseOptionsInput controls paging of a tool response.
type ResponseOptionsInput struct {
	PageSize         int `json:"page_size,omitempty"`
	MaxResponseBytes int `json:"max_response_bytes,omitempty"`
}

func (a *App) pageSize(in *ResponseOptionsInput) int {
	if in != nil && in.PageSize > 0 {
		return min(in.PageSize, 10000)
	}
	return a.Cfg.DefaultPageSize
}

func (a *App) maxBytes(in *ResponseOptionsInput) int {
	if in != nil && in.MaxResponseBytes > 0 {
		return min(in.MaxResponseBytes, 16<<20)
	}
	return a.Cfg.MaxResponseBytes
}

func boolPtr(b bool) *bool { return &b }

func readOnlyTool(name, desc string) *mcp.Tool {
	return &mcp.Tool{
		Name:        name,
		Description: desc,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    true,
			DestructiveHint: boolPtr(false),
			IdempotentHint:  true,
			OpenWorldHint:   boolPtr(false),
		},
	}
}

// Register registers all seven tools with real handlers on the server.
func (a *App) Register(s *mcp.Server) {
	mcp.AddTool(s, readOnlyTool("get_exiftool_info",
		"Report the installed ExifTool: executable path, version, config mode, supported/readable file formats and server limits. Use this first to diagnose the environment."), a.getExiftoolInfo)
	mcp.AddTool(s, readOnlyTool("read_metadata",
		"Read metadata from one or more files, batches, directories or recursive trees with agent-controlled tag filters and read options. Returns per-file entries with full tag identity (group, document, instance). Large results are stored and paged via get_result_page."), a.readMetadata)
	searchTool := readOnlyTool("search_metadata",
		"Find files whose metadata satisfies a typed, nestable predicate query (three-valued logic: match / no_match / indeterminate). Scans run in batches over a frozen candidate snapshot and can be resumed with resume_token after the response or a server restart.")
	// The query AST is recursive, which the SDK's schema inference
	// cannot express; supply the equivalent JSON Schema explicitly.
	searchTool.InputSchema = searchInputSchema
	mcp.AddTool(s, searchTool, a.searchMetadata)
	mcp.AddTool(s, readOnlyTool("compare_metadata",
		"Compare the metadata of exactly two files read with identical options, returning added / removed / changed tags with full identity."), a.compareMetadata)
	mcp.AddTool(s, readOnlyTool("discover_tags",
		"Discover tag and group names, either from the installed ExifTool catalog (-listx/-listg) or observed in real files with occurrence counts."), a.discoverTags)
	mcp.AddTool(s, readOnlyTool("extract_binary",
		"Extract one specific embedded binary tag (thumbnail, preview, ICC profile, ...) from one file, byte-identical to 'exiftool -b'. Small images are returned as MCP image content, small binaries as base64, large binaries by reference for get_result_page."), a.extractBinary)
	mcp.AddTool(s, readOnlyTool("get_result_page",
		"Fetch the next page or fragment of a previously stored result (from read_metadata, search_metadata, compare_metadata, discover_tags or extract_binary). Never runs ExifTool and never resumes a scan."), a.getResultPage)
}

// errResult builds an IsError CallToolResult for whole-operation failures.
func errResult() *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true}
}

func okResult(isErr bool) *mcp.CallToolResult {
	if isErr {
		return errResult()
	}
	return &mcp.CallToolResult{}
}

// encodeCursor renders a record offset as an opaque cursor string.
func encodeCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("o:%d", offset)))
}

// decodeCursor parses a cursor produced by encodeCursor.
func decodeCursor(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, fmt.Errorf("invalid cursor")
	}
	var off int
	if _, err := fmt.Sscanf(string(raw), "o:%d", &off); err != nil || off < 0 {
		return 0, fmt.Errorf("invalid cursor")
	}
	return off, nil
}

func marshalRecords[T any](items []T) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, len(items))
	for i, it := range items {
		b, err := json.Marshal(it)
		if err != nil {
			return nil, err
		}
		out[i] = b
	}
	return out, nil
}

// pageByBudget picks the longest prefix of items whose marshaled size
// fits byteBudget (always at least one item) and count limit.
func pageByBudget[T any](items []T, countLimit, byteBudget int) (int, []json.RawMessage, error) {
	records, err := marshalRecords(items)
	if err != nil {
		return 0, nil, err
	}
	n, used := 0, 0
	for i, rec := range records {
		if countLimit > 0 && i >= countLimit {
			break
		}
		if n > 0 && used+len(rec) > byteBudget {
			break
		}
		used += len(rec)
		n++
	}
	return n, records, nil
}

// catalog returns the cached tag catalog for this ExifTool installation.
func (a *App) catalog(ctx context.Context) (*catalog.Catalog, error) {
	version, err := runner.Version(ctx, a.Runner)
	if err != nil {
		return nil, err
	}
	key := version + "|" + a.Runner.ConfigMode() + "|" + a.Runner.Path()
	a.catMu.Lock()
	defer a.catMu.Unlock()
	if a.cat != nil && a.catKey == key {
		return a.cat, nil
	}
	c, err := catalog.Load(ctx, a.Runner)
	if err != nil {
		return nil, err
	}
	a.cat, a.catKey = c, key
	return c, nil
}
