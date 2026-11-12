package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

func TestScanLockReleaseRespectsOwnership(t *testing.T) {
	s := newStore(t)
	job := &ScanJob{ResultID: strings.Repeat("a", 32)}
	if err := s.CreateScan(job); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	relA, err := s.LockScan(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(s.dir, "scans", job.ID, "job.lock")
	// A's heartbeat stops (crash): the lock goes stale, B takes over.
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatal(err)
	}
	relB, err := s.LockScan(ctx, job.ID)
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	// A finally releases: it must NOT delete B's lock.
	relA()
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("stale owner's release deleted the successor's lock: %v", err)
	}
	// B still holds it: a third acquisition cannot proceed.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.LockScan(cctx, job.ID); err == nil {
		t.Fatal("third acquisition should fail while B holds the lock")
	}
	relB()
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lock file should be gone after the owner releases (stat err = %v)", err)
	}
}

func TestQuotaCountsMetadata(t *testing.T) {
	// meta.json is store data too: a quota sized for the payload
	// alone must reject the write instead of being overrun by the
	// metadata written alongside it.
	st, err := New(t.TempDir(), time.Hour, 128)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveText(strings.Repeat("x", 110)); !errors.Is(err, ErrFull) {
		t.Fatalf("SaveText(110B) at quota 128: err = %v, want ErrFull", err)
	}
	// CreateResult writes only a meta.json; even that must respect
	// the quota.
	st2, err := New(t.TempDir(), time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st2.CreateResult(KindRecords); !errors.Is(err, ErrFull) {
		t.Fatalf("CreateResult at quota 1: err = %v, want ErrFull", err)
	}
	// Successful writes stay within the quota afterwards.
	st3, err := New(t.TempDir(), time.Hour, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st3.SaveText("hello"); err != nil {
		t.Fatal(err)
	}
	if got := st3.TotalBytes(); got > 4096 {
		t.Fatalf("TotalBytes after small write = %d, over the 4096 quota", got)
	}
}

func TestQuotaAcrossInstances(t *testing.T) {
	// Two Store instances on one directory (two MCP servers
	// sharing EXIFTOOL_MCP_STORE): the capacity check and the
	// write must be atomic across processes too, not only under
	// one instance's mutex. Before the inter-process write lock,
	// concurrent writers overshot a 1300-byte quota to ~2388.
	for round := 0; round < 3; round++ {
		dir := t.TempDir()
		s1, err := New(dir, time.Hour, 1300)
		if err != nil {
			t.Fatal(err)
		}
		s2, err := New(dir, time.Hour, 1300)
		if err != nil {
			t.Fatal(err)
		}
		payload := strings.Repeat("x", 900)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = s1.SaveText(payload) }()
		go func() { defer wg.Done(); _, _ = s2.SaveText(payload) }()
		wg.Wait()
		if total := s1.TotalBytes(); total > 1300 {
			t.Fatalf("round %d: total = %d, over the 1300 quota", round, total)
		}
	}
}

func TestSaveRecordsNilCountsMeta(t *testing.T) {
	// An empty record set still writes a meta.json; the quota
	// must cover it.
	st, err := New(t.TempDir(), time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveRecords(nil); !errors.Is(err, ErrFull) {
		t.Fatalf("SaveRecords(nil) at quota 1: err = %v, want ErrFull", err)
	}
}

func TestFragmentAtExactEOF(t *testing.T) {
	// Offset == payload length is a valid empty final fragment:
	// no bytes, cursor unchanged, no error.
	st, err := New(t.TempDir(), time.Hour, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.SaveText("abc")
	if err != nil {
		t.Fatal(err)
	}
	_, chunk, next, err := st.Fragment(m.ID, 3, 10)
	if err != nil {
		t.Fatalf("Fragment at exact EOF: err = %v, want nil", err)
	}
	if len(chunk) != 0 || next != 3 {
		t.Fatalf("Fragment at exact EOF: chunk = %q next = %d, want empty and 3", chunk, next)
	}
}

func TestScanQuotaAcrossInstances(t *testing.T) {
	// Checkpoints count toward the quota, and their growth check
	// must be atomic across Store instances: before SaveScan took
	// the inter-process write lock, two instances saving ~50 KB
	// checkpoints concurrently overshot an 85 KB quota in most
	// rounds (111 KB actual).
	cands := make([]meta.Candidate, 600)
	for i := range cands {
		cands[i] = meta.Candidate{Path: fmt.Sprintf("/photos/dir/sub/img-%05d.jpg", i), Size: 1024, ModTime: time.Unix(1700000000, 0)}
	}
	for round := 0; round < 30; round++ {
		dir := t.TempDir()
		s1, err := New(dir, time.Hour, 85000)
		if err != nil {
			t.Fatal(err)
		}
		s2, err := New(dir, time.Hour, 85000)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_ = s1.CreateScan(&ScanJob{ResultID: strings.Repeat("a", 32), Candidates: cands})
		}()
		go func() {
			defer wg.Done()
			<-start
			_ = s2.CreateScan(&ScanJob{ResultID: strings.Repeat("b", 32), Candidates: cands})
		}()
		close(start)
		wg.Wait()
		if total := s1.TotalBytes(); total > 85000 {
			t.Fatalf("round %d: total = %d, over the 85000 quota", round, total)
		}
	}
}

func TestFragmentMissingPayloadAtEOF(t *testing.T) {
	// The end-of-payload shortcut must not hide a missing
	// payload file: corruption is corruption at any offset.
	st, err := New(t.TempDir(), time.Hour, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.SaveText("abc")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(st.Dir(), "results", m.ID, "text.txt")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.Fragment(m.ID, 3, 10); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Fragment at EOF with deleted payload: err = %v, want ErrCorrupt", err)
	}
	// And the intact case still returns the empty final chunk.
	st2, err := New(t.TempDir(), time.Hour, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := st2.SaveText("abc")
	if err != nil {
		t.Fatal(err)
	}
	_, chunk, next, err := st2.Fragment(m2.ID, 3, 10)
	if err != nil || len(chunk) != 0 || next != 3 {
		t.Fatalf("Fragment at EOF intact: chunk=%q next=%d err=%v", chunk, next, err)
	}
}

func TestSaveTextPendingSurvivesEviction(t *testing.T) {
	// A pending (incomplete) fragment is not an eviction
	// candidate: writes that would need its space fail instead
	// of removing it. After MarkComplete it becomes an ordinary
	// evictable result.
	st, err := New(t.TempDir(), time.Hour, 45000)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := st.SaveTextPending(strings.Repeat("a", 41000))
	if err != nil {
		t.Fatal(err)
	}
	if pending.Complete {
		t.Fatal("pending fragment reported complete")
	}
	big := []json.RawMessage{json.RawMessage(`{"data":"` + strings.Repeat("x", 30000) + `"}`)}
	if _, err := st.SaveRecords(big); !errors.Is(err, ErrFull) {
		t.Fatalf("SaveRecords beside a pending fragment: err = %v, want ErrFull (no eviction)", err)
	}
	if _, err := st.LoadMeta(pending.ID); err != nil {
		t.Fatalf("pending fragment vanished: %v", err)
	}
	if err := st.MarkComplete(pending.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveRecords(big); err != nil {
		t.Fatalf("SaveRecords after finalize: %v", err)
	}
	if _, err := st.LoadMeta(pending.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finalized fragment should have been evicted, err = %v", err)
	}
}

func TestRecordsPageRecordTooLarge(t *testing.T) {
	st, err := New(t.TempDir(), time.Hour, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	record := json.RawMessage(`{"data":"` + strings.Repeat("x", 3000) + `"}`)
	m, err := st.SaveRecords([]json.RawMessage{record})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RecordsPage(m.ID, 0, 10, 64); !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("oversized first record: err = %v, want ErrRecordTooLarge", err)
	}
	// A sufficient limit returns it whole.
	_, page, err := st.RecordsPage(m.ID, 0, 10, 4096)
	if err != nil || len(page.Records) != 1 {
		t.Fatalf("sufficient limit: records=%d err=%v", len(page.Records), err)
	}
	// Mid-page budgets still end the page normally (no error).
	small := []json.RawMessage{
		json.RawMessage(`{"n":"` + strings.Repeat("a", 90) + `"}`),
		json.RawMessage(`{"n":"` + strings.Repeat("b", 90) + `"}`),
	}
	m2, err := st.SaveRecords(small)
	if err != nil {
		t.Fatal(err)
	}
	_, page, err = st.RecordsPage(m2.ID, 0, 10, 150)
	if err != nil || len(page.Records) != 1 || !page.HasMore {
		t.Fatalf("mid-page budget: records=%d hasMore=%v err=%v, want 1 record + HasMore", len(page.Records), page.HasMore, err)
	}
}

func TestFragmentTruncatedPayload(t *testing.T) {
	// A payload whose size disagrees with the metadata is
	// corrupt at ANY offset, including the exact end: the EOF
	// shortcut must not bless a truncated file with an empty
	// success.
	st, err := New(t.TempDir(), time.Hour, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.SaveBlob([]byte("abcdef"), "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(st.Dir(), "results", m.ID, "blob.bin")
	if err := os.Truncate(blob, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.Fragment(m.ID, 6, 10); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Fragment at EOF on truncated payload: err = %v, want ErrCorrupt", err)
	}
	if _, _, _, err := st.Fragment(m.ID, 0, 3); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Fragment mid-payload on truncated payload: err = %v, want ErrCorrupt", err)
	}
}

func TestDiscardResult(t *testing.T) {
	st, err := New(t.TempDir(), time.Hour, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.SaveTextPending("payload")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DiscardResult(m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LoadMeta(m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after discard: err = %v, want ErrNotFound", err)
	}
	// Discarding an absent result is not an error.
	if err := st.DiscardResult(m.ID); err != nil {
		t.Fatalf("second discard: %v", err)
	}
}

func TestReferencedFragmentIsPinned(t *testing.T) {
	// An unreferenced fragment is evicted under quota pressure;
	// a fragment referenced by a surviving records result is
	// not — a value_ref stays valid as long as its referer.
	st, err := New(t.TempDir(), time.Hour, 90000)
	if err != nil {
		t.Fatal(err)
	}
	unref, err := st.SaveText(strings.Repeat("u", 41000))
	if err != nil {
		t.Fatal(err)
	}
	frag, err := st.SaveTextPending(strings.Repeat("a", 41000))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkComplete(frag.ID); err != nil {
		t.Fatal(err)
	}
	refRecord := json.RawMessage(`{"values":[{"value_ref":{"result_id":"` + frag.ID + `","kind":"text"}}]}`)
	if _, err := st.SaveRecords([]json.RawMessage{refRecord}); err != nil {
		t.Fatal(err)
	}
	big := []json.RawMessage{json.RawMessage(`{"data":"` + strings.Repeat("x", 30000) + `"}`)}
	if _, err := st.SaveRecords(big); err != nil {
		t.Fatalf("pressure write: %v", err)
	}
	if _, err := st.LoadMeta(unref.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unreferenced fragment: err = %v, want evicted (ErrNotFound)", err)
	}
	if _, err := st.LoadMeta(frag.ID); err != nil {
		t.Fatalf("referenced fragment was evicted while its referer survives: %v", err)
	}
}

func TestPinnedFragmentBlocksWriteUntilRefererDies(t *testing.T) {
	// When the only eviction candidate is pinned, the write
	// fails with ErrFull rather than breaking the reference.
	// Once the referer is gone, the fragment is evictable.
	st, err := New(t.TempDir(), time.Hour, 50000)
	if err != nil {
		t.Fatal(err)
	}
	frag, err := st.SaveTextPending(strings.Repeat("a", 41000))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkComplete(frag.ID); err != nil {
		t.Fatal(err)
	}
	refRecord := json.RawMessage(`{"values":[{"value_ref":{"result_id":"` + frag.ID + `","kind":"text"}}]}`)
	referer, err := st.SaveRecords([]json.RawMessage{refRecord})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveText(strings.Repeat("b", 20000)); !errors.Is(err, ErrFull) {
		t.Fatalf("write beside a pinned fragment: err = %v, want ErrFull", err)
	}
	if _, err := st.LoadMeta(frag.ID); err != nil {
		t.Fatalf("pinned fragment vanished: %v", err)
	}
	if err := st.DiscardResult(referer.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveText(strings.Repeat("b", 20000)); err != nil {
		t.Fatalf("write after the referer died: %v", err)
	}
	if _, err := st.LoadMeta(frag.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fragment after referer death: err = %v, want evicted (ErrNotFound)", err)
	}
}

func TestTruncateCrashSafety(t *testing.T) {
	// Each truncate writes a uniquely named segment: a crash
	// between the segment write and the meta save can then
	// only orphan the new file — the meta keeps pointing at
	// the intact previous segment. (With the old fixed name,
	// the same crash corrupted the result: meta claiming the
	// old record count over new, shorter content.)
	st, err := New(t.TempDir(), time.Hour, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	recs := []json.RawMessage{
		json.RawMessage(`{"n":1}`),
		json.RawMessage(`{"n":2}`),
		json.RawMessage(`{"n":3}`),
	}
	m, err := st.SaveRecords(recs)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.TruncateRecords(m.ID, 2); err != nil {
		t.Fatal(err)
	}
	meta1, err := st.LoadMeta(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(meta1.Segments) != 1 || !strings.HasPrefix(meta1.Segments[0].File, "seg-trunc-") {
		t.Fatalf("segments after first truncate: %+v, want one seg-trunc-* segment", meta1.Segments)
	}
	// A second truncate gets a DIFFERENT segment name.
	if err := st.TruncateRecords(m.ID, 1); err != nil {
		t.Fatal(err)
	}
	meta2, err := st.LoadMeta(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(meta2.Segments) != 1 || meta2.Segments[0].File == meta1.Segments[0].File {
		t.Fatalf("second truncate reused the segment name: %+v vs %+v", meta2.Segments, meta1.Segments)
	}
	_, page, err := st.RecordsPage(m.ID, 0, 10, 0)
	if err != nil || len(page.Records) != 1 {
		t.Fatalf("after two truncates: records=%d err=%v, want the 1 surviving record", len(page.Records), err)
	}
	// Simulate the crash state: an uncommitted seg-trunc-*
	// file exists while the meta points elsewhere. Reads are
	// unaffected, and the next truncate sweeps the orphan.
	orphan := filepath.Join(st.Dir(), "results", m.ID, "seg-trunc-crashsim.jsonl")
	if err := os.WriteFile(orphan, []byte("{\"n\":99}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RecordsPage(m.ID, 0, 10, 0); err != nil {
		t.Fatalf("RecordsPage with a crash orphan present: %v", err)
	}
	if err := st.TruncateRecords(m.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("crash orphan was not swept by the next truncate: %v", err)
	}
}
