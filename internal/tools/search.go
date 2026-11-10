package tools

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alfa-reza/exiftool-mcp-go/internal/meta"
	"github.com/alfa-reza/exiftool-mcp-go/internal/query"
	"github.com/alfa-reza/exiftool-mcp-go/internal/store"
)

// searchInputSchema is the explicit JSON Schema for search_metadata.
// The query node definition is recursive ($defs + $ref), mirroring
// query.Node; semantic validation happens in query.Prepare.
var searchInputSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"paths":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"recursive":   map[string]any{"type": "boolean"},
		"extensions":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"query":       map[string]any{"$ref": "#/$defs/queryNode"},
		"return_tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"read_options": map[string]any{"type": "object", "properties": map[string]any{
			"include_duplicates":  map[string]any{"type": "boolean"},
			"unknown_tags":        map[string]any{"type": "integer"},
			"embedded_level":      map[string]any{"type": "integer"},
			"numeric_values":      map[string]any{"type": "boolean"},
			"preserve_structures": map[string]any{"type": "boolean"},
			"fast_scan_level":     map[string]any{"type": "integer"},
		}},
		"time_context": map[string]any{"type": "object", "properties": map[string]any{
			"timezone": map[string]any{"type": "string"},
		}},
		"scan_options": map[string]any{"type": "object", "properties": map[string]any{
			"max_files":   map[string]any{"type": "integer"},
			"max_seconds": map[string]any{"type": "integer"},
		}},
		"resume_token": map[string]any{"type": "string"},
	},
	"$defs": map[string]any{
		"queryNode": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"type":            map[string]any{"type": "string", "enum": []string{"all", "any", "not", "predicate"}},
				"scope":           map[string]any{"type": "string", "enum": []string{"file", "same_document"}},
				"children":        map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/queryNode"}},
				"tag":             map[string]any{"type": "string"},
				"operator":        map[string]any{"type": "string", "enum": []string{"exists", "missing", "eq", "ne", "contains", "starts_with", "ends_with", "regex", "gt", "gte", "lt", "lte", "between", "in", "not_in", "before", "after"}},
				"value":           map[string]any{},
				"value_type":      map[string]any{"type": "string", "enum": []string{"text", "number", "datetime"}},
				"duplicate_match": map[string]any{"type": "string", "enum": []string{"any", "all"}},
				"case_sensitive":  map[string]any{"type": "boolean"},
			},
			"required": []string{"type"},
		},
	},
}

// --- search_metadata ---

type ScanOptionsInput struct {
	MaxFiles   int `json:"max_files,omitempty"`
	MaxSeconds int `json:"max_seconds,omitempty"`
}

type SearchMetadataIn struct {
	Paths       []string           `json:"paths,omitempty"`
	Recursive   bool               `json:"recursive,omitempty"`
	Extensions  []string           `json:"extensions,omitempty"`
	Query       *query.Node        `json:"query,omitempty"`
	ReturnTags  []string           `json:"return_tags,omitempty"`
	ReadOptions *ReadOptionsInput  `json:"read_options,omitempty"`
	TimeContext *query.TimeContext `json:"time_context,omitempty"`
	ScanOptions *ScanOptionsInput  `json:"scan_options,omitempty"`
	ResumeToken string             `json:"resume_token,omitempty"`
}

// ScanRecord is one committed search outcome (match or indeterminate).
type ScanRecord struct {
	Path                  string       `json:"path"`
	Status                string       `json:"status"` // match | indeterminate
	Reason                string       `json:"reason,omitempty"`
	Values                []meta.Entry `json:"values,omitempty"`
	ChangedSinceDiscovery bool         `json:"changed_since_discovery,omitempty"`
}

type SearchMetadataOut struct {
	Status             string         `json:"status"` // complete | partial | failed
	Items              []ScanRecord   `json:"items,omitempty"`
	MatchedCount       int            `json:"matched_count"`
	NoMatchCount       int            `json:"no_match_count"`
	IndeterminateCount int            `json:"indeterminate_count"`
	ErrorCount         int            `json:"error_count"`
	ScannedCount       int            `json:"scanned_count"`
	TotalCandidates    int            `json:"total_candidates"`
	ScanComplete       bool           `json:"scan_complete"`
	ResumeToken        string         `json:"resume_token,omitempty"`
	ResultID           string         `json:"result_id,omitempty"`
	ExpiresAt          string         `json:"expires_at,omitempty"`
	EffectiveOptions   map[string]any `json:"effective_options,omitempty"`
	Errors             []ToolError    `json:"errors,omitempty"`
}

func (a *App) searchMetadata(ctx context.Context, _ *mcp.CallToolRequest, in SearchMetadataIn) (*mcp.CallToolResult, SearchMetadataOut, error) {
	out := SearchMetadataOut{Status: "complete"}
	fail := func(code, msg string) (*mcp.CallToolResult, SearchMetadataOut, error) {
		out.Status = "failed"
		out.Errors = append(out.Errors, ToolError{Code: code, Message: msg})
		return errResult(), out, nil
	}
	if in.ResumeToken != "" {
		if len(in.Paths) > 0 || in.Query != nil {
			return fail("INVALID_ARGUMENT", "resume_token cannot be combined with paths or query")
		}
		return a.resumeSearch(ctx, in)
	}
	if len(in.Paths) == 0 || in.Query == nil {
		return fail("INVALID_ARGUMENT", "a new scan requires paths and query")
	}
	prepared, err := query.Prepare(in.Query, in.TimeContext)
	if err != nil {
		return fail("INVALID_ARGUMENT", err.Error())
	}
	for _, t := range in.ReturnTags {
		if err := meta.ValidateSelector(t); err != nil {
			return fail("INVALID_ARGUMENT", err.Error())
		}
	}
	// Read only the tags the query and the response need; this preserves
	// predicate semantics because every referenced tag is requested.
	include := collectQueryTags(in.Query)
	include = appendUnique(include, in.ReturnTags...)
	spec := in.ReadOptions.Spec(include, nil)
	if err := spec.Validate(); err != nil {
		return fail("INVALID_ARGUMENT", err.Error())
	}
	candidates, enumErrs := meta.Enumerate(in.Paths, in.Recursive, in.Extensions)
	for _, e := range enumErrs {
		out.Errors = append(out.Errors, ToolError{Code: e.Code, Message: e.Message, Path: e.Path})
	}
	result, err := a.Store.CreateResult(store.KindRecords)
	if err != nil {
		return fail(storeErrorCode(err), err.Error())
	}
	queryJSON, _ := json.Marshal(in.Query)
	specJSON, _ := json.Marshal(spec)
	job := &store.ScanJob{
		ResultID:   result.ID,
		QueryJSON:  queryJSON,
		SpecJSON:   specJSON,
		ReturnTags: in.ReturnTags,
		Candidates: candidates,
	}
	if in.TimeContext != nil {
		job.TimeContext = in.TimeContext.Timezone
	}
	if err := a.Store.CreateScan(job); err != nil {
		return fail(storeErrorCode(err), err.Error())
	}
	out.EffectiveOptions = spec.Describe()
	return a.runSearchBatch(ctx, in, job, prepared, spec, out)
}

func (a *App) resumeSearch(ctx context.Context, in SearchMetadataIn) (*mcp.CallToolResult, SearchMetadataOut, error) {
	out := SearchMetadataOut{Status: "complete"}
	fail := func(code, msg string) (*mcp.CallToolResult, SearchMetadataOut, error) {
		out.Status = "failed"
		out.Errors = append(out.Errors, ToolError{Code: code, Message: msg})
		return errResult(), out, nil
	}
	job, err := a.Store.LoadScan(in.ResumeToken)
	if err != nil {
		return fail(storeErrorCode(err), err.Error())
	}
	resultMeta, err := a.Store.LoadMeta(job.ResultID)
	if err != nil {
		return fail(storeErrorCode(err), err.Error())
	}
	if resultMeta.TotalRecords != job.CommittedRecords {
		return fail("CHECKPOINT_CORRUPT", "stored records do not match the scan checkpoint; the scan cannot be resumed safely")
	}
	var node query.Node
	if err := json.Unmarshal(job.QueryJSON, &node); err != nil {
		return fail("CHECKPOINT_CORRUPT", err.Error())
	}
	// The scan's original timezone assumption is authoritative; a
	// time_context sent with the resume call only fills in when the
	// scan was started without one.
	tc := in.TimeContext
	if job.TimeContext != "" {
		tc = &query.TimeContext{Timezone: job.TimeContext}
	}
	prepared, err := query.Prepare(&node, tc)
	if err != nil {
		return fail("CHECKPOINT_CORRUPT", err.Error())
	}
	var spec meta.ReadSpec
	if err := json.Unmarshal(job.SpecJSON, &spec); err != nil {
		return fail("CHECKPOINT_CORRUPT", err.Error())
	}
	out.EffectiveOptions = spec.Describe()
	return a.runSearchBatch(ctx, in, job, prepared, spec, out)
}

// runSearchBatch evaluates one bounded batch of the scan and commits it.
func (a *App) runSearchBatch(ctx context.Context, in SearchMetadataIn, job *store.ScanJob, prepared *query.Prepared, spec meta.ReadSpec, out SearchMetadataOut) (*mcp.CallToolResult, SearchMetadataOut, error) {
	fail := func(code, msg string) (*mcp.CallToolResult, SearchMetadataOut, error) {
		out.Status = "failed"
		out.Errors = append(out.Errors, ToolError{Code: code, Message: msg})
		return errResult(), out, nil
	}
	budget := a.Cfg.ScanBatchFiles
	batchTime := a.Cfg.ScanBatchTime
	if in.ScanOptions != nil {
		if in.ScanOptions.MaxFiles > 0 {
			budget = min(in.ScanOptions.MaxFiles, 100000)
		}
		if in.ScanOptions.MaxSeconds > 0 {
			batchTime = time.Duration(min(in.ScanOptions.MaxSeconds, 3600)) * time.Second
		}
	}
	remaining := len(job.Candidates) - job.NextIndex
	if remaining > 0 && budget > remaining {
		budget = remaining
	}
	var callRecords []ScanRecord
	if budget > 0 {
		batchCtx, cancel := context.WithTimeout(ctx, batchTime)
		defer cancel()
		batch := job.Candidates[job.NextIndex : job.NextIndex+budget]
		files := make([]string, len(batch))
		changed := make([]bool, len(batch))
		var alive []int
		for i, c := range batch {
			files[i] = c.Path
			fi, err := os.Stat(c.Path)
			if err != nil {
				job.Errors++
				job.Processed++
				job.NextIndex++
				out.Errors = append(out.Errors, ToolError{Code: "PATH_NOT_FOUND", Message: err.Error(), Path: c.Path})
				continue
			}
			if fi.Size() != c.Size || !fi.ModTime().Equal(c.ModTime) {
				changed[i] = true
			}
			alive = append(alive, i)
		}
		if len(alive) > 0 {
			aliveFiles := make([]string, len(alive))
			for j, i := range alive {
				aliveFiles[j] = files[i]
			}
			results, err := a.Reader.Read(batchCtx, aliveFiles, spec)
			if err != nil {
				// Nothing from this batch is committed; the checkpoint
				// still points at its start.
				return fail(errorCode(err), err.Error())
			}
			for j, fr := range results {
				i := alive[j]
				job.Processed++
				job.NextIndex++
				if fr.Status == "error" {
					job.Errors++
					out.Errors = append(out.Errors, ToolError{Code: "EXTRACTION_FAILED", Message: fr.Error, Path: fr.Path})
					continue
				}
				tri, reason := prepared.Eval(fr.Entries)
				switch tri {
				case query.Match:
					job.Matched++
					callRecords = append(callRecords, ScanRecord{
						Path: fr.Path, Status: "match",
						Values:                filterEntries(fr.Entries, job.ReturnTags),
						ChangedSinceDiscovery: changed[i],
					})
				case query.Indeterminate:
					job.Indet++
					callRecords = append(callRecords, ScanRecord{
						Path: fr.Path, Status: "indeterminate", Reason: reason,
						Values:                filterEntries(fr.Entries, job.ReturnTags),
						ChangedSinceDiscovery: changed[i],
					})
				default:
					job.NoMatch++
				}
			}
		}
	}
	// Commit order: records first, then the checkpoint that counts them.
	if len(callRecords) > 0 {
		records, err := marshalRecords(callRecords)
		if err != nil {
			return fail("EXTRACTION_FAILED", err.Error())
		}
		if _, err := a.Store.AppendRecords(job.ResultID, records); err != nil {
			return fail(storeErrorCode(err), err.Error())
		}
		job.CommittedRecords += len(callRecords)
	}
	job.Done = job.NextIndex >= len(job.Candidates)
	if err := a.Store.SaveScan(job); err != nil {
		return fail(storeErrorCode(err), err.Error())
	}
	if job.Done {
		_ = a.Store.MarkComplete(job.ResultID)
	}
	pageN, _, err := pageByBudget(callRecords, a.Cfg.DefaultPageSize, a.Cfg.MaxResponseBytes)
	if err == nil && pageN < len(callRecords) {
		callRecords = callRecords[:pageN]
	}
	out.Items = callRecords
	out.MatchedCount = job.Matched
	out.NoMatchCount = job.NoMatch
	out.IndeterminateCount = job.Indet
	out.ErrorCount = job.Errors
	out.ScannedCount = job.Processed
	out.TotalCandidates = len(job.Candidates)
	out.ScanComplete = job.Done
	out.ResultID = job.ResultID
	out.ExpiresAt = job.ExpiresAt.Format(time.RFC3339)
	if !job.Done {
		out.ResumeToken = job.ID
		out.Status = "partial"
	} else if job.Errors > 0 {
		out.Status = "partial"
	}
	return okResult(false), out, nil
}

// collectQueryTags walks the AST and returns all predicate tag selectors.
func collectQueryTags(n *query.Node) []string {
	var out []string
	var walk func(*query.Node)
	walk = func(x *query.Node) {
		if x == nil {
			return
		}
		if x.Type == "predicate" && x.Tag != "" {
			out = appendUnique(out, x.Tag)
		}
		for _, c := range x.Children {
			walk(c)
		}
	}
	walk(n)
	return out
}

func appendUnique(list []string, items ...string) []string {
	seen := make(map[string]bool, len(list))
	for _, s := range list {
		seen[s] = true
	}
	for _, s := range items {
		if !seen[s] {
			seen[s] = true
			list = append(list, s)
		}
	}
	return list
}

// filterEntries returns the entries selected by returnTags (all entries
// when returnTags is empty is NOT implied: empty means no values).
func filterEntries(entries []meta.Entry, selectors []string) []meta.Entry {
	if len(selectors) == 0 {
		return nil
	}
	var out []meta.Entry
	for _, e := range entries {
		for _, sel := range selectors {
			if selectorMatches(e, sel) {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

// selectorMatches mirrors the query package's selector semantics for
// shaping return values.
func selectorMatches(e meta.Entry, sel string) bool {
	group, name := "", sel
	for i := 0; i < len(sel); i++ {
		if sel[i] == ':' {
			group, name = sel[:i], sel[i+1:]
			break
		}
	}
	if name != "All" && e.Name != name {
		return false
	}
	if group == "" || e.Group == group {
		return true
	}
	// Any family segment may be used to reference the group.
	for _, seg := range splitColon(e.Group) {
		if seg == group {
			return true
		}
	}
	return false
}

func splitColon(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
