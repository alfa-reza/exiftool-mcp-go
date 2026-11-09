package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
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
