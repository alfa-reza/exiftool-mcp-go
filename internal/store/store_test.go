package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alfa-reza/exiftool-mcp-go/internal/meta"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir(), time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRecordsPagingReconstructs1000(t *testing.T) {
	s := newStore(t)
	var records []json.RawMessage
	for i := 0; i < 1000; i++ {
		records = append(records, json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)))
	}
	m, err := s.SaveRecords(records)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[int]bool)
	offset := 0
	for {
		_, page, err := s.RecordsPage(m.ID, offset, 77, 4096)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Records) == 0 && page.HasMore {
			t.Fatal("empty page with has_more=true")
		}
		for _, r := range page.Records {
			var v struct {
				N int `json:"n"`
			}
			if err := json.Unmarshal(r, &v); err != nil {
				t.Fatal(err)
			}
			if seen[v.N] {
				t.Fatalf("duplicate record %d", v.N)
			}
			seen[v.N] = true
		}
		offset = page.NextOffset
		if !page.HasMore {
			break
		}
	}
	if len(seen) != 1000 {
		t.Fatalf("reconstructed %d of 1000 records", len(seen))
	}
}

func TestRecordsPageByteBudgetAcrossSegments(t *testing.T) {
	// Three segments of one record each (A, B, C). A byte budget that
	// fits A but not B must return exactly [A] — never [A, C] with B
	// silently skipped — and paging must reconstruct A, B, C in order.
	s := newStore(t)
	m, err := s.CreateResult(KindRecords)
	if err != nil {
		t.Fatal(err)
	}
	recs := []string{`{"v":"AAA"}`, `{"v":"BBB"}`, `{"v":"CCC"}`}
	for _, r := range recs {
		if _, err := s.AppendRecords(m.ID, []json.RawMessage{json.RawMessage(r)}); err != nil {
			t.Fatal(err)
		}
	}
	budget := len(recs[0]) + 1 // fits one record, not two
	var got []string
	offset := 0
	for i := 0; i < 5; i++ {
		_, page, err := s.RecordsPage(m.ID, offset, 0, budget)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Records) != 1 {
			t.Fatalf("page %d: expected exactly 1 record, got %d (%v)", i, len(page.Records), page.Records)
		}
		got = append(got, string(page.Records[0]))
		offset = page.NextOffset
		if !page.HasMore {
			break
		}
	}
	if strings.Join(got, ",") != strings.Join(recs, ",") {
		t.Fatalf("reconstruction mismatch: got %v want %v", got, recs)
	}
	if offset != 3 {
		t.Fatalf("final offset = %d, want 3", offset)
	}
}

func TestQuotaEnforcedOnSave(t *testing.T) {
	s, err := New(t.TempDir(), time.Hour, 1000)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.SaveBlob(make([]byte, 600), "application/octet-stream")
	if err != nil {
		t.Fatalf("first blob should fit: %v", err)
	}
	// A second write that does not fit alongside the first evicts
	// the completed first result to make room instead of failing.
	second, err := s.SaveBlob(make([]byte, 600), "application/octet-stream")
	if err != nil {
		t.Fatalf("second blob should displace the first: %v", err)
	}
	if _, err := s.LoadMeta(first.ID); err == nil {
		t.Fatal("first blob should have been evicted")
	}
	if _, err := s.LoadMeta(second.ID); err != nil {
		t.Fatalf("second blob should be stored: %v", err)
	}
	// A payload larger than the whole quota can never fit, no
	// matter what is evicted.
	if _, err := s.SaveText(strings.Repeat("x", 1200)); !errors.Is(err, ErrFull) {
		t.Fatalf("payload larger than the quota should fail: %v", err)
	}
	if _, err := s.SaveText("tiny"); err != nil {
		t.Fatalf("small text should still fit: %v", err)
	}
	// Appends are enforced too (fresh store so cleanup eviction of
	// the completed results above cannot make room).
	s2, err := New(t.TempDir(), time.Hour, 1000)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s2.CreateResult(KindRecords)
	if err != nil {
		t.Fatal(err)
	}
	big := []json.RawMessage{json.RawMessage(strings.Repeat("y", 600))}
	if _, err := s2.AppendRecords(m.ID, big); err != nil {
		t.Fatalf("first append should fit: %v", err)
	}
	if _, err := s2.AppendRecords(m.ID, big); !errors.Is(err, ErrFull) {
		t.Fatalf("append past the quota should fail: %v", err)
	}
}

func TestTextFragmentAlwaysAdvances(t *testing.T) {
	// A byte budget smaller than the next UTF-8 code point must still
	// return that code point whole: an empty chunk with an unmoved
	// cursor would loop forever.
	s := newStore(t)
	text := "\U0001D11Eabc" // 4-byte rune + ASCII
	m, err := s.SaveText(text)
	if err != nil {
		t.Fatal(err)
	}
	_, chunk, next, err := s.Fragment(m.ID, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if string(chunk) != "\U0001D11E" || next != 4 {
		t.Fatalf("first fragment = %q next=%d, want the full rune and 4", chunk, next)
	}
	var out []byte
	off := int64(0)
	for i := 0; i < 20; i++ {
		_, chunk, next, err := s.Fragment(m.ID, off, 1)
		if err != nil {
			t.Fatal(err)
		}
		if next <= off {
			t.Fatalf("fragment did not advance at offset %d", off)
		}
		if !utf8.Valid(chunk) {
			t.Fatalf("fragment at %d is not valid UTF-8: %q", off, chunk)
		}
		out = append(out, chunk...)
		off = next
		if off >= int64(len(text)) {
			break
		}
	}
	if string(out) != text {
		t.Fatalf("reconstruction = %q, want %q", out, text)
	}
}

func TestBlobFragmentReconstruction(t *testing.T) {
	s := newStore(t)
	data := bytes.Repeat([]byte("0123456789abcdef"), 5000) // 80000 bytes
	m, err := s.SaveBlob(data, "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	var off int64
	for {
		_, chunk, next, err := s.Fragment(m.ID, off, 1000)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, chunk...)
		if next >= m.TotalBytes {
			break
		}
		off = next
	}
	if !bytes.Equal(out, data) {
		t.Fatal("fragment reconstruction mismatch")
	}
}

func TestTextFragmentUTF8Boundary(t *testing.T) {
	s := newStore(t)
	text := strings.Repeat("héllo→", 100) // multi-byte runes
	m, err := s.SaveText(text)
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	var off int64
	for off < m.TotalBytes {
		_, chunk, next, err := s.Fragment(m.ID, off, 7) // deliberately awkward size
		if err != nil {
			t.Fatal(err)
		}
		if !utf8Valid(chunk) {
			t.Fatalf("fragment at %d is not valid UTF-8", off)
		}
		out = append(out, chunk...)
		off = next
	}
	if string(out) != text {
		t.Fatal("text reconstruction mismatch")
	}
}

func utf8Valid(b []byte) bool { return utf8.Valid(b) }

func TestExpiryAndCorruption(t *testing.T) {
	s := newStore(t)
	m, err := s.SaveText("hello")
	if err != nil {
		t.Fatal(err)
	}
	// Expire it by moving the store clock forward.
	s.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, err := s.LoadMeta(m.ID); err != ErrExpired {
		t.Fatalf("expected ErrExpired, got %v", err)
	}
	s.now = time.Now
	// Corrupt the metadata.
	dir := s.resultDir(m.ID)
	if err := writeAtomic(dir+"/meta.json", []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadMeta(m.ID); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("expected corrupt error, got %v", err)
	}
	if _, err := s.LoadMeta("00000000000000000000000000000000"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestScanCheckpointRoundTrip(t *testing.T) {
	s := newStore(t)
	res, err := s.CreateResult(KindRecords)
	if err != nil {
		t.Fatal(err)
	}
	job := &ScanJob{
		ResultID:  res.ID,
		QueryJSON: json.RawMessage(`{"type":"predicate","tag":"Make","operator":"eq","value":"Canon"}`),
		Candidates: []meta.Candidate{
			{Path: "/tmp/a.jpg", Size: 10},
			{Path: "/tmp/b.jpg", Size: 20},
		},
	}
	if err := s.CreateScan(job); err != nil {
		t.Fatal(err)
	}
	job.NextIndex = 1
	job.Processed = 1
	job.Matched = 1
	job.CommittedRecords = 1
	if err := s.SaveScan(job); err != nil {
		t.Fatal(err)
	}
	// Simulate a restart: a fresh Store on the same directory.
	s2, err := New(s.dir, time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s2.LoadScan(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.NextIndex != 1 || loaded.Matched != 1 || len(loaded.Candidates) != 2 {
		t.Fatalf("checkpoint not preserved: %+v", loaded)
	}
	// Append + page across the restart boundary.
	if _, err := s2.AppendRecords(res.ID, []json.RawMessage{json.RawMessage(`{"path":"/tmp/a.jpg"}`)}); err != nil {
		t.Fatal(err)
	}
	_, page, err := s2.RecordsPage(res.ID, 0, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(page.Records))
	}
}

func TestQuotaEvictionMakesRoomForPendingWrite(t *testing.T) {
	s, err := New(t.TempDir(), time.Hour, 2048)
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.SaveText(strings.Repeat("x", 1500))
	if err != nil {
		t.Fatal(err)
	}
	// Usage is below the quota, but usage plus the new write is not.
	// The completed old result must be evicted to make room instead
	// of the write being rejected.
	if _, err := s.SaveText(strings.Repeat("y", 1000)); err != nil {
		t.Fatalf("write should have displaced the old completed result: %v", err)
	}
	if _, err := s.LoadMeta(old.ID); err == nil {
		t.Fatal("old completed result should have been evicted")
	}
	if got := s.TotalBytes(); got > 2048 {
		t.Fatalf("usage %d exceeds quota", got)
	}
}

func TestQuotaCountsScanCheckpoints(t *testing.T) {
	s, err := New(t.TempDir(), time.Hour, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	job := &ScanJob{ResultID: strings.Repeat("a", 32)}
	for i := 0; i < 300; i++ {
		job.Candidates = append(job.Candidates, meta.Candidate{Path: fmt.Sprintf("/photos/some/fairly/long/path/photo-%04d.jpg", i)})
	}
	if err := s.CreateScan(job); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.scanPath(job.ID))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() < 30000 {
		t.Fatalf("fixture too small: %d bytes", fi.Size())
	}
	if got := s.TotalBytes(); got < fi.Size() {
		t.Fatalf("TotalBytes %d does not include the %d-byte checkpoint", got, fi.Size())
	}
}

func TestConcurrentWritesRespectQuota(t *testing.T) {
	s, err := New(t.TempDir(), time.Hour, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("z", 8<<10)
	var wg sync.WaitGroup
	errs := make([]error, 16)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.SaveText(payload)
		}(i)
	}
	wg.Wait()
	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
		} else if !errors.Is(err, ErrFull) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if succeeded == 0 {
		t.Fatal("at least one write must fit")
	}
	if got := s.TotalBytes(); got > 64<<10 {
		t.Fatalf("concurrent writes overshot the quota: %d bytes", got)
	}
}

func TestFragmentAtEOFCompletesRune(t *testing.T) {
	s := newStore(t)
	m, err := s.SaveText("é")
	if err != nil {
		t.Fatal(err)
	}
	// A 1-byte budget cuts the 2-byte rune; the fragment must still
	// return the whole rune and advance. The completion read hits
	// EOF here, which used to discard the bytes it did read and
	// stall paging forever.
	_, chunk, next, err := s.Fragment(m.ID, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(chunk) != "é" || next != 2 {
		t.Fatalf("fragment = %q next %d, want %q next 2", chunk, next, "é")
	}
	if !utf8.Valid(chunk) {
		t.Fatal("fragment is not valid UTF-8")
	}
}
