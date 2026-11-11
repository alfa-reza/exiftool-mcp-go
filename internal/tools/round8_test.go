package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

// Round-8 regression tests: fragments created for value_refs are
// pending (incomplete) until the owning call commits, so a call's
// own commit writes can no longer evict them. Where the quota
// genuinely cannot hold a fragment and its referencing records
// together, the honest outcome is RESULT_STORE_FULL — never a
// successful response carrying a dead reference.

func TestCompareValueRefCommitOverflowFailsStoreFull(t *testing.T) {
	requireExifTool(t)
	dir := t.TempDir()
	a := filepath.Join(dir, "a.jpg")
	b := filepath.Join(dir, "b.jpg")
	writeJPEG(t, a)
	writeJPEG(t, b)
	big := writeTextValue(t, dir, "big.txt", strings.Repeat("a", 40960))
	cli(t, "-overwrite_original", "-Comment=one", "-XMP-dc:Description<="+big, a)
	cli(t, "-overwrite_original", "-Comment=two", "-XMP-dc:Description=small", b)
	// The fragment is ~41.1 KB and the quota is 41,500: before
	// the pending lifecycle, SaveRecords (committed after the
	// old verify step) evicted the fragment to make room and
	// the tool still reported complete with a dead reference.
	// The fragment plus its records cannot fit this quota at
	// all, so the call must now fail store-full instead.
	app := smallQuotaApp(t, 41500)
	_, out, err := app.compareMetadata(context.Background(), nil, CompareMetadataIn{
		LeftPath: a, RightPath: b,
		ResponseOptions: &ResponseOptionsInput{PageSize: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "failed" || len(out.Errors) == 0 || out.Errors[0].Code != "RESULT_STORE_FULL" {
		t.Fatalf("status=%s errors=%v, want failed RESULT_STORE_FULL (was: complete with a dead ref)", out.Status, out.Errors)
	}
}

func TestSearchValueRefCommitOverflowFailsStoreFull(t *testing.T) {
	requireExifTool(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "one.jpg")
	writeJPEG(t, p)
	big := writeTextValue(t, t.TempDir(), "big.txt", strings.Repeat("a", 41300))
	cli(t, "-overwrite_original", "-Comment<="+big, p)
	// Fragment ~41.5 KB against a 42,600 quota: verification
	// passed, then the batch commit (records + checkpoint)
	// evicted the fragment and the scan still reported
	// complete. Fragment plus records cannot fit this quota,
	// so the batch must now fail store-full instead.
	app := smallQuotaApp(t, 42600)
	_, out, err := app.searchMetadata(context.Background(), nil, SearchMetadataIn{
		Paths:      []string{dir},
		Query:      &query.Node{Type: "predicate", Tag: "Comment", Operator: "exists"},
		ReturnTags: []string{"Comment"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "failed" || len(out.Errors) == 0 || out.Errors[0].Code != "RESULT_STORE_FULL" {
		t.Fatalf("status=%s errors=%v, want failed RESULT_STORE_FULL (was: complete with a dead ref)", out.Status, out.Errors)
	}
}

func TestSearchPaginationIsExplicit(t *testing.T) {
	requireExifTool(t)
	photos := setupPhotos(t)
	// Page size 1 with two matches: the response must say the
	// page was cut and hand over a cursor into the scan result,
	// instead of silently returning one item.
	cfg := config.Config{
		StoreDir: t.TempDir(), CallTimeout: 60 * time.Second, MaxConcurrency: 4,
		MaxResponseBytes: 1 << 20, DefaultPageSize: 1, MaxInlineBinary: 1 << 20,
		ResultTTL: time.Hour, StoreQuotaBytes: 1 << 30, ScanBatchFiles: 200, ScanBatchTime: 30 * time.Second,
	}
	rn := runner.New(cfg)
	st, err := store.New(cfg.StoreDir, cfg.ResultTTL, cfg.StoreQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{Cfg: cfg, Runner: rn, Reader: &meta.Reader{Runner: rn}, Store: st}
	_, out, err := app.searchMetadata(context.Background(), nil, SearchMetadataIn{
		Paths: []string{photos},
		Query: &query.Node{Type: "predicate", Tag: "Make", Operator: "eq", Value: "Canon"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.MatchedCount != 2 || len(out.Items) != 1 {
		t.Fatalf("matched=%d items=%d, want 2 matched and a 1-item page", out.MatchedCount, len(out.Items))
	}
	if !out.HasMore || out.NextCursor == "" {
		t.Fatalf("has_more=%v next_cursor=%q, want an explicit continuation", out.HasMore, out.NextCursor)
	}
	off, err := decodeCursor(out.NextCursor)
	if err != nil || off != 1 {
		t.Fatalf("next_cursor decodes to %d (err %v), want offset 1", off, err)
	}
	// The cursor continues the RESULT via get_result_page.
	_, page, err := app.getResultPage(context.Background(), nil, GetResultPageIn{ResultID: out.ResultID, Cursor: out.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.HasMore {
		t.Fatalf("continuation page: items=%d hasMore=%v, want the last record", len(page.Items), page.HasMore)
	}
}

func TestDecodeCursorStrict(t *testing.T) {
	// Round-trips of genuine cursors keep working.
	for _, want := range []int{0, 1, 7, 12345} {
		got, err := decodeCursor(encodeCursor(want))
		if err != nil || got != want {
			t.Fatalf("decodeCursor(encodeCursor(%d)) = %d, %v", want, got, err)
		}
	}
	if got, err := decodeCursor(""); err != nil || got != 0 {
		t.Fatalf("empty cursor = %d, %v, want 0, nil", got, err)
	}
	// Anything that is not exactly "o:" + digits is invalid; a
	// scanning parse used to accept all of these as offset 1.
	for _, s := range []string{"o:1junk", "o:1:2", "o:1.0", "o:", "o:-3", "x:5", "o: 1", "o:1 ", "o:99999999999999999999999999"} {
		enc := base64.RawURLEncoding.EncodeToString([]byte(s))
		if off, err := decodeCursor(enc); err == nil {
			t.Fatalf("decodeCursor(%q) = %d with no error, want invalid cursor", s, off)
		}
	}
}

func TestGetResultPageRecordTooLarge(t *testing.T) {
	app, _ := newTestApp(t, t.TempDir())
	record := []byte(`{"data":"` + strings.Repeat("x", 3000) + `"}`)
	m, err := app.Store.SaveRecords([]json.RawMessage{record})
	if err != nil {
		t.Fatal(err)
	}
	_, out, err := app.getResultPage(context.Background(), nil, GetResultPageIn{ResultID: m.ID, MaxBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "failed" || len(out.Errors) == 0 || out.Errors[0].Code != "INVALID_ARGUMENT" {
		t.Fatalf("status=%s errors=%v, want failed INVALID_ARGUMENT (was: the 3 KB record returned whole)", out.Status, out.Errors)
	}
	if !strings.Contains(out.Errors[0].Message, "at least") {
		t.Fatalf("error should state the minimum limit required: %q", out.Errors[0].Message)
	}
}

func TestSearchValueRefFinalizedWhenQuotaFits(t *testing.T) {
	requireExifTool(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "one.jpg")
	writeJPEG(t, p)
	big := writeTextValue(t, t.TempDir(), "big.txt", strings.Repeat("a", 41300))
	cli(t, "-overwrite_original", "-Comment<="+big, p)
	// With room for the fragment and its records, the scan
	// completes and the fragment is finalized: a normal,
	// complete, readable result.
	app := smallQuotaApp(t, 1<<20)
	_, out, err := app.searchMetadata(context.Background(), nil, SearchMetadataIn{
		Paths:      []string{dir},
		Query:      &query.Node{Type: "predicate", Tag: "Comment", Operator: "exists"},
		ReturnTags: []string{"Comment"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "complete" || out.MatchedCount != 1 {
		t.Fatalf("status=%s matched=%d errors=%v, want complete with 1 match", out.Status, out.MatchedCount, out.Errors)
	}
	refs := 0
	for _, rec := range out.Items {
		for _, e := range rec.Values {
			if e.ValueRef == nil {
				continue
			}
			refs++
			m, err := app.Store.LoadMeta(e.ValueRef.ResultID)
			if err != nil {
				t.Fatalf("value_ref dead after commit: %v", err)
			}
			if !m.Complete {
				t.Fatal("fragment was not finalized to complete after the commit")
			}
		}
	}
	if refs == 0 {
		t.Fatal("no value_refs were produced; the fixture did not exercise externalization")
	}
}
