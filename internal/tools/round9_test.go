package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alfa-reza/exiftool-mcp-go/internal/config"
	"github.com/alfa-reza/exiftool-mcp-go/internal/meta"
	"github.com/alfa-reza/exiftool-mcp-go/internal/query"
	"github.com/alfa-reza/exiftool-mcp-go/internal/runner"
	"github.com/alfa-reza/exiftool-mcp-go/internal/store"
)

// countTextResults reports how many text-kind results (value
// fragments) currently exist in the app's store, complete or not.
func countTextResults(t *testing.T, app *App) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(app.Cfg.StoreDir, "results"))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if m, err := app.Store.LoadMeta(e.Name()); err == nil && m.Kind == store.KindText {
			n++
		}
	}
	return n
}

// Round-9 regression tests.

func TestSearchRecordOverBudgetIsNotReturnedWhole(t *testing.T) {
	requireExifTool(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "one.jpg")
	writeJPEG(t, p)
	big := writeTextValue(t, t.TempDir(), "big.txt", strings.Repeat("a", 13000))
	cli(t, "-overwrite_original", "-Comment<="+big, p)
	cfg := config.Config{
		StoreDir: t.TempDir(), CallTimeout: 60 * time.Second, MaxConcurrency: 4,
		MaxResponseBytes: 1024, DefaultPageSize: 100, MaxInlineBinary: 1 << 20,
		ResultTTL: time.Hour, StoreQuotaBytes: 1 << 30, ScanBatchFiles: 200, ScanBatchTime: 30 * time.Second,
	}
	rn := runner.New(cfg)
	st, err := store.New(cfg.StoreDir, cfg.ResultTTL, cfg.StoreQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{Cfg: cfg, Runner: rn, Reader: &meta.Reader{Runner: rn}, Store: st}
	_, out, err := app.searchMetadata(context.Background(), nil, SearchMetadataIn{
		Paths:      []string{dir},
		Query:      &query.Node{Type: "predicate", Tag: "Comment", Operator: "exists"},
		ReturnTags: []string{"Comment"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The 13 KB record must not come back against a 1,024-byte
	// response budget: the page is empty, the match is counted,
	// and the cursor points at the record in the stored result.
	if len(out.Items) != 0 {
		t.Fatalf("items=%d, want 0 (the oversize record must not be returned whole)", len(out.Items))
	}
	if out.MatchedCount != 1 || !out.HasMore || out.NextCursor == "" {
		t.Fatalf("matched=%d has_more=%v cursor=%q, want 1 + an explicit continuation", out.MatchedCount, out.HasMore, out.NextCursor)
	}
	off, err := decodeCursor(out.NextCursor)
	if err != nil || off != 0 {
		t.Fatalf("cursor decodes to %d (err %v), want 0 (the first record of the result)", off, err)
	}
	_, page, err := app.getResultPage(context.Background(), nil, GetResultPageIn{ResultID: out.ResultID, Cursor: out.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.HasMore {
		t.Fatalf("continuation page: items=%d hasMore=%v, want the stored record", len(page.Items), page.HasMore)
	}
}

func TestReadFailureDiscardsPendingFragments(t *testing.T) {
	requireExifTool(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "one.jpg")
	writeJPEG(t, p)
	big := writeTextValue(t, t.TempDir(), "big.txt", strings.Repeat("a", 41000))
	cli(t, "-overwrite_original", "-Comment<="+big, p)
	app := smallQuotaApp(t, 45000)
	// The first call fails AFTER externalizing (a single record
	// cannot fit a 100-byte budget even as a reference). Its
	// pending fragment must be discarded, not left to occupy
	// quota until the TTL.
	_, out1, err := app.readMetadata(context.Background(), nil, ReadMetadataIn{
		Paths:           []string{p},
		ResponseOptions: &ResponseOptionsInput{MaxResponseBytes: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out1.Status != "failed" || len(out1.Errors) == 0 || out1.Errors[0].Code != "INVALID_ARGUMENT" {
		t.Fatalf("call 1: status=%s errors=%v, want failed INVALID_ARGUMENT", out1.Status, out1.Errors)
	}
	if n := countTextResults(t, app); n != 0 {
		t.Fatalf("%d text fragments leaked from the failed call, want 0", n)
	}
	// The retry with a sane budget succeeds (it used to degrade
	// to RESULT_STORE_FULL on the leaked fragment's quota).
	_, out2, err := app.readMetadata(context.Background(), nil, ReadMetadataIn{Paths: []string{p}})
	if err != nil {
		t.Fatal(err)
	}
	if out2.Status != "complete" {
		t.Fatalf("retry: status=%s errors=%v, want complete", out2.Status, out2.Errors)
	}
}

func TestSearchFailureDiscardsPendingFragments(t *testing.T) {
	requireExifTool(t)
	dir := t.TempDir()
	// Two matched files, each with a ~41 KB value: the batch
	// externalizes the first, then the second fragment cannot
	// fit and the batch fails — the first fragment must be
	// discarded, not leaked as an unevictable pending result.
	for _, name := range []string{"one.jpg", "two.jpg"} {
		p := filepath.Join(dir, name)
		writeJPEG(t, p)
		v := writeTextValue(t, t.TempDir(), "v.txt", strings.Repeat("a", 40960))
		cli(t, "-overwrite_original", "-Comment<="+v, p)
	}
	app := smallQuotaApp(t, 45000) // room for ONE ~41 KB fragment, not two
	_, out, err := app.searchMetadata(context.Background(), nil, SearchMetadataIn{
		Paths:      []string{dir},
		Query:      &query.Node{Type: "predicate", Tag: "Comment", Operator: "exists"},
		ReturnTags: []string{"Comment"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "failed" || len(out.Errors) == 0 || out.Errors[0].Code != "RESULT_STORE_FULL" {
		t.Fatalf("status=%s errors=%v, want failed RESULT_STORE_FULL", out.Status, out.Errors)
	}
	if n := countTextResults(t, app); n != 0 {
		t.Fatalf("%d text fragments leaked from the failed batch, want 0", n)
	}
}

func TestInfoTimeoutIsNotCodedNotFound(t *testing.T) {
	cfg := config.Config{
		StoreDir: t.TempDir(), CallTimeout: 60 * time.Second, MaxConcurrency: 4,
		MaxResponseBytes: 1 << 20, DefaultPageSize: 100, MaxInlineBinary: 1 << 20,
		ResultTTL: time.Hour, StoreQuotaBytes: 1 << 30, ScanBatchFiles: 200, ScanBatchTime: 30 * time.Second,
	}
	st, err := store.New(cfg.StoreDir, cfg.ResultTTL, cfg.StoreQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	// failRunner's Run always returns context.DeadlineExceeded.
	app := &App{Cfg: cfg, Runner: failRunner{}, Reader: &meta.Reader{Runner: failRunner{}}, Store: st}
	_, out, err := app.getExiftoolInfo(context.Background(), nil, ExiftoolInfoIn{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Errors) == 0 || out.Errors[0].Code != "TIMEOUT" {
		t.Fatalf("errors=%v, want a TIMEOUT classification (was EXIFTOOL_NOT_FOUND for every failure)", out.Errors)
	}
	if out.Status != "unavailable" {
		t.Fatalf("status=%s, want unavailable", out.Status)
	}
}

func TestCompareFailureDiscardsPendingFragments(t *testing.T) {
	requireExifTool(t)
	dir := t.TempDir()
	a := filepath.Join(dir, "a.jpg")
	b := filepath.Join(dir, "b.jpg")
	writeJPEG(t, a)
	writeJPEG(t, b)
	big := writeTextValue(t, dir, "big.txt", strings.Repeat("a", 40960))
	cli(t, "-overwrite_original", "-XMP-dc:Description<="+big, a)
	cli(t, "-overwrite_original", "-XMP-dc:Description=small", b)
	app := smallQuotaApp(t, 1<<20)
	_, out, err := app.compareMetadata(context.Background(), nil, CompareMetadataIn{
		LeftPath: a, RightPath: b,
		ResponseOptions: &ResponseOptionsInput{MaxResponseBytes: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "failed" || len(out.Errors) == 0 || out.Errors[0].Code != "INVALID_ARGUMENT" {
		t.Fatalf("status=%s errors=%v, want failed INVALID_ARGUMENT", out.Status, out.Errors)
	}
	if n := countTextResults(t, app); n != 0 {
		t.Fatalf("%d text fragments leaked from the failed compare, want 0", n)
	}
}
