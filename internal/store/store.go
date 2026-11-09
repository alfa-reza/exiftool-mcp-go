// Package store persists tool results and search checkpoints on local
// disk so that paging and scan resume survive server restarts.
//
// Layout under the store directory:
//
//	results/<id>/meta.json      result metadata (source of truth)
//	results/<id>/seg-NNNNNN.jsonl  committed record segments (kind=records)
//	results/<id>/blob.bin       binary payload (kind=binary)
//	results/<id>/text.txt       text payload (kind=text)
//	scans/<id>/job.json         search job checkpoint
//
// All metadata writes are atomic (write temp file, then rename). A
// segment that is not referenced by meta.json is ignored, so a crash
// mid-batch can never produce a half-committed batch.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alfa-reza/exiftool-mcp-go/internal/meta"
)

// Error codes surfaced to tool handlers.
var (
	ErrNotFound = errors.New("result not found")
	ErrExpired  = errors.New("result expired")
	ErrCorrupt  = errors.New("stored data corrupt")
	ErrFull     = errors.New("result store quota exceeded")
)

// Kind classifies a stored result.
type Kind string

const (
	KindRecords Kind = "records"
	KindText    Kind = "text_fragment"
	KindBinary  Kind = "binary_fragment"
)

// Segment describes one committed record segment file.
type Segment struct {
	File    string `json:"file"`
	Records int    `json:"records"`
	Bytes   int64  `json:"bytes"`
}

// ResultMeta is the metadata of one stored result.
type ResultMeta struct {
	ID           string    `json:"id"`
	Kind         Kind      `json:"kind"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	TotalRecords int       `json:"total_records"`
	TotalBytes   int64     `json:"total_bytes"`
	Complete     bool      `json:"complete"`
	MIME         string    `json:"mime,omitempty"`
	SHA256       string    `json:"sha256,omitempty"`
	Segments     []Segment `json:"segments,omitempty"`
}

// ScanJob is the persisted checkpoint of one search scan.
type ScanJob struct {
	ID         string           `json:"id"`
	ResultID   string           `json:"result_id"`
	QueryJSON  json.RawMessage  `json:"query"`
	SpecJSON   json.RawMessage  `json:"spec"`
	ReturnTags []string         `json:"return_tags,omitempty"`
	CreatedAt  time.Time        `json:"created_at"`
	ExpiresAt  time.Time        `json:"expires_at"`
	Candidates []meta.Candidate `json:"candidates"`
	NextIndex  int              `json:"next_index"`
	// CommittedRecords is the number of result records written by
	// committed batches. On resume it is reconciled with the result
	// store: a mismatch means a crash mid-commit and is reported as
	// CHECKPOINT_CORRUPT instead of silently duplicating records.
	CommittedRecords int  `json:"committed_records"`
	Processed        int  `json:"processed"`
	Matched          int  `json:"matched"`
	NoMatch          int  `json:"no_match"`
	Indet            int  `json:"indeterminate"`
	Errors           int  `json:"errors"`
	Done             bool `json:"done"`
}

// Store is a disk-backed result store.
type Store struct {
	dir   string
	ttl   time.Duration
	quota int64
	mu    sync.Mutex
	now   func() time.Time
}

// New creates (if needed) the store directory with private permissions.
func New(dir string, ttl time.Duration, quota int64) (*Store, error) {
	for _, sub := range []string{"results", "scans"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, err
		}
	}
	return &Store{dir: dir, ttl: ttl, quota: quota, now: time.Now}, nil
}

// Dir returns the store root directory.
func (s *Store) Dir() string { return s.dir }

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func (s *Store) resultDir(id string) string {
	return filepath.Join(s.dir, "results", id)
}

func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Store) saveMeta(m *ResultMeta) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.resultDir(m.ID), "meta.json"), data)
}

// LoadMeta reads and validates a result's metadata.
func (s *Store) LoadMeta(id string) (*ResultMeta, error) {
	if !validID(id) {
		return nil, ErrNotFound
	}
	data, err := os.ReadFile(filepath.Join(s.resultDir(id), "meta.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var m ResultMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if s.now().After(m.ExpiresAt) {
		return nil, ErrExpired
	}
	return &m, nil
}

func (s *Store) newMeta(kind Kind) *ResultMeta {
	now := s.now()
	return &ResultMeta{
		ID:        newID(),
		Kind:      kind,
		CreatedAt: now,
		ExpiresAt: now.Add(s.ttl),
		Complete:  true,
	}
}

// SaveRecords stores a complete list of JSON records.
func (s *Store) SaveRecords(records []json.RawMessage) (*ResultMeta, error) {
	m := s.newMeta(KindRecords)
	if err := os.MkdirAll(s.resultDir(m.ID), 0o700); err != nil {
		return nil, err
	}
	if _, err := s.appendSegment(m, records); err != nil {
		return nil, err
	}
	if err := s.saveMeta(m); err != nil {
		return nil, err
	}
	return m, nil
}

// CreateResult creates an empty, incomplete result (used by scans that
// append committed batches over time).
func (s *Store) CreateResult(kind Kind) (*ResultMeta, error) {
	m := s.newMeta(kind)
	m.Complete = false
	if err := os.MkdirAll(s.resultDir(m.ID), 0o700); err != nil {
		return nil, err
	}
	if err := s.saveMeta(m); err != nil {
		return nil, err
	}
	return m, nil
}

// appendSegment writes one segment file and updates meta in memory.
// The caller persists meta afterwards; an unreferenced segment file is
// ignored after a crash.
func (s *Store) appendSegment(m *ResultMeta, records []json.RawMessage) (string, error) {
	if len(records) == 0 {
		return "", nil
	}
	name := fmt.Sprintf("seg-%06d.jsonl", len(m.Segments)+1)
	var buf strings.Builder
	var bytesCount int64
	for _, rec := range records {
		buf.Write(rec)
		buf.WriteByte('\n')
		bytesCount += int64(len(rec)) + 1
	}
	if err := writeAtomic(filepath.Join(s.resultDir(m.ID), name), []byte(buf.String())); err != nil {
		return "", err
	}
	m.Segments = append(m.Segments, Segment{File: name, Records: len(records), Bytes: bytesCount})
	m.TotalRecords += len(records)
	m.TotalBytes += bytesCount
	return name, nil
}

// AppendRecords atomically appends a committed batch to a result.
func (s *Store) AppendRecords(id string, records []json.RawMessage) (*ResultMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.LoadMeta(id)
	if err != nil {
		return nil, err
	}
	if _, err := s.appendSegment(m, records); err != nil {
		return nil, err
	}
	if err := s.saveMeta(m); err != nil {
		return nil, err
	}
	return m, nil
}

// MarkComplete flags a result as fully written.
func (s *Store) MarkComplete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.LoadMeta(id)
	if err != nil {
		return err
	}
	m.Complete = true
	return s.saveMeta(m)
}

// SaveBlob stores a binary payload.
func (s *Store) SaveBlob(data []byte, mime string) (*ResultMeta, error) {
	m := s.newMeta(KindBinary)
	if err := os.MkdirAll(s.resultDir(m.ID), 0o700); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	m.SHA256 = hex.EncodeToString(sum[:])
	m.MIME = mime
	m.TotalBytes = int64(len(data))
	if err := writeAtomic(filepath.Join(s.resultDir(m.ID), "blob.bin"), data); err != nil {
		return nil, err
	}
	if err := s.saveMeta(m); err != nil {
		return nil, err
	}
	return m, nil
}

// SaveText stores a text payload.
func (s *Store) SaveText(text string) (*ResultMeta, error) {
	m := s.newMeta(KindText)
	if err := os.MkdirAll(s.resultDir(m.ID), 0o700); err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(text))
	m.SHA256 = hex.EncodeToString(sum[:])
	m.TotalBytes = int64(len(text))
	if err := writeAtomic(filepath.Join(s.resultDir(m.ID), "text.txt"), []byte(text)); err != nil {
		return nil, err
	}
	if err := s.saveMeta(m); err != nil {
		return nil, err
	}
	return m, nil
}

// Page is one page of records.
type Page struct {
	Records    []json.RawMessage
	NextOffset int
	HasMore    bool
}

// RecordsPage returns up to pageSize records starting at offset,
// bounded by maxBytes of payload (a single oversized record is still
// returned alone so no record is ever lost).
func (s *Store) RecordsPage(id string, offset, pageSize int, maxBytes int) (*ResultMeta, *Page, error) {
	m, err := s.LoadMeta(id)
	if err != nil {
		return nil, nil, err
	}
	if m.Kind != KindRecords {
		return nil, nil, fmt.Errorf("%w: result %s holds %s, not records", ErrCorrupt, id, m.Kind)
	}
	if offset < 0 || offset > m.TotalRecords {
		return nil, nil, fmt.Errorf("%w: offset %d out of range", ErrCorrupt, offset)
	}
	page := &Page{}
	skip := offset
	var used int
	for _, seg := range m.Segments {
		if pageSize > 0 && len(page.Records) >= pageSize {
			break
		}
		if seg.Records <= skip {
			skip -= seg.Records
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.resultDir(id), seg.File))
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		if len(lines) != seg.Records {
			return nil, nil, fmt.Errorf("%w: segment %s record count mismatch", ErrCorrupt, seg.File)
		}
		for _, line := range lines {
			if skip > 0 {
				skip--
				continue
			}
			if pageSize > 0 && len(page.Records) >= pageSize {
				break
			}
			if maxBytes > 0 && len(page.Records) > 0 && used+len(line) > maxBytes {
				break
			}
			page.Records = append(page.Records, json.RawMessage(line))
			used += len(line)
		}
	}
	page.NextOffset = offset + len(page.Records)
	page.HasMore = page.NextOffset < m.TotalRecords
	return m, page, nil
}

// Fragment returns up to maxBytes of a text or binary payload starting
// at byte offset. For text, the cut never splits a UTF-8 code point.
func (s *Store) Fragment(id string, offset int64, maxBytes int) (*ResultMeta, []byte, int64, error) {
	m, err := s.LoadMeta(id)
	if err != nil {
		return nil, nil, 0, err
	}
	var file string
	switch m.Kind {
	case KindBinary:
		file = "blob.bin"
	case KindText:
		file = "text.txt"
	default:
		return nil, nil, 0, fmt.Errorf("%w: result %s holds records, not a fragment payload", ErrCorrupt, id)
	}
	if offset < 0 || offset > m.TotalBytes {
		return nil, nil, 0, fmt.Errorf("%w: offset %d out of range", ErrCorrupt, offset)
	}
	f, err := os.Open(filepath.Join(s.resultDir(id), file))
	if err != nil {
		return nil, nil, 0, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	defer f.Close()
	if _, err := f.Seek(offset, 0); err != nil {
		return nil, nil, 0, err
	}
	buf := make([]byte, maxBytes)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return nil, nil, 0, err
	}
	chunk := buf[:n]
	if m.Kind == KindText {
		chunk = trimToUTF8Boundary(chunk, offset+int64(n) < m.TotalBytes)
	}
	return m, chunk, offset + int64(len(chunk)), nil
}

// trimToUTF8Boundary drops a trailing partial UTF-8 sequence when more
// data follows, so text fragments always decode cleanly.
func trimToUTF8Boundary(b []byte, more bool) []byte {
	if !more || len(b) == 0 {
		return b
	}
	i := len(b) - 1
	for i >= 0 && i >= len(b)-4 {
		c := b[i]
		if c >= 0xC0 {
			need := 2
			if c >= 0xF0 {
				need = 4
			} else if c >= 0xE0 {
				need = 3
			}
			if len(b)-i < need {
				return b[:i]
			}
			return b
		}
		if c < 0x80 {
			return b
		}
		i--
	}
	return b
}

// --- Scan jobs ---

func (s *Store) scanPath(id string) string {
	return filepath.Join(s.dir, "scans", id, "job.json")
}

// CreateScan persists a new scan job.
func (s *Store) CreateScan(job *ScanJob) error {
	job.ID = newID()
	now := s.now()
	job.CreatedAt = now
	job.ExpiresAt = now.Add(s.ttl)
	if err := os.MkdirAll(filepath.Join(s.dir, "scans", job.ID), 0o700); err != nil {
		return err
	}
	return s.SaveScan(job)
}

// SaveScan atomically persists a scan job checkpoint.
func (s *Store) SaveScan(job *ScanJob) error {
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return writeAtomic(s.scanPath(job.ID), data)
}

// LoadScan reads a scan job checkpoint.
func (s *Store) LoadScan(id string) (*ScanJob, error) {
	if !validID(id) {
		return nil, ErrNotFound
	}
	data, err := os.ReadFile(s.scanPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var job ScanJob
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if s.now().After(job.ExpiresAt) {
		return nil, ErrExpired
	}
	return &job, nil
}

// Cleanup removes expired results and scans, then enforces the disk
// quota by deleting the oldest completed results first. It returns the
// number of removed entries.
func (s *Store) Cleanup() (int, error) {
	removed := 0
	now := s.now()
	type aged struct {
		id      string
		created time.Time
		size    int64
		done    bool
	}
	var results []aged
	var total int64
	entries, _ := os.ReadDir(filepath.Join(s.dir, "results"))
	for _, e := range entries {
		if !e.IsDir() || !validID(e.Name()) {
			continue
		}
		m, err := s.LoadMetaIgnoringExpiry(e.Name())
		if err != nil {
			// Corrupt or unreadable metadata: remove the orphan.
			_ = os.RemoveAll(s.resultDir(e.Name()))
			removed++
			continue
		}
		if now.After(m.ExpiresAt) {
			_ = os.RemoveAll(s.resultDir(e.Name()))
			removed++
			continue
		}
		var sz int64
		_ = filepath.Walk(s.resultDir(e.Name()), func(_ string, info os.FileInfo, err error) error {
			if err == nil {
				sz += info.Size()
			}
			return nil
		})
		results = append(results, aged{id: e.Name(), created: m.CreatedAt, size: sz, done: m.Complete})
		total += sz
	}
	scans, _ := os.ReadDir(filepath.Join(s.dir, "scans"))
	for _, e := range scans {
		if !e.IsDir() || !validID(e.Name()) {
			continue
		}
		job, err := s.loadScanIgnoringExpiry(e.Name())
		if err != nil || now.After(job.ExpiresAt) {
			_ = os.RemoveAll(filepath.Join(s.dir, "scans", e.Name()))
			removed++
		}
	}
	if s.quota > 0 && total > s.quota {
		sort.Slice(results, func(i, j int) bool {
			if results[i].done != results[j].done {
				return results[i].done // completed first
			}
			return results[i].created.Before(results[j].created)
		})
		for _, r := range results {
			if total <= s.quota {
				break
			}
			_ = os.RemoveAll(s.resultDir(r.id))
			total -= r.size
			removed++
		}
	}
	return removed, nil
}

// TotalBytes reports the current disk usage of stored results.
func (s *Store) TotalBytes() int64 {
	var total int64
	_ = filepath.Walk(filepath.Join(s.dir, "results"), func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// LoadMetaIgnoringExpiry reads metadata without the TTL check (cleanup).
func (s *Store) LoadMetaIgnoringExpiry(id string) (*ResultMeta, error) {
	data, err := os.ReadFile(filepath.Join(s.resultDir(id), "meta.json"))
	if err != nil {
		return nil, err
	}
	var m ResultMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *Store) loadScanIgnoringExpiry(id string) (*ScanJob, error) {
	data, err := os.ReadFile(s.scanPath(id))
	if err != nil {
		return nil, err
	}
	var job ScanJob
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, err
	}
	return &job, nil
}
