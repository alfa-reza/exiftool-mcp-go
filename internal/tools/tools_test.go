package tools

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alfa-reza/exiftool-mcp-go/internal/config"
	"github.com/alfa-reza/exiftool-mcp-go/internal/meta"
	"github.com/alfa-reza/exiftool-mcp-go/internal/query"
	"github.com/alfa-reza/exiftool-mcp-go/internal/runner"
	"github.com/alfa-reza/exiftool-mcp-go/internal/store"
)

// tinyJPEG is a valid 1x1 JPEG used as fixture ground truth.
const tinyJPEG = "/9j/4AAQSkZJRgABAQEASABIAAD/2wBDAP//////////////////////////////////////////////////////////////////////////////////////2wBDAf//////////////////////////////////////////////////////////////////////////////////////wAARCAABAAEDASIAAhEBAxEB/8QAFAABAAAAAAAAAAAAAAAAAAAACf/EABQQAQAAAAAAAAAAAAAAAAAAAAD/xAAUAQEAAAAAAAAAAAAAAAAAAAAA/8QAFBEBAAAAAAAAAAAAAAAAAAAAAP/aAAwDAQACEQMRAD8AvwH/2Q=="

func jpegBytes(t *testing.T) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(tinyJPEG)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func requireExifTool(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("exiftool")
	if err != nil {
		t.Skip("exiftool not installed; skipping integration test")
	}
	return p
}

func cli(t *testing.T, args ...string) {
	t.Helper()
	et := requireExifTool(t)
	cmd := exec.Command(et, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture exiftool %v: %v\n%s", args, err, out)
	}
}

func writeJPEG(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, jpegBytes(t), 0o644); err != nil {
		t.Fatal(err)
	}
}

type countingRunner struct {
	inner runner.Runner
	calls atomic.Int64
}

func (c *countingRunner) Run(ctx context.Context, args ...string) (runner.Result, error) {
	c.calls.Add(1)
	return c.inner.Run(ctx, args...)
}
func (c *countingRunner) Path() string       { return c.inner.Path() }
func (c *countingRunner) ConfigMode() string { return c.inner.ConfigMode() }

func newTestApp(t *testing.T, storeDir string) (*App, *countingRunner) {
	t.Helper()
	cfg := config.Config{
		StoreDir:         storeDir,
		CallTimeout:      60 * time.Second,
		MaxConcurrency:   4,
		MaxResponseBytes: 1 << 20,
		DefaultPageSize:  100,
		MaxInlineBinary:  1 << 20,
		ResultTTL:        time.Hour,
		StoreQuotaBytes:  1 << 30,
		ScanBatchFiles:   200,
		ScanBatchTime:    30 * time.Second,
	}
	cr := &countingRunner{inner: runner.New(cfg)}
	st, err := store.New(storeDir, cfg.ResultTTL, cfg.StoreQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{Cfg: cfg, Runner: cr, Reader: &meta.Reader{Runner: cr}, Store: st}
	return app, cr
}

// setupPhotos builds a directory with three tagged JPEGs and one
// corrupt file, returning the directory and file paths.
func setupPhotos(t *testing.T) string {
	t.Helper()
	requireExifTool(t)
	dir := t.TempDir()
	a := filepath.Join(dir, "a.jpg")
	b := filepath.Join(dir, "b.jpg")
	c := filepath.Join(dir, "c.jpg")
	thumb := filepath.Join(dir, "thumb-src.jpg")
	for _, p := range []string{a, b, c, thumb} {
		writeJPEG(t, p)
	}
	cli(t, "-q", "-overwrite_original",
		"-EXIF:Make=Canon", "-EXIF:Model=EOS Test", "-EXIF:ISO=3200",
		"-EXIF:DateTimeOriginal=2023:05:01 10:00:00",
		"-EXIF:CreateDate=2023:05:01 10:00:00", "-XMP:CreateDate=2023:05:01 10:00:00",
		a)
	cli(t, "-q", "-overwrite_original", "-ThumbnailImage<="+thumb, a)
	cli(t, "-q", "-overwrite_original", "-EXIF:Make=Nikon", "-EXIF:ISO=100", b)
	cli(t, "-q", "-overwrite_original", "-EXIF:Make=Canon", "-EXIF:ISO=800", c)
	// A genuinely corrupt file: binary junk no parser accepts (a text
	// file would be read successfully by ExifTool as TXT).
	junk := make([]byte, 300)
	for i := range junk {
		junk[i] = byte((i*37 + 11) % 256)
	}
	if err := os.WriteFile(filepath.Join(dir, "corrupt.jpg"), junk, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(thumb); err != nil {
		t.Fatal(err)
	}
	return dir
}

func findEntry(fr meta.FileResult, name string) *meta.Entry {
	for i := range fr.Entries {
		if fr.Entries[i].Name == name {
			return &fr.Entries[i]
		}
	}
	return nil
}

func TestInfoIntegration(t *testing.T) {
	app, _ := newTestApp(t, t.TempDir())
	_, out, err := app.getExiftoolInfo(context.Background(), nil, ExiftoolInfoIn{Format: "jpg"})
	if err != nil {
		t.Fatal(err)
	}
	et := requireExifTool(t)
	verOut, _ := exec.Command(et, "-ver").Output()
	if out.Status != "ok" || out.Version != strings.TrimSpace(string(verOut)) {
		t.Fatalf("info mismatch: %+v (cli %s)", out, verOut)
	}
	if out.FormatResult != "readable" {
		t.Fatalf("jpg should be readable, got %q", out.FormatResult)
	}
	if out.ConfigMode != "standard" {
		t.Fatalf("expected standard config mode, got %q", out.ConfigMode)
	}
}

func TestReadMetadataIntegration(t *testing.T) {
	dir := setupPhotos(t)
	app, _ := newTestApp(t, t.TempDir())
	ctx := context.Background()
	aPath := filepath.Join(dir, "a.jpg")
	before, _ := os.ReadFile(aPath)
	hashBefore := sha256.Sum256(before)

	_, out, err := app.readMetadata(ctx, nil, ReadMetadataIn{Paths: []string{aPath}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "complete" || len(out.Items) != 1 {
		t.Fatalf("unexpected read out: %+v", out)
	}
	fr := out.Items[0]
	mk := findEntry(fr, "Make")
	if mk == nil || mk.Value != "Canon" || mk.Group != "EXIF:IFD0" {
		t.Fatalf("Make entry wrong: %+v", mk)
	}
	// Duplicate names across groups are separate entries.
	createDates := 0
	for _, e := range fr.Entries {
		if e.Name == "CreateDate" {
			createDates++
		}
	}
	if createDates < 2 {
		t.Fatalf("expected EXIF+XMP CreateDate entries, got %d", createDates)
	}
	after, _ := os.ReadFile(aPath)
	if sha256.Sum256(after) != hashBefore {
		t.Fatal("source file changed after read (read-only violation)")
	}

	// Select filter actually restricts the result.
	_, sel, err := app.readMetadata(ctx, nil, ReadMetadataIn{
		Paths:          []string{aPath},
		MetadataFilter: &MetadataFilter{Mode: "select", IncludeTags: []string{"EXIF:CreateDate"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Items) != 1 {
		t.Fatalf("select: expected 1 item, got %d", len(sel.Items))
	}
	for _, e := range sel.Items[0].Entries {
		if e.Name != "CreateDate" {
			t.Fatalf("select returned unexpected entry %s", e.Key)
		}
	}

	// Mixed batch: one good, one corrupt, one missing.
	_, mixed, err := app.readMetadata(ctx, nil, ReadMetadataIn{
		Paths: []string{aPath, filepath.Join(dir, "corrupt.jpg"), filepath.Join(dir, "missing.jpg")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if mixed.Status != "partial" || mixed.ProcessedCount != 2 {
		t.Fatalf("mixed batch: status=%s processed=%d", mixed.Status, mixed.ProcessedCount)
	}
	statusByBase := map[string]string{}
	for _, it := range mixed.Items {
		statusByBase[filepath.Base(it.Path)] = it.Status
	}
	if statusByBase["a.jpg"] != "ok" || statusByBase["corrupt.jpg"] != "error" {
		t.Fatalf("mixed batch statuses: %v", statusByBase)
	}
	foundNotFound := false
	for _, e := range mixed.Errors {
		if e.Code == "PATH_NOT_FOUND" {
			foundNotFound = true
		}
	}
	if !foundNotFound {
		t.Fatalf("expected PATH_NOT_FOUND error, got %+v", mixed.Errors)
	}
}

func TestSearchResumeRestartFrozen(t *testing.T) {
	dir := setupPhotos(t)
	storeDir := t.TempDir()
	ctx := context.Background()
	q := &query.Node{Type: "predicate", Tag: "Make", Operator: "eq", Value: "Canon"}

	app1, _ := newTestApp(t, storeDir)
	_, out1, err := app1.searchMetadata(ctx, nil, SearchMetadataIn{
		Paths:       []string{dir},
		Query:       q,
		ScanOptions: &ScanOptionsInput{MaxFiles: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out1.ScanComplete || out1.ResumeToken == "" {
		t.Fatalf("first batch should be incomplete with a resume token: %+v", out1)
	}
	if out1.TotalCandidates != 4 || out1.ScannedCount != 2 || out1.MatchedCount != 1 {
		t.Fatalf("first batch stats wrong: %+v", out1)
	}
	// A file created after enumeration must not join the frozen scan.
	writeJPEG(t, filepath.Join(dir, "d.jpg"))
	cli(t, "-q", "-overwrite_original", "-EXIF:Make=Canon", filepath.Join(dir, "d.jpg"))

	// Simulate a server restart: brand-new App and Store on the same dir.
	app2, _ := newTestApp(t, storeDir)
	_, out2, err := app2.searchMetadata(ctx, nil, SearchMetadataIn{ResumeToken: out1.ResumeToken})
	if err != nil {
		t.Fatal(err)
	}
	if !out2.ScanComplete {
		t.Fatalf("second batch should complete the scan: %+v", out2)
	}
	if out2.TotalCandidates != 4 || out2.ScannedCount != 4 {
		t.Fatalf("frozen snapshot violated: %+v", out2)
	}
	if out2.MatchedCount != 2 || out2.NoMatchCount != 1 || out2.ErrorCount != 1 {
		t.Fatalf("final stats wrong: matched=%d nomatch=%d errors=%d",
			out2.MatchedCount, out2.NoMatchCount, out2.ErrorCount)
	}
	// Combining resume with a new query is rejected.
	_, bad, err := app2.searchMetadata(ctx, nil, SearchMetadataIn{ResumeToken: out1.ResumeToken, Query: q})
	if err != nil {
		t.Fatal(err)
	}
	if bad.Status != "failed" {
		t.Fatalf("resume+query should fail, got %+v", bad)
	}
	// Full history is readable through the result id.
	_, page, err := app2.getResultPage(ctx, nil, GetResultPageIn{ResultID: out2.ResultID, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("expected 2 stored match records, got %d", len(page.Items))
	}
}

func TestSearchResumeKeepsTimeContext(t *testing.T) {
	// Two files with a naive DateTimeOriginal of 10:00. The query asks
	// "after 04:00Z". Under the scan's +07:00 assumption the naive time
	// is 03:00Z (no_match); without any assumption the comparison is
	// mixed-timezone and indeterminate. The first batch runs with the
	// time context; the resume (after a simulated restart) sends none
	// and must reuse the stored one, or the second file's verdict
	// silently changes mid-scan.
	requireExifTool(t)
	dir := t.TempDir()
	for _, name := range []string{"a.jpg", "b.jpg"} {
		p := filepath.Join(dir, name)
		writeJPEG(t, p)
		cli(t, "-q", "-overwrite_original", "-EXIF:DateTimeOriginal=2023:05:01 10:00:00", p)
	}
	storeDir := t.TempDir()
	ctx := context.Background()
	q := &query.Node{Type: "predicate", Tag: "DateTimeOriginal", Operator: "after",
		Value: "2023-05-01T04:00:00Z", ValueType: "datetime"}

	app1, _ := newTestApp(t, storeDir)
	_, out1, err := app1.searchMetadata(ctx, nil, SearchMetadataIn{
		Paths:       []string{dir},
		Query:       q,
		TimeContext: &query.TimeContext{Timezone: "+07:00"},
		ScanOptions: &ScanOptionsInput{MaxFiles: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out1.ScanComplete || out1.NoMatchCount != 1 || out1.IndeterminateCount != 0 {
		t.Fatalf("first batch wrong: %+v", out1)
	}
	app2, _ := newTestApp(t, storeDir)
	_, out2, err := app2.searchMetadata(ctx, nil, SearchMetadataIn{ResumeToken: out1.ResumeToken})
	if err != nil {
		t.Fatal(err)
	}
	if !out2.ScanComplete || out2.NoMatchCount != 2 || out2.IndeterminateCount != 0 {
		t.Fatalf("resume lost the stored time_context: %+v", out2)
	}
}

func TestSearchResumeAfterCrashWindow(t *testing.T) {
	// Simulate a crash between committing a batch's records and
	// saving its checkpoint: the records stay, the checkpoint is
	// rewound to the pre-batch state. Resume must roll the
	// uncounted records back and reprocess the batch — not fail
	// with CHECKPOINT_CORRUPT, not duplicate any record, and not
	// lose the crashed batch's error classification.
	dir := setupPhotos(t)
	storeDir := t.TempDir()
	ctx := context.Background()
	q := &query.Node{Type: "predicate", Tag: "Make", Operator: "eq", Value: "Canon"}

	app1, _ := newTestApp(t, storeDir)
	_, out1, err := app1.searchMetadata(ctx, nil, SearchMetadataIn{
		Paths:       []string{dir},
		Query:       q,
		ScanOptions: &ScanOptionsInput{MaxFiles: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out1.MatchedCount != 2 || out1.ResumeToken == "" {
		t.Fatalf("first batch wrong: %+v", out1)
	}
	// Rewind the checkpoint to before the batch; the committed
	// records (2 matches) remain in the result store.
	job, err := app1.Store.LoadScan(out1.ResumeToken)
	if err != nil {
		t.Fatal(err)
	}
	job.CommittedRecords, job.NextIndex, job.Processed = 0, 0, 0
	job.Matched, job.NoMatch, job.Indet, job.Errors = 0, 0, 0, 0
	job.ErrorDetails = nil
	if err := app1.Store.SaveScan(job); err != nil {
		t.Fatal(err)
	}
	// Resume after a restart: rollback + reprocessing.
	app2, _ := newTestApp(t, storeDir)
	_, out2, err := app2.searchMetadata(ctx, nil, SearchMetadataIn{ResumeToken: out1.ResumeToken})
	if err != nil {
		t.Fatal(err)
	}
	if out2.Status == "failed" {
		t.Fatalf("resume after crash window failed: %+v", out2.Errors)
	}
	if !out2.ScanComplete || out2.ScannedCount != 4 {
		t.Fatalf("scan did not complete after recovery: %+v", out2)
	}
	if out2.MatchedCount != 2 || out2.NoMatchCount != 1 || out2.ErrorCount != 1 {
		t.Fatalf("counts after rollback wrong: %+v", out2)
	}
	_, page, err := app2.getResultPage(ctx, nil, GetResultPageIn{ResultID: out2.ResultID, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("expected exactly 2 stored records (no duplicates), got %d", len(page.Items))
	}
	// The corrupt file's error, recorded by the reprocessed batch,
	// is part of the persisted error history.
	foundErr := false
	for _, e := range out2.Errors {
		if e.Code == "EXTRACTION_FAILED" {
			foundErr = true
		}
	}
	if !foundErr {
		t.Fatalf("expected persisted EXTRACTION_FAILED error, got %+v", out2.Errors)
	}
}

func TestSearchConcurrentResume(t *testing.T) {
	// Two resumes of the same token at the same time must serialize:
	// both succeed, and the scan's records are written exactly once.
	dir := setupPhotos(t)
	storeDir := t.TempDir()
	ctx := context.Background()
	q := &query.Node{Type: "predicate", Tag: "Make", Operator: "eq", Value: "Canon"}

	app, _ := newTestApp(t, storeDir)
	_, out1, err := app.searchMetadata(ctx, nil, SearchMetadataIn{
		Paths:       []string{dir},
		Query:       q,
		ScanOptions: &ScanOptionsInput{MaxFiles: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	token := out1.ResumeToken
	if token == "" {
		t.Fatalf("expected a resume token: %+v", out1)
	}
	var wg sync.WaitGroup
	outs := make([]SearchMetadataOut, 2)
	errs := make([]error, 2)
	for k := 0; k < 2; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			_, o, e := app.searchMetadata(ctx, nil, SearchMetadataIn{ResumeToken: token})
			outs[k], errs[k] = o, e
		}(k)
	}
	wg.Wait()
	for k := range errs {
		if errs[k] != nil {
			t.Fatal(errs[k])
		}
		if outs[k].Status == "failed" {
			t.Fatalf("concurrent resume %d failed: %+v", k, outs[k].Errors)
		}
		if !outs[k].ScanComplete || outs[k].MatchedCount != 2 {
			t.Fatalf("concurrent resume %d wrong totals: %+v", k, outs[k])
		}
	}
	_, page, err := app.getResultPage(ctx, nil, GetResultPageIn{ResultID: out1.ResultID, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("expected exactly 2 stored records, got %d", len(page.Items))
	}
}

func TestReadDuplicatesPreserved(t *testing.T) {
	// A JPEG with two COM segments holds two File:Comment tags in the
	// same group. A duplicates read must return both, distinguished by
	// their copy identity; ExifTool drops the second without -G4.
	requireExifTool(t)
	data := jpegBytes(t)
	com := func(s string) []byte {
		seg := []byte{0xFF, 0xFE, 0, byte(len(s) + 2)}
		return append(seg, s...)
	}
	dup := append([]byte{}, data[:2]...)
	dup = append(dup, com("first comment")...)
	dup = append(dup, com("second comment")...)
	dup = append(dup, data[2:]...)
	path := filepath.Join(t.TempDir(), "dup.jpg")
	if err := os.WriteFile(path, dup, 0o644); err != nil {
		t.Fatal(err)
	}
	app, _ := newTestApp(t, t.TempDir())
	ctx := context.Background()
	_, out, err := app.readMetadata(ctx, nil, ReadMetadataIn{
		Paths:          []string{path},
		MetadataFilter: &MetadataFilter{Mode: "select", IncludeTags: []string{"Comment"}},
		ReadOptions:    &ReadOptionsInput{IncludeDuplicates: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 || out.Items[0].Status != "ok" {
		t.Fatalf("read failed: %+v", out)
	}
	var values []string
	var copies []string
	for _, e := range out.Items[0].Entries {
		if s, ok := e.Value.(string); ok {
			values = append(values, s)
			copies = append(copies, e.Copy)
		}
	}
	if len(values) != 2 || values[0] != "first comment" || values[1] != "second comment" {
		t.Fatalf("duplicates not preserved: %v", values)
	}
	if copies[0] != "" || copies[1] != "Copy1" {
		t.Fatalf("copy identity wrong: %v", copies)
	}
}

func TestReadMetadataPagingAcrossChunks(t *testing.T) {
	// 250 files with a page size of 100: the first page comes from
	// the first chunk, the rest must be walkable through the stored
	// result with no record lost or duplicated across chunk and
	// segment boundaries.
	requireExifTool(t)
	dir := t.TempDir()
	const n = 250
	for i := 0; i < n; i++ {
		writeJPEG(t, filepath.Join(dir, fmt.Sprintf("f%03d.jpg", i)))
	}
	app, _ := newTestApp(t, t.TempDir())
	ctx := context.Background()
	_, out, err := app.readMetadata(ctx, nil, ReadMetadataIn{
		Paths:           []string{dir},
		ResponseOptions: &ResponseOptionsInput{PageSize: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 100 || !out.HasMore || out.ResultID == "" || out.ProcessedCount != n {
		t.Fatalf("first page wrong: items=%d hasMore=%v result=%q processed=%d",
			len(out.Items), out.HasMore, out.ResultID, out.ProcessedCount)
	}
	seen := make(map[string]bool)
	for _, it := range out.Items {
		seen[it.Path] = true
	}
	cursor := out.NextCursor
	for cursor != "" {
		_, page, err := app.getResultPage(ctx, nil, GetResultPageIn{ResultID: out.ResultID, Cursor: cursor, PageSize: 100})
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range page.Items {
			var fr meta.FileResult
			if err := json.Unmarshal(raw, &fr); err != nil {
				t.Fatal(err)
			}
			if seen[fr.Path] {
				t.Fatalf("duplicate across pages: %s", fr.Path)
			}
			seen[fr.Path] = true
		}
		if !page.HasMore {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != n {
		t.Fatalf("paged %d of %d files", len(seen), n)
	}
}

func TestSearchChangedAndIndeterminate(t *testing.T) {
	requireExifTool(t)
	dir := t.TempDir()
	f1 := filepath.Join(dir, "f1.jpg")
	f2 := filepath.Join(dir, "f2.jpg")
	writeJPEG(t, f1)
	writeJPEG(t, f2)
	cli(t, "-q", "-overwrite_original", "-EXIF:Make=Canon", f1)
	cli(t, "-q", "-overwrite_original", "-EXIF:Make=Canon", f2)
	storeDir := t.TempDir()
	app, _ := newTestApp(t, storeDir)
	ctx := context.Background()
	q := &query.Node{Type: "predicate", Tag: "Make", Operator: "eq", Value: "Canon"}
	_, out1, err := app.searchMetadata(ctx, nil, SearchMetadataIn{
		Paths: []string{dir}, Query: q, ScanOptions: &ScanOptionsInput{MaxFiles: 1},
	})
	if err != nil || out1.ScanComplete {
		t.Fatalf("batch1: %+v err=%v", out1, err)
	}
	// Modify f2 after enumeration.
	cli(t, "-q", "-overwrite_original", "-EXIF:ISO=200", f2)
	_, out2, err := app.searchMetadata(ctx, nil, SearchMetadataIn{ResumeToken: out1.ResumeToken})
	if err != nil {
		t.Fatal(err)
	}
	if !out2.ScanComplete || len(out2.Items) != 1 {
		t.Fatalf("batch2: %+v", out2)
	}
	if !out2.Items[0].ChangedSinceDiscovery {
		t.Fatalf("changed file was not flagged: %+v", out2.Items[0])
	}

	// Indeterminate: naive datetime vs aware bound, no time context.
	dir2 := setupPhotos(t)
	app2, _ := newTestApp(t, t.TempDir())
	dq := &query.Node{Type: "predicate", Tag: "DateTimeOriginal", Operator: "before",
		Value: "2024-01-01T00:00:00+07:00", ValueType: "datetime"}
	_, out3, err := app2.searchMetadata(ctx, nil, SearchMetadataIn{
		Paths: []string{filepath.Join(dir2, "a.jpg")}, Query: dq,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out3.IndeterminateCount != 1 || len(out3.Items) != 1 || out3.Items[0].Status != "indeterminate" {
		t.Fatalf("expected one indeterminate record: %+v", out3)
	}
}

func TestCompareIntegration(t *testing.T) {
	dir := setupPhotos(t)
	app, _ := newTestApp(t, t.TempDir())
	ctx := context.Background()
	a := filepath.Join(dir, "a.jpg")
	mod := filepath.Join(dir, "mod.jpg")
	data, _ := os.ReadFile(a)
	if err := os.WriteFile(mod, data, 0o644); err != nil {
		t.Fatal(err)
	}
	cli(t, "-q", "-overwrite_original", "-EXIF:ISO=6400", "-XMP:Rating=5", mod)
	_, out, err := app.compareMetadata(ctx, nil, CompareMetadataIn{LeftPath: a, RightPath: mod})
	if err != nil {
		t.Fatal(err)
	}
	var isoChanged, ratingAdded bool
	for _, ch := range out.Changed {
		if ch.Name == "ISO" {
			isoChanged = true
		}
	}
	for _, ch := range out.Added {
		if ch.Name == "Rating" {
			ratingAdded = true
		}
	}
	if !isoChanged || !ratingAdded {
		t.Fatalf("compare missed changes: changed=%+v added=%+v", out.Changed, out.Added)
	}
	// Comparing a file with a corrupt one must not fabricate a diff.
	_, bad, err := app.compareMetadata(ctx, nil, CompareMetadataIn{LeftPath: a, RightPath: filepath.Join(dir, "corrupt.jpg")})
	if err != nil {
		t.Fatal(err)
	}
	if bad.Status != "failed" || len(bad.Added)+len(bad.Removed)+len(bad.Changed) != 0 {
		t.Fatalf("expected failed compare with no diff, got %+v", bad)
	}
}

func TestCompareDuplicateCopyPairing(t *testing.T) {
	requireExifTool(t)
	build := func(comments ...string) string {
		data := jpegBytes(t)
		com := func(s string) []byte {
			seg := []byte{0xFF, 0xFE, 0, byte(len(s) + 2)}
			return append(seg, s...)
		}
		out := append([]byte{}, data[:2]...)
		for _, c := range comments {
			out = append(out, com(c)...)
		}
		out = append(out, data[2:]...)
		p := filepath.Join(t.TempDir(), "x.jpg")
		if err := os.WriteFile(p, out, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	left := build("alpha", "beta")
	right := build("beta") // first copy deleted; "beta" renumbers to copy one
	app, _ := newTestApp(t, t.TempDir())
	_, out, err := app.compareMetadata(context.Background(), nil, CompareMetadataIn{
		LeftPath: left, RightPath: right,
		MetadataFilter:   &MetadataFilter{Mode: "select", IncludeTags: []string{"Comment"}},
		ReadOptions:      &ReadOptionsInput{IncludeDuplicates: true},
		IncludeUnchanged: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The honest diff: alpha was removed, beta is unchanged, and no
	// value "changed" — positional pairing fabricated exactly that.
	if len(out.Changed) != 0 {
		t.Fatalf("no value changed, got %+v", out.Changed)
	}
	if len(out.Removed) != 1 || out.Removed[0].LeftValue != "alpha" {
		t.Fatalf("alpha should be the removal, got %+v", out.Removed)
	}
	if len(out.Unchanged) != 1 || out.Unchanged[0].LeftValue != "beta" {
		t.Fatalf("beta should be unchanged, got %+v", out.Unchanged)
	}
}

func TestCompareUnchangedExcludedFromPages(t *testing.T) {
	dir := setupPhotos(t)
	app, _ := newTestApp(t, t.TempDir())
	ctx := context.Background()
	_, out, err := app.compareMetadata(ctx, nil, CompareMetadataIn{
		LeftPath: filepath.Join(dir, "a.jpg"), RightPath: filepath.Join(dir, "c.jpg"),
		ResponseOptions: &ResponseOptionsInput{PageSize: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.UnchangedCount == 0 {
		t.Fatal("expected some unchanged tags between the two Canon photos")
	}
	if len(out.Unchanged) != 0 {
		t.Fatalf("unchanged records leaked into the response: %+v", out.Unchanged)
	}
	if out.ResultID == "" {
		t.Fatal("expected overflow to be stored for paging")
	}
	_, page, err := app.Store.RecordsPage(out.ResultID, 0, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	sawChange := false
	for _, raw := range page.Records {
		var rec ChangeRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Change == "unchanged" {
			t.Fatalf("unchanged record leaked into the stored result: %+v", rec)
		}
		if rec.Change == "changed" {
			sawChange = true
		}
	}
	if !sawChange {
		t.Fatal("stored result should contain the ISO change")
	}
}

func TestFilterModeTypoRejected(t *testing.T) {
	dir := setupPhotos(t)
	app, _ := newTestApp(t, t.TempDir())
	ctx := context.Background()
	a := filepath.Join(dir, "a.jpg")
	_, out, err := app.readMetadata(ctx, nil, ReadMetadataIn{
		Paths:          []string{a},
		MetadataFilter: &MetadataFilter{Mode: "slect", IncludeTags: []string{"Make"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "failed" || len(out.Errors) == 0 || out.Errors[0].Code != "INVALID_ARGUMENT" {
		t.Fatalf("mistyped filter mode must be rejected, got %+v", out)
	}
	_, cout, err := app.compareMetadata(ctx, nil, CompareMetadataIn{
		LeftPath: a, RightPath: a,
		MetadataFilter: &MetadataFilter{Mode: "slect"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cout.Status != "failed" || len(cout.Errors) == 0 || cout.Errors[0].Code != "INVALID_ARGUMENT" {
		t.Fatalf("mistyped filter mode must be rejected in compare, got %+v", cout)
	}
}

func TestFastLevelGuardForConclusions(t *testing.T) {
	dir := setupPhotos(t)
	app, _ := newTestApp(t, t.TempDir())
	ctx := context.Background()
	a := filepath.Join(dir, "a.jpg")
	q := &query.Node{Type: "predicate", Tag: "Make", Operator: "eq", Value: "Canon"}
	_, out, err := app.searchMetadata(ctx, nil, SearchMetadataIn{
		Paths: []string{dir}, Query: q,
		ReadOptions: &ReadOptionsInput{FastScanLevel: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "failed" || len(out.Errors) == 0 || out.Errors[0].Code != "INVALID_ARGUMENT" {
		t.Fatalf("search with fast_scan_level 2 must be rejected, got %+v", out)
	}
	_, cout, err := app.compareMetadata(ctx, nil, CompareMetadataIn{
		LeftPath: a, RightPath: a,
		ReadOptions: &ReadOptionsInput{FastScanLevel: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cout.Status != "failed" || len(cout.Errors) == 0 || cout.Errors[0].Code != "INVALID_ARGUMENT" {
		t.Fatalf("compare with fast_scan_level 4 must be rejected, got %+v", cout)
	}
	// A plain read keeps the option: the caller asked for the
	// extraction itself, and the effective options disclose it.
	_, rout, err := app.readMetadata(ctx, nil, ReadMetadataIn{
		Paths:       []string{a},
		ReadOptions: &ReadOptionsInput{FastScanLevel: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rout.Status == "failed" {
		t.Fatalf("plain read with fast_scan_level 3 should not be rejected: %+v", rout)
	}
}

func TestSearchTagCaseInsensitive(t *testing.T) {
	dir := setupPhotos(t)
	app, _ := newTestApp(t, t.TempDir())
	q := &query.Node{Type: "predicate", Tag: "make", Operator: "eq", Value: "Canon"}
	_, out, err := app.searchMetadata(context.Background(), nil, SearchMetadataIn{
		Paths: []string{dir}, Query: q, ScanOptions: &ScanOptionsInput{MaxFiles: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.MatchedCount != 2 {
		t.Fatalf("lowercase tag should match both Canon photos: %+v", out)
	}
}

func TestDiscoverIntegration(t *testing.T) {
	dir := setupPhotos(t)
	app, _ := newTestApp(t, t.TempDir())
	ctx := context.Background()
	_, cat, err := app.discoverTags(ctx, nil, DiscoverTagsIn{Scope: "catalog", Query: "CreateDate"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range cat.Items {
		if it.Name == "CreateDate" && it.Source == "catalog" {
			found = true
		}
	}
	if !found || cat.TotalCount == 0 {
		t.Fatalf("catalog search failed: total=%d", cat.TotalCount)
	}
	_, obs, err := app.discoverTags(ctx, nil, DiscoverTagsIn{
		Scope: "present_in_files", Query: "Make", Paths: []string{dir}, Extensions: []string{"jpg"},
	})
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, it := range obs.Items {
		if it.Name == "Make" && it.Source == "observed" {
			if it.Count != 3 {
				t.Fatalf("Make should be observed in 3 files, got %d", it.Count)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("observed Make not found: %+v", obs.Items)
	}
}

func TestExtractBinaryIntegration(t *testing.T) {
	dir := setupPhotos(t)
	app, _ := newTestApp(t, t.TempDir())
	ctx := context.Background()
	a := filepath.Join(dir, "a.jpg")
	res, out, err := app.extractBinary(ctx, nil, ExtractBinaryIn{Path: a, Tag: "ThumbnailImage"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "complete" || out.Delivery != "image" {
		t.Fatalf("thumbnail delivery wrong: %+v", out)
	}
	if out.SHA256 != fmt.Sprintf("%x", sha256.Sum256(jpegBytes(t))) {
		t.Fatalf("thumbnail hash mismatch: %s", out.SHA256)
	}
	img, ok := res.Content[0].(*mcp.ImageContent)
	if !ok {
		t.Fatalf("first content is not image content: %T", res.Content[0])
	}
	sum := sha256.Sum256(img.Data)
	if sum != sha256.Sum256(jpegBytes(t)) {
		t.Fatal("image content bytes differ from fixture thumbnail")
	}
	// Absent binary tag: a structured failure, never an empty success.
	_, missing, err := app.extractBinary(ctx, nil, ExtractBinaryIn{Path: a, Tag: "ICC_Profile"})
	if err != nil {
		t.Fatal(err)
	}
	if missing.Status != "failed" || len(missing.Errors) == 0 || missing.Errors[0].Code != "BINARY_NOT_FOUND" {
		t.Fatalf("expected BINARY_NOT_FOUND, got %+v", missing)
	}
	// Wildcards are rejected.
	_, wild, err := app.extractBinary(ctx, nil, ExtractBinaryIn{Path: a, Tag: "EXIF:All"})
	if err != nil {
		t.Fatal(err)
	}
	if wild.Status != "failed" {
		t.Fatalf("group selector should be rejected, got %+v", wild)
	}
}

func TestPagingDoesNotInvokeExifTool(t *testing.T) {
	dir := setupPhotos(t)
	app, cr := newTestApp(t, t.TempDir())
	ctx := context.Background()
	_, out, err := app.readMetadata(ctx, nil, ReadMetadataIn{
		Paths:           []string{filepath.Join(dir, "a.jpg"), filepath.Join(dir, "b.jpg"), filepath.Join(dir, "c.jpg")},
		ResponseOptions: &ResponseOptionsInput{PageSize: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.HasMore || out.ResultID == "" {
		t.Fatalf("expected paged result: %+v", out)
	}
	callsBefore := cr.calls.Load()
	seen := map[string]bool{}
	seen[filepath.Base(out.Items[0].Path)] = true
	cursor := out.NextCursor
	for cursor != "" {
		_, page, err := app.getResultPage(ctx, nil, GetResultPageIn{ResultID: out.ResultID, Cursor: cursor, PageSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 {
			t.Fatalf("expected 1 record per page, got %d", len(page.Items))
		}
		var rec meta.FileResult
		if err := json.Unmarshal(page.Items[0], &rec); err != nil {
			t.Fatal(err)
		}
		if seen[filepath.Base(rec.Path)] {
			t.Fatalf("duplicate record across pages: %s", rec.Path)
		}
		seen[filepath.Base(rec.Path)] = true
		cursor = page.NextCursor
	}
	if len(seen) != 3 {
		t.Fatalf("expected 3 unique files across pages, got %v", seen)
	}
	if cr.calls.Load() != callsBefore {
		t.Fatal("get_result_page invoked exiftool")
	}
}

func TestSearchResumeCannotChangeTimeContext(t *testing.T) {
	dir := setupPhotos(t)
	storeDir := t.TempDir()
	ctx := context.Background()
	q := &query.Node{Type: "predicate", Tag: "Make", Operator: "eq", Value: "Canon"}

	// A scan started WITHOUT a time_context cannot gain one on
	// resume: later batches would judge datetimes by a rule the
	// earlier batches did not use.
	app1, _ := newTestApp(t, storeDir)
	_, out1, err := app1.searchMetadata(ctx, nil, SearchMetadataIn{
		Paths: []string{dir}, Query: q, ScanOptions: &ScanOptionsInput{MaxFiles: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, out2, err := app1.searchMetadata(ctx, nil, SearchMetadataIn{
		ResumeToken: out1.ResumeToken,
		TimeContext: &query.TimeContext{Timezone: "Asia/Jakarta"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out2.Status != "failed" || len(out2.Errors) == 0 || out2.Errors[0].Code != "INVALID_ARGUMENT" {
		t.Fatalf("adding a time_context on resume must be rejected: %+v", out2)
	}

	// A scan started WITH one cannot switch to another, but may
	// repeat the same one.
	app2, _ := newTestApp(t, storeDir)
	_, out3, err := app2.searchMetadata(ctx, nil, SearchMetadataIn{
		Paths: []string{dir}, Query: q, ScanOptions: &ScanOptionsInput{MaxFiles: 1},
		TimeContext: &query.TimeContext{Timezone: "Asia/Jakarta"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, out4, err := app2.searchMetadata(ctx, nil, SearchMetadataIn{
		ResumeToken: out3.ResumeToken,
		TimeContext: &query.TimeContext{Timezone: "UTC"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out4.Status != "failed" || len(out4.Errors) == 0 || out4.Errors[0].Code != "INVALID_ARGUMENT" {
		t.Fatalf("changing the time_context on resume must be rejected: %+v", out4)
	}
	_, out5, err := app2.searchMetadata(ctx, nil, SearchMetadataIn{
		ResumeToken: out3.ResumeToken,
		TimeContext: &query.TimeContext{Timezone: "Asia/Jakarta"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out5.Status == "failed" {
		t.Fatalf("repeating the same time_context must be accepted: %+v", out5)
	}
}

func TestSearchConcurrentResumeAcrossApps(t *testing.T) {
	// Two App instances over one store directory stand in for two
	// server processes sharing it: the store-level scan lock must
	// serialize their batches so no record is ever duplicated.
	dir := setupPhotos(t)
	storeDir := t.TempDir()
	ctx := context.Background()
	q := &query.Node{Type: "predicate", Tag: "Make", Operator: "eq", Value: "Canon"}
	app1, _ := newTestApp(t, storeDir)
	_, out1, err := app1.searchMetadata(ctx, nil, SearchMetadataIn{
		Paths: []string{dir}, Query: q, ScanOptions: &ScanOptionsInput{MaxFiles: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	app2, _ := newTestApp(t, storeDir)
	apps := []*App{app1, app2}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, _ = apps[i%2].searchMetadata(ctx, nil, SearchMetadataIn{
				ResumeToken: out1.ResumeToken, ScanOptions: &ScanOptionsInput{MaxFiles: 1},
			})
		}(i)
	}
	wg.Wait()
	job, err := app1.Store.LoadScan(out1.ResumeToken)
	if err != nil {
		t.Fatal(err)
	}
	if !job.Done || job.Processed != 4 || job.Matched != 2 || job.NoMatch != 1 || job.Errors != 1 {
		t.Fatalf("scan state after concurrent resumes: %+v", job)
	}
	m, err := app1.Store.LoadMeta(job.ResultID)
	if err != nil {
		t.Fatal(err)
	}
	if m.TotalRecords != 2 {
		t.Fatalf("expected exactly 2 records (no duplicates), got %d", m.TotalRecords)
	}
}

// stallRunner simulates a pathological ExifTool: multi-file reads —
// and reads of the corrupt fixture at any size — block until the
// context ends, while ordinary single-file reads answer at once.
type stallRunner struct{}

func (stallRunner) Path() string       { return "stall" }
func (stallRunner) ConfigMode() string { return "standard" }

func (stallRunner) Run(ctx context.Context, args ...string) (runner.Result, error) {
	var files []string
	for _, a := range args {
		if strings.HasSuffix(a, ".jpg") {
			files = append(files, a)
		}
	}
	stall := len(files) != 1
	if len(files) == 1 && strings.Contains(files[0], "corrupt") {
		stall = true
	}
	if stall {
		<-ctx.Done()
		return runner.Result{}, ctx.Err()
	}
	f := files[0]
	make_ := "Canon"
	if strings.Contains(f, "b.jpg") {
		make_ = "Nikon"
	}
	// Marshal properly: a Windows path's backslashes are not valid
	// JSON string escapes when concatenated by hand.
	payload, err := json.Marshal([]map[string]string{
		{"SourceFile": f, "EXIF:IFD0:Make": make_},
	})
	if err != nil {
		return runner.Result{}, err
	}
	return runner.Result{Stdout: payload}, nil
}

func TestSearchBatchTimeoutFallsBackPerFile(t *testing.T) {
	// The batch read times out because one file stalls forever.
	// The scan must still complete in a single call: healthy files
	// are read individually and the stalling file becomes a
	// per-file error instead of a permanent roadblock.
	dir := setupPhotos(t)
	storeDir := t.TempDir()
	cfg := config.Config{
		StoreDir: storeDir, CallTimeout: 2 * time.Second, MaxConcurrency: 4,
		MaxResponseBytes: 1 << 20, DefaultPageSize: 100, MaxInlineBinary: 1 << 20,
		ResultTTL: time.Hour, StoreQuotaBytes: 1 << 30,
		ScanBatchFiles: 200, ScanBatchTime: 30 * time.Second,
	}
	st, err := store.New(storeDir, cfg.ResultTTL, cfg.StoreQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	sr := stallRunner{}
	app := &App{Cfg: cfg, Runner: sr, Reader: &meta.Reader{Runner: sr}, Store: st}
	q := &query.Node{Type: "predicate", Tag: "Make", Operator: "eq", Value: "Canon"}
	_, out, err := app.searchMetadata(context.Background(), nil, SearchMetadataIn{
		Paths: []string{dir}, Query: q,
		ScanOptions: &ScanOptionsInput{MaxFiles: 4, MaxSeconds: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status == "failed" {
		t.Fatalf("batch timeout must not fail the scan: %+v", out.Errors)
	}
	if !out.ScanComplete || out.ScannedCount != 4 {
		t.Fatalf("scan should complete despite the stalling file: %+v", out)
	}
	if out.MatchedCount != 2 || out.NoMatchCount != 1 || out.ErrorCount != 1 {
		t.Fatalf("wrong counts after per-file fallback: %+v", out)
	}
}

func TestSearchEnumerationErrorKeepsStatusPartial(t *testing.T) {
	// One input path does not exist: the scan over the valid path
	// completes, but the final status must stay partial and the
	// enumeration error must be reported — part of the requested
	// input was never examined.
	dir := setupPhotos(t)
	app, _ := newTestApp(t, t.TempDir())
	ctx := context.Background()
	q := &query.Node{Type: "predicate", Tag: "Make", Operator: "eq", Value: "Canon"}
	_, out, err := app.searchMetadata(ctx, nil, SearchMetadataIn{
		Paths:       []string{dir, filepath.Join(dir, "does-not-exist")},
		Query:       q,
		ScanOptions: &ScanOptionsInput{MaxFiles: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.ScanComplete {
		t.Fatalf("scan over the valid path should complete: %+v", out)
	}
	if out.Status != "partial" {
		t.Fatalf("status must stay partial when enumeration failed, got %q", out.Status)
	}
	found := false
	for _, e := range out.Errors {
		if e.Code == "PATH_NOT_FOUND" {
			found = true
		}
	}
	if !found {
		t.Fatalf("enumeration error not reported: %+v", out.Errors)
	}
}

func TestReadOversizedValueStoreFailureIsLoud(t *testing.T) {
	// A value too large to inline is externalized to the store.
	// When that store write fails (here: a quota smaller than the
	// value), the read must fail loudly — leaving the value inline
	// would silently blow the response byte budget.
	requireExifTool(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "big.jpg")
	writeJPEG(t, p)
	// The 40 KB value travels in a file (-TAG<=file): passing it as
	// a command-line argument exceeds the Windows process limit.
	valueFile := filepath.Join(dir, "comment.txt")
	if err := os.WriteFile(valueFile, []byte(strings.Repeat("x", 40000)), 0o644); err != nil {
		t.Fatal(err)
	}
	cli(t, "-q", "-overwrite_original", "-Comment<="+valueFile, p)
	storeDir := t.TempDir()
	cfg := config.Config{
		StoreDir: storeDir, CallTimeout: 60 * time.Second, MaxConcurrency: 4,
		MaxResponseBytes: 1 << 20, DefaultPageSize: 100, MaxInlineBinary: 1 << 20,
		ResultTTL: time.Hour, StoreQuotaBytes: 2048,
		ScanBatchFiles: 200, ScanBatchTime: 30 * time.Second,
	}
	st, err := store.New(storeDir, cfg.ResultTTL, cfg.StoreQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	rn := runner.New(cfg)
	app := &App{Cfg: cfg, Runner: rn, Reader: &meta.Reader{Runner: rn}, Store: st}
	_, out, err := app.readMetadata(context.Background(), nil, ReadMetadataIn{
		Paths:          []string{p},
		MetadataFilter: &MetadataFilter{Mode: "select", IncludeTags: []string{"Comment"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "failed" || len(out.Errors) == 0 || out.Errors[0].Code != "RESULT_STORE_FULL" {
		t.Fatalf("oversized value with a full store must fail as RESULT_STORE_FULL, got %+v", out)
	}
}

func TestReaderCombinedGroupFamiliesRealExifTool(t *testing.T) {
	// Real-binary integration for the combined -G argument: with
	// include_duplicates (and with -ee), entries must keep their
	// real groups. Separate -G flags made ExifTool drop them
	// (keys came back as ":Make" / "Main:Make").
	requireExifTool(t)
	dir := t.TempDir()
	// A valid JPEG carrying two COM segments (duplicate comments)
	// plus a real Make tag.
	p := filepath.Join(dir, "dup.jpg")
	base := jpegBytes(t)
	var buf []byte
	buf = append(buf, base[:2]...)
	for _, s := range []string{"alpha", "beta"} {
		seg := []byte{0xFF, 0xFE, 0, byte(len(s) + 2)}
		seg = append(seg, s...)
		buf = append(buf, seg...)
	}
	buf = append(buf, base[2:]...)
	if err := os.WriteFile(p, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	cli(t, "-q", "-overwrite_original", "-Make=Canon", p)

	app, _ := newTestApp(t, t.TempDir())
	ctx := context.Background()
	read := func(spec meta.ReadSpec) []meta.Entry {
		res, err := app.Reader.Read(ctx, []string{p}, spec)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 1 || res[0].Status == "error" {
			t.Fatalf("read failed: %+v", res)
		}
		return res[0].Entries
	}
	spec := meta.ReadSpec{IncludeTags: []string{"Comment", "EXIF:Make"}, IncludeDuplicates: true}
	entries := read(spec)
	var comments []meta.Entry
	groups := map[string]bool{}
	for _, e := range entries {
		groups[e.Name+"|"+e.Group] = true
		if e.Name == "Comment" {
			comments = append(comments, e)
		}
		if e.Name == "Make" && !strings.HasPrefix(e.Group, "EXIF") {
			t.Errorf("Make lost its group under include_duplicates: %+v", e)
		}
	}
	if len(comments) != 2 {
		t.Fatalf("expected 2 Comment entries, got %d (%v)", len(comments), groups)
	}
	copies := map[string]bool{}
	for _, c := range comments {
		if c.Group != "File" {
			t.Errorf("Comment group = %q, want File", c.Group)
		}
		copies[c.Copy] = true
	}
	if !copies[""] || !copies["Copy1"] {
		t.Errorf("duplicate copies not distinguished: %v", copies)
	}
	// Embedded reads keep groups AND gain document identity.
	spec.EmbeddedLevel = 2
	for _, e := range read(spec) {
		if e.Name == "Make" {
			if !strings.HasPrefix(e.Group, "EXIF") {
				t.Errorf("Make lost its group under -ee: %+v", e)
			}
			if e.Document != "Main" {
				t.Errorf("Make document = %q, want Main", e.Document)
			}
		}
	}
}
