package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
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
	// Index is the candidate index this record came from.
	Index int `json:"index"`
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
	if queryNeedsProvenance(in.Query) {
		// same_document verdicts need document identity in the
		// entries; request family 3 without forcing -ee.
		spec.DocProvenance = true
	}
	// Verdicts are computed over EVERY value of a tag: without
	// family 4, ExifTool's JSON silently drops same-group
	// duplicates, and a hidden value could flip a verdict — a
	// fabricated no_match under duplicate_match "any". Search
	// therefore always reads duplicates; the caller's
	// include_duplicates choice governs read_metadata output,
	// not verdict inputs. EffectiveOptions reports the spec
	// actually used.
	spec.IncludeDuplicates = true
	if err := spec.Validate(); err != nil {
		return fail("INVALID_ARGUMENT", err.Error())
	}
	if err := guardFastLevel(spec); err != nil {
		return fail("INVALID_ARGUMENT", err.Error())
	}
	candidates, enumErrs := meta.Enumerate(in.Paths, in.Recursive, in.Extensions)
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
	// Enumeration errors are part of the scan's persisted state:
	// they are reported by every batch call (via ErrorDetails) and
	// keep the final status partial — part of the requested input
	// was never examined, so the scan is not "complete".
	for _, e := range enumErrs {
		job.EnumErrors++
		addScanError(job, e.Code, e.Message, e.Path)
	}
	if in.TimeContext != nil {
		job.TimeContext = in.TimeContext.Timezone
		job.HasTimeContext = true
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
	// Serialize batches of one scan: a concurrent resume waits for the
	// running batch, then continues from its committed checkpoint.
	// The in-process mutex covers this server; the store lock extends
	// the same guarantee to a second server process sharing the
	// store directory.
	lock := a.scanLock(in.ResumeToken)
	lock.Lock()
	defer lock.Unlock()
	release, err := a.Store.LockScan(ctx, in.ResumeToken)
	if err != nil {
		return fail(storeErrorCode(err), err.Error())
	}
	defer release()
	job, err := a.Store.LoadScan(in.ResumeToken)
	if err != nil {
		return fail(storeErrorCode(err), err.Error())
	}
	resultMeta, err := a.Store.LoadMeta(job.ResultID)
	if err != nil {
		return fail(storeErrorCode(err), err.Error())
	}
	if resultMeta.TotalRecords < job.CommittedRecords {
		return fail("CHECKPOINT_CORRUPT", "stored records do not match the scan checkpoint; the scan cannot be resumed safely")
	}
	if resultMeta.TotalRecords > job.CommittedRecords {
		// A batch appended its records but the checkpoint save was
		// lost (crash between the two writes). The batch's other
		// outcomes — no-matches and per-file errors — were never
		// persisted, so adopting its records would corrupt the
		// counts (earlier recovery turned lost errors into
		// no-matches). Roll the result back to the checkpoint and
		// let the batch be reprocessed from its start instead.
		if err := a.Store.TruncateRecords(job.ResultID, job.CommittedRecords); err != nil {
			return fail("CHECKPOINT_CORRUPT", err.Error())
		}
	}
	var node query.Node
	if err := json.Unmarshal(job.QueryJSON, &node); err != nil {
		return fail("CHECKPOINT_CORRUPT", err.Error())
	}
	// The scan's timezone assumption is frozen at scan start: a
	// resume may not add one to a scan that began without it, nor
	// change the one it began with — later batches would otherwise
	// judge datetimes by a different rule than earlier ones.
	hasTC := job.HasTimeContext || job.TimeContext != ""
	if in.TimeContext != nil && (!hasTC || in.TimeContext.Timezone != job.TimeContext) {
		return fail("INVALID_ARGUMENT", "time_context is frozen when the scan starts and cannot be added or changed on resume")
	}
	var tc *query.TimeContext
	if hasTC {
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
	if err := guardFastLevel(spec); err != nil {
		return fail("INVALID_ARGUMENT", err.Error())
	}
	out.EffectiveOptions = spec.Describe()
	return a.runSearchBatch(ctx, in, job, prepared, spec, out)
}

// maxStoredScanErrors caps how many per-file error details a
// checkpoint persists; the exact total always remains in
// ScanJob.Errors.
const maxStoredScanErrors = 200

// addScanError records a per-file error in the checkpoint so it is
// still reported by later resumes.
func addScanError(job *store.ScanJob, code, message, path string) {
	if len(job.ErrorDetails) >= maxStoredScanErrors {
		return
	}
	job.ErrorDetails = append(job.ErrorDetails, store.ScanError{Code: code, Message: message, Path: path})
}

// readFilesIndividually reads files one at a time after a batch
// read timed out, under the scan call's shared work deadline (ctx):
// a file gets at most the configured per-invocation timeout within
// whatever of the deadline remains — the fallback does NOT restart
// the call's time budget per file. A file whose read fails becomes
// an error result for that file rather than failing the whole
// batch. When the work deadline is spent, the files determined so
// far are returned and the rest of the batch waits for the next
// call; the caller commits exactly this prefix. Caller cancellation
// still aborts the call.
func (a *App) readFilesIndividually(ctx context.Context, files []string, spec meta.ReadSpec) ([]meta.FileResult, error) {
	out := make([]meta.FileResult, 0, len(files))
	for _, f := range files {
		callCtx, cancel := context.WithTimeout(ctx, a.Cfg.CallTimeout)
		res, err := a.Reader.Read(callCtx, []string{f}, spec)
		cancel()
		if err != nil {
			if ctx.Err() == context.Canceled {
				return nil, ctx.Err()
			}
			out = append(out, meta.FileResult{Path: f, Status: "error", Error: err.Error()})
			if ctx.Err() != nil {
				break // work deadline spent; the tail waits
			}
			continue
		}
		out = append(out, res...)
	}
	return out, nil
}

// externalizeValues moves oversized values in scan record entries
// into the store as text fragments (the same treatment read_metadata
// gives them), so a search response never carries a huge value
// inline either.
func (a *App) externalizeValues(entries []meta.Entry) ([]meta.Entry, error) {
	for i := range entries {
		if _, err := a.externalizeEntryValue(&entries[i]); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

// runSearchBatch evaluates one bounded batch of the scan and commits it.
func (a *App) runSearchBatch(ctx context.Context, in SearchMetadataIn, job *store.ScanJob, prepared *query.Prepared, spec meta.ReadSpec, out SearchMetadataOut) (*mcp.CallToolResult, SearchMetadataOut, error) {
	// The checkpoint exists before the batch runs, so every
	// outcome of this call — including a failed first batch —
	// identifies it: the caller can resume instead of restarting
	// the scan from zero.
	out.ResultID = job.ResultID
	out.ResumeToken = job.ID
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
	if remaining <= 0 {
		// Already fully scanned (e.g. a repeated resume of a
		// completed scan): report completion instead of slicing
		// past the candidate list.
		budget = 0
	} else if budget > remaining {
		budget = remaining
	}
	var callRecords []ScanRecord
	if budget > 0 {
		// ONE work deadline bounds the whole invocation: the batch
		// attempt and the per-file fallback both live under it, so
		// max_seconds is a real bound for the call. (The round-2
		// fallback escaped it with a fresh timeout per file.)
		workCtx, cancel := context.WithTimeout(ctx, batchTime)
		defer cancel()
		batchStart := job.NextIndex
		batch := job.Candidates[job.NextIndex : job.NextIndex+budget]
		files := make([]string, len(batch))
		changed := make([]bool, len(batch))
		statErrs := make([]error, len(batch))
		var alive []int
		for i, c := range batch {
			files[i] = c.Path
			fi, err := os.Stat(c.Path)
			if err != nil {
				statErrs[i] = err
				continue
			}
			if fi.Size() != c.Size || !fi.ModTime().Equal(c.ModTime) {
				changed[i] = true
			}
			alive = append(alive, i)
		}
		var results []meta.FileResult
		if len(alive) > 0 {
			aliveFiles := make([]string, len(alive))
			for j, i := range alive {
				aliveFiles[j] = files[i]
			}
			// The batch attempt gets half the work budget; when it
			// times out, the per-file fallback works with the other
			// half under the SAME deadline instead of a new one.
			attemptCtx, acancel := context.WithTimeout(workCtx, batchTime/2)
			r, err := a.Reader.Read(attemptCtx, aliveFiles, spec)
			acancel()
			if err != nil && attemptCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
				// One pathological file can stall every retry of
				// the same batch forever; read the files one by
				// one under the remaining work deadline.
				r, err = a.readFilesIndividually(workCtx, aliveFiles, spec)
			}
			if err != nil {
				// Nothing from this batch is committed; the
				// checkpoint still points at its start.
				return fail(errorCode(err), err.Error())
			}
			results = r
		}
		// Accounting walks the batch in candidate order and
		// consumes the results as the prefix they are: if the work
		// deadline stopped the fallback early, the unprocessed tail
		// is left for the next call — the watermark never skips it.
		resIdx := 0
		for i := range batch {
			if statErrs[i] != nil {
				job.Errors++
				job.Processed++
				job.NextIndex++
				addScanError(job, "PATH_NOT_FOUND", statErrs[i].Error(), files[i])
				continue
			}
			if resIdx >= len(results) {
				break
			}
			fr := results[resIdx]
			resIdx++
			job.Processed++
			job.NextIndex++
			if fr.Status == "error" {
				job.Errors++
				addScanError(job, "EXTRACTION_FAILED", fr.Error, fr.Path)
				continue
			}
			tri, reason := prepared.Eval(fr.Entries)
			switch tri {
			case query.Match:
				job.Matched++
				vals, verr := a.externalizeValues(filterEntries(fr.Entries, job.ReturnTags))
				if verr != nil {
					return fail(storeErrorCode(verr), verr.Error())
				}
				callRecords = append(callRecords, ScanRecord{
					Path: fr.Path, Status: "match",
					Values:                vals,
					ChangedSinceDiscovery: changed[i],
					Index:                 batchStart + i,
				})
			case query.Indeterminate:
				job.Indet++
				vals, verr := a.externalizeValues(filterEntries(fr.Entries, job.ReturnTags))
				if verr != nil {
					return fail(storeErrorCode(verr), verr.Error())
				}
				callRecords = append(callRecords, ScanRecord{
					Path: fr.Path, Status: "indeterminate", Reason: reason,
					Values:                vals,
					ChangedSinceDiscovery: changed[i],
					Index:                 batchStart + i,
				})
			default:
				job.NoMatch++
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
		if err := a.Store.MarkComplete(job.ResultID); err != nil {
			return fail(storeErrorCode(err), err.Error())
		}
	}
	pageN, _, err := pageByBudget(callRecords, a.Cfg.DefaultPageSize, a.Cfg.MaxResponseBytes)
	if err == nil && pageN < len(callRecords) {
		callRecords = callRecords[:pageN]
	}
	out.Items = callRecords
	out.MatchedCount = job.Matched
	out.NoMatchCount = job.NoMatch
	out.IndeterminateCount = job.Indet
	out.ErrorCount = job.Errors + job.EnumErrors
	out.ScannedCount = job.Processed
	out.TotalCandidates = len(job.Candidates)
	out.ScanComplete = job.Done
	out.ResultID = job.ResultID
	out.ExpiresAt = job.ExpiresAt.Format(time.RFC3339)
	if !job.Done {
		out.ResumeToken = job.ID
		out.Status = "partial"
	} else if job.Errors > 0 || job.EnumErrors > 0 {
		out.Status = "partial"
	}
	// Report the scan's accumulated per-file errors (persisted in the
	// checkpoint, so they survive restarts) after any errors this call
	// collected before the batch ran (e.g. enumeration errors).
	for _, e := range job.ErrorDetails {
		out.Errors = append(out.Errors, ToolError{Code: e.Code, Message: e.Message, Path: e.Path})
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

// queryNeedsProvenance reports whether any node in the query uses
// the same_document scope; such queries can only reach determinate
// verdicts when the read entries carry document identity.
func queryNeedsProvenance(n *query.Node) bool {
	if n == nil {
		return false
	}
	if n.Scope == "same_document" {
		return true
	}
	for _, c := range n.Children {
		if queryNeedsProvenance(c) {
			return true
		}
	}
	return false
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
// shaping return values (tag and group names are case-insensitive,
// as ExifTool resolves them).
func selectorMatches(e meta.Entry, sel string) bool {
	group, name := "", sel
	for i := 0; i < len(sel); i++ {
		if sel[i] == ':' {
			group, name = sel[:i], sel[i+1:]
			break
		}
	}
	if !strings.EqualFold(name, "All") && !strings.EqualFold(e.Name, name) {
		return false
	}
	if group == "" || strings.EqualFold(e.Group, group) {
		return true
	}
	// Any family segment may be used to reference the group.
	for _, seg := range splitColon(e.Group) {
		if strings.EqualFold(seg, group) {
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
