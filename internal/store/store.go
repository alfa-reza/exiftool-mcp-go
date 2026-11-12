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
	"bytes"
	"context"
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
	"unicode/utf8"

	"github.com/alfa-reza/exiftool-mcp-go/internal/meta"
)

// Error codes surfaced to tool handlers.
var (
	ErrNotFound = errors.New("result not found")
	ErrExpired  = errors.New("result expired")
	ErrCorrupt  = errors.New("stored data corrupt")
	ErrFull     = errors.New("result store quota exceeded")
	ErrBusy     = errors.New("result store busy: write lock held by another process")
	// ErrRecordTooLarge reports a stored record that cannot fit
	// a page byte limit at all (RecordsPage).
	ErrRecordTooLarge = errors.New("stored record exceeds the page byte limit")
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
	ID         string          `json:"id"`
	ResultID   string          `json:"result_id"`
	QueryJSON  json.RawMessage `json:"query"`
	SpecJSON   json.RawMessage `json:"spec"`
	ReturnTags []string        `json:"return_tags,omitempty"`
	// TimeContext is the timezone assumption the scan was started
	// with. A resume must reuse it: accepting a different (or no)
	// assumption on resume would silently change verdicts mid-scan.
	TimeContext string `json:"time_context,omitempty"`
	// HasTimeContext records whether a time_context was supplied at
	// all when the scan started, so a resume cannot ADD one to a
	// scan that began without it. (Checkpoints written before this
	// field existed are recognized by TimeContext != "".)
	HasTimeContext bool             `json:"has_time_context,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
	ExpiresAt      time.Time        `json:"expires_at"`
	Candidates     []meta.Candidate `json:"candidates"`
	NextIndex      int              `json:"next_index"`
	// CommittedRecords is the number of result records written by
	// committed batches. On resume it is reconciled with the result
	// store. Records ahead of the checkpoint mean a crash between
	// appending a batch and saving the checkpoint; those records
	// are rolled back and the batch is reprocessed, because the
	// batch's other outcomes (no-matches, per-file errors) were
	// never persisted and adopting the records would corrupt the
	// counts. Fewer records than the checkpoint claims is genuine
	// corruption.
	CommittedRecords int `json:"committed_records"`
	Processed        int `json:"processed"`
	Matched          int `json:"matched"`
	NoMatch          int `json:"no_match"`
	Indet            int `json:"indeterminate"`
	Errors           int `json:"errors"`
	// EnumErrors counts input paths that produced no candidates at
	// enumeration time (details ride in ErrorDetails). A scan with
	// enumeration errors is never reported as fully complete: part
	// of the requested input was never examined.
	EnumErrors int  `json:"enum_errors,omitempty"`
	Done       bool `json:"done"`
	// ErrorDetails persists the per-file errors of all batches (the
	// exact total is Errors; details are capped by the tools layer),
	// so a resumed scan still reports why earlier files failed.
	ErrorDetails []ScanError `json:"error_details,omitempty"`
}

// ScanError is one persisted per-file scan error.
type ScanError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Path    string `json:"path,omitempty"`
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

// metaBytes returns the size of the meta.json a result occupies,
// so capacity checks count the metadata alongside the payload
// instead of discovering the overrun only afterwards, when
// TotalBytes walks the written files.
func metaBytes(m *ResultMeta) int64 {
	data, err := json.Marshal(m)
	if err != nil {
		return 0
	}
	return int64(len(data))
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

// ensureCapacity guarantees room for add more bytes of store data.
// The caller must hold s.mu: the capacity check and the write it
// guards have to be one atomic step, or concurrent writers can each
// pass the check and overshoot the quota together. When the
// projected usage exceeds the quota it runs a cleanup that evicts
// completed results until the pending write fits — eviction is
// driven by the size of the write that needs room, not only by
// usage already over the limit. If the data still does not fit,
// ErrFull is returned.
func (s *Store) ensureCapacity(add int64) error {
	if s.quota <= 0 || add <= 0 {
		return nil
	}
	if s.TotalBytes()+add <= s.quota {
		return nil
	}
	_, _ = s.cleanup(add)
	if s.TotalBytes()+add > s.quota {
		return fmt.Errorf("%w: %d bytes do not fit in the %d-byte quota", ErrFull, add, s.quota)
	}
	return nil
}

func recordsBytes(records []json.RawMessage) int64 {
	var n int64
	for _, r := range records {
		n += int64(len(r)) + 1
	}
	return n
}

// SaveRecords stores a complete list of JSON records.
func (s *Store) SaveRecords(records []json.RawMessage) (*ResultMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockWrite()
	if err != nil {
		return nil, err
	}
	defer unlock()
	m := s.newMeta(KindRecords)
	// appendSegment will reference exactly one segment when there
	// are records; measure the meta in the state saveMeta will
	// persist — including for an empty result, whose meta.json is
	// still store data the quota must cover.
	sim := *m
	if len(records) > 0 {
		sim.Segments = []Segment{{File: "seg-000001.jsonl", Records: len(records), Bytes: recordsBytes(records)}}
		sim.TotalRecords = len(records)
		sim.TotalBytes = recordsBytes(records)
	}
	add := recordsBytes(records) + metaBytes(&sim)
	if err := s.ensureCapacity(add); err != nil {
		return nil, err
	}
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
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockWrite()
	if err != nil {
		return nil, err
	}
	defer unlock()
	m := s.newMeta(kind)
	m.Complete = false
	// Even an empty result occupies its meta.json; the quota
	// applies to it like to any other store data.
	if err := s.ensureCapacity(metaBytes(m)); err != nil {
		return nil, err
	}
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
	unlock, err := s.lockWrite()
	if err != nil {
		return nil, err
	}
	defer unlock()
	m, err := s.LoadMeta(id)
	if err != nil {
		return nil, err
	}
	add := recordsBytes(records)
	if len(records) > 0 {
		// The meta grows by one segment entry; count the growth.
		sim := *m
		sim.Segments = append(append([]Segment{}, m.Segments...),
			Segment{File: fmt.Sprintf("seg-%06d.jsonl", len(m.Segments)+1), Records: len(records), Bytes: recordsBytes(records)})
		sim.TotalRecords = m.TotalRecords + len(records)
		sim.TotalBytes = m.TotalBytes + recordsBytes(records)
		add += metaBytes(&sim) - metaBytes(m)
	}
	if err := s.ensureCapacity(add); err != nil {
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

// TruncateRecords drops every record after the first keep ones. It
// is the rollback half of the scan commit protocol: records that a
// crashed batch appended but its checkpoint never counted are
// removed so the batch can be reprocessed cleanly, with its
// no-matches and per-file errors re-derived instead of guessed.
// The surviving records are rewritten into a single fresh segment
// and the meta is switched to it before the old segments are
// deleted, so a crash mid-truncate leaves orphan files, never a
// dangling reference.
func (s *Store) TruncateRecords(id string, keep int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockWrite()
	if err != nil {
		return err
	}
	defer unlock()
	m, err := s.LoadMeta(id)
	if err != nil {
		return err
	}
	if m.Kind != KindRecords {
		return fmt.Errorf("%w: result %s holds %s, not records", ErrCorrupt, id, m.Kind)
	}
	if keep < 0 || keep > m.TotalRecords {
		return fmt.Errorf("%w: cannot truncate %d records to %d", ErrCorrupt, m.TotalRecords, keep)
	}
	if keep == m.TotalRecords {
		return nil
	}
	var lines []string
	remaining := keep
	for _, seg := range m.Segments {
		if remaining == 0 {
			break
		}
		data, err := os.ReadFile(filepath.Join(s.resultDir(id), seg.File))
		if err != nil {
			return fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		segLines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		if len(segLines) != seg.Records {
			return fmt.Errorf("%w: segment %s record count mismatch", ErrCorrupt, seg.File)
		}
		take := min(remaining, len(segLines))
		lines = append(lines, segLines[:take]...)
		remaining -= take
	}
	// Remove orphans of earlier truncates that crashed before
	// their saveMeta: unreferenced seg-trunc-* segments.
	referenced := make(map[string]bool, len(m.Segments))
	for _, seg := range m.Segments {
		referenced[seg.File] = true
	}
	if olds, _ := filepath.Glob(filepath.Join(s.resultDir(id), "seg-trunc-*.jsonl")); olds != nil {
		for _, f := range olds {
			if !referenced[filepath.Base(f)] {
				_ = os.Remove(f)
			}
		}
	}
	old := m.Segments
	m.Segments = nil
	m.TotalBytes = 0
	if keep > 0 {
		var buf bytes.Buffer
		for _, line := range lines {
			buf.WriteString(line)
			buf.WriteByte('\n')
		}
		// A unique name per truncate: with a fixed name, a
		// crash between replacing the segment and saving the
		// meta on a SECOND truncate left the meta claiming
		// the old record count over the new, shorter content
		// — a corrupt result. With a unique name the meta
		// still points at the intact previous segment until
		// saveMeta commits the new one; the uncommitted file
		// is an orphan, swept by a later truncate (above) or
		// removed with the result itself.
		name := "seg-trunc-" + newID() + ".jsonl"
		if err := writeAtomic(filepath.Join(s.resultDir(id), name), buf.Bytes()); err != nil {
			return err
		}
		m.Segments = []Segment{{File: name, Records: keep, Bytes: int64(buf.Len())}}
		m.TotalBytes = int64(buf.Len())
	}
	m.TotalRecords = keep
	if err := s.saveMeta(m); err != nil {
		return err
	}
	for _, seg := range old {
		if len(m.Segments) == 0 || seg.File != m.Segments[0].File {
			_ = os.Remove(filepath.Join(s.resultDir(id), seg.File))
		}
	}
	return nil
}

// DiscardResult removes a result outright, ignoring absence.
// It exists for the pending-fragment lifecycle: a tool whose
// call fails after creating pending fragments discards them,
// because incomplete results are never evicted and would
// otherwise occupy quota until their TTL, degrading a retry
// of the same call to RESULT_STORE_FULL. Callers must only
// discard results no committed record references.
func (s *Store) DiscardResult(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockWrite()
	if err != nil {
		return err
	}
	defer unlock()
	if !validID(id) {
		return fmt.Errorf("%w: invalid result id", ErrNotFound)
	}
	return os.RemoveAll(s.resultDir(id))
}

// MarkComplete flags a result as fully written.
func (s *Store) MarkComplete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockWrite()
	if err != nil {
		return err
	}
	defer unlock()
	m, err := s.LoadMeta(id)
	if err != nil {
		return err
	}
	m.Complete = true
	return s.saveMeta(m)
}

// SaveBlob stores a binary payload.
func (s *Store) SaveBlob(data []byte, mime string) (*ResultMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockWrite()
	if err != nil {
		return nil, err
	}
	defer unlock()
	m := s.newMeta(KindBinary)
	sum := sha256.Sum256(data)
	m.SHA256 = hex.EncodeToString(sum[:])
	m.MIME = mime
	m.TotalBytes = int64(len(data))
	if err := s.ensureCapacity(int64(len(data)) + metaBytes(m)); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.resultDir(m.ID), 0o700); err != nil {
		return nil, err
	}
	if err := writeAtomic(filepath.Join(s.resultDir(m.ID), "blob.bin"), data); err != nil {
		return nil, err
	}
	if err := s.saveMeta(m); err != nil {
		return nil, err
	}
	return m, nil
}

// SaveText stores a text payload as a complete result.
func (s *Store) SaveText(text string) (*ResultMeta, error) {
	return s.saveText(text, true)
}

// SaveTextPending stores a text payload as an INCOMPLETE result.
// Cleanup never evicts incomplete results, so a fragment saved
// this way cannot be removed by any write — including the commit
// writes of the very call that created it — until its owner
// finalizes it with MarkComplete once the records referencing it
// are committed. Value fragments (value_ref targets) use this
// lifecycle: a fragment that could be evicted between creation
// and the commit of its referencing records can leave a
// successful response carrying a dead reference. A pending
// fragment abandoned by a failed call is still bounded by its
// TTL: expiry removal does not consult completeness.
func (s *Store) SaveTextPending(text string) (*ResultMeta, error) {
	return s.saveText(text, false)
}

func (s *Store) saveText(text string, complete bool) (*ResultMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockWrite()
	if err != nil {
		return nil, err
	}
	defer unlock()
	m := s.newMeta(KindText)
	m.Complete = complete
	sum := sha256.Sum256([]byte(text))
	m.SHA256 = hex.EncodeToString(sum[:])
	m.TotalBytes = int64(len(text))
	if err := s.ensureCapacity(int64(len(text)) + metaBytes(m)); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.resultDir(m.ID), 0o700); err != nil {
		return nil, err
	}
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
// bounded by maxBytes of payload. A record that cannot fit the
// byte limit at all fails with ErrRecordTooLarge: returning it
// whole would break the budget, an empty page would wedge the
// cursor on it forever, and truncating it would corrupt the JSON.
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
	// Records are consumed strictly in order. Reaching the page size
	// or the byte budget ends the whole page: continuing into the next
	// segment after a mid-segment stop would skip the unconsumed
	// records of the current segment while the cursor (derived from
	// the returned count) advances past them, losing them forever.
segments:
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
				break segments
			}
			if maxBytes > 0 && len(page.Records) == 0 && len(line) > maxBytes {
				return nil, nil, fmt.Errorf("%w: record of %d bytes exceeds the %d-byte page limit; a limit of at least %d is required",
					ErrRecordTooLarge, len(line), maxBytes, len(line))
			}
			if maxBytes > 0 && len(page.Records) > 0 && used+len(line) > maxBytes {
				break segments
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
	// The payload is opened BEFORE the end-of-payload shortcut:
	// a missing payload file is corruption the shortcut must not
	// paper over with a successful empty read.
	f, err := os.Open(filepath.Join(s.resultDir(id), file))
	if err != nil {
		return nil, nil, 0, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, nil, 0, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if fi.Size() != m.TotalBytes {
		// The payload on disk is not the payload the metadata
		// describes (truncated or extended): corruption at any
		// offset, including the end-of-payload shortcut below,
		// which must not bless it with an empty success.
		return nil, nil, 0, fmt.Errorf("%w: payload size %d does not match metadata total %d", ErrCorrupt, fi.Size(), m.TotalBytes)
	}
	if offset == m.TotalBytes {
		// A read at the exact end is a valid empty final fragment,
		// not an error: the payload is exhausted by definition, so
		// there is no next chunk.
		return m, []byte{}, offset, nil
	}
	if _, err := f.Seek(offset, 0); err != nil {
		return nil, nil, 0, err
	}
	if maxBytes <= 0 {
		return nil, nil, 0, fmt.Errorf("%w: fragment size must be positive", ErrCorrupt)
	}
	buf := make([]byte, maxBytes)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return nil, nil, 0, err
	}
	chunk := buf[:n]
	if m.Kind == KindText {
		chunk = trimToUTF8Boundary(chunk, offset+int64(n) < m.TotalBytes)
		if len(chunk) == 0 && n > 0 {
			// The byte budget is smaller than the next code point.
			// Return the complete code point anyway so the cursor
			// always advances and paging can never stall. ReadAt
			// reports io.EOF when the code point ends the payload;
			// the bytes it did read are the ones that matter, so
			// the error must not discard them.
			ext := make([]byte, 4)
			if rn, _ := f.ReadAt(ext, offset); rn > 0 {
				if _, size := utf8.DecodeRune(ext[:rn]); size > 0 && size <= rn {
					chunk = ext[:size]
				} else {
					// The payload itself ends mid-rune (corrupt);
					// return one byte so the cursor still advances.
					chunk = ext[:1]
				}
			}
		}
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

// SaveScan atomically persists a scan job checkpoint. Checkpoints
// count toward the store quota like any other payload; only the
// growth over the previous checkpoint is charged, so re-saving a
// same-size checkpoint never fails on its own bytes.
func (s *Store) SaveScan(job *ScanJob) error {
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// The growth check and the write must be atomic across
	// processes too: two servers sharing one store directory
	// could otherwise each pass the check and overshoot the
	// quota together (the result writers already hold this lock;
	// checkpoints were the gap).
	unlock, err := s.lockWrite()
	if err != nil {
		return err
	}
	defer unlock()
	var existing int64
	if fi, err := os.Stat(s.scanPath(job.ID)); err == nil {
		existing = fi.Size()
	}
	if growth := int64(len(data)) - existing; growth > 0 {
		if err := s.ensureCapacity(growth); err != nil {
			return err
		}
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

const (
	// lockHeartbeat refreshes a held scan lock's mtime; lockStaleAfter
	// is how long a heartbeat may be silent before the holder is
	// considered crashed and the lock is taken over.
	lockHeartbeat  = 10 * time.Second
	lockStaleAfter = 45 * time.Second
)

// readLockToken returns the ownership token stored in a scan lock
// file, or "" when it cannot be read.
func readLockToken(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// LockScan acquires the cross-process lock for one scan and returns
// its release function. The mutex in the tools layer serializes
// batches inside one server process; this lock extends the same
// guarantee to two server processes sharing one store directory —
// without it, both could run the same batch and duplicate records.
// The lock is a file created exclusively inside the scan's
// directory, containing a unique ownership token; a heartbeat keeps
// its mtime fresh while held, and a lock whose heartbeat has
// stopped is taken over. The heartbeat and the release verify the
// token before touching the file: after a takeover the file belongs
// to the successor, and the stale owner must neither refresh nor
// delete it. Waiting honors ctx cancellation.
// writeLockWait bounds how long a writer waits for a peer process
// to finish its write before reporting the store busy. Writes are
// short (a payload plus its meta), so this is generous.
const writeLockWait = 30 * time.Second

// lockWrite acquires the store-wide inter-process write lock. The
// in-process mutex makes a capacity check and the write it guards
// atomic within one server, but several MCP servers can share one
// store directory; this lock extends the same atomicity across
// processes, following the scan-lock discipline (exclusive
// create, ownership token, heartbeat, stale takeover). The caller
// must already hold s.mu and must call the returned release
// function when the write is done.
func (s *Store) lockWrite() (func(), error) {
	path := filepath.Join(s.dir, "write.lock")
	token := fmt.Sprintf("%d-%s", os.Getpid(), newID())
	deadline := time.Now().Add(writeLockWait)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.WriteString(token)
			_ = f.Close()
			stop := make(chan struct{})
			go func() {
				tick := time.NewTicker(lockHeartbeat)
				defer tick.Stop()
				for {
					select {
					case <-stop:
						return
					case <-tick.C:
						if readLockToken(path) != token {
							return // taken over; stop refreshing
						}
						now := time.Now()
						_ = os.Chtimes(path, now, now)
					}
				}
			}()
			var once sync.Once
			return func() {
				once.Do(func() {
					close(stop)
					if readLockToken(path) == token {
						_ = os.Remove(path)
					}
				})
			}, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		if fi, err := os.Stat(path); err == nil && time.Since(fi.ModTime()) > lockStaleAfter {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, ErrBusy
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (s *Store) LockScan(ctx context.Context, id string) (func(), error) {
	if !validID(id) {
		return nil, ErrNotFound
	}
	dir := filepath.Join(s.dir, "scans", id)
	if _, err := os.Stat(dir); err != nil {
		return nil, ErrNotFound
	}
	path := filepath.Join(dir, "job.lock")
	token := fmt.Sprintf("%d-%s", os.Getpid(), newID())
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.WriteString(token)
			_ = f.Close()
			stop := make(chan struct{})
			go func() {
				tick := time.NewTicker(lockHeartbeat)
				defer tick.Stop()
				for {
					select {
					case <-stop:
						return
					case <-tick.C:
						if readLockToken(path) != token {
							return // taken over; stop refreshing
						}
						now := s.now()
						_ = os.Chtimes(path, now, now)
					}
				}
			}()
			var once sync.Once
			return func() {
				once.Do(func() {
					close(stop)
					if readLockToken(path) == token {
						_ = os.Remove(path)
					}
				})
			}, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		if fi, err := os.Stat(path); err == nil && s.now().Sub(fi.ModTime()) > lockStaleAfter {
			_ = os.Remove(path)
			continue
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Cleanup removes expired results and scans, then enforces the disk
// quota by deleting the oldest completed results first. It returns
// the number of removed entries.
func (s *Store) Cleanup() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockWrite()
	if err != nil {
		return 0, err
	}
	defer unlock()
	return s.cleanup(0)
}

// cleanup is Cleanup with a reservation: eviction frees enough room
// for need additional bytes, not merely enough to get back under
// the quota — a write that arrives while usage sits just below the
// limit must still be able to displace old completed results. Only
// completed results are eviction candidates; incomplete ones belong
// to work in flight and leave through expiry. The caller must hold
// s.mu.
func (s *Store) cleanup(need int64) (int, error) {
	removed := 0
	now := s.now()

	var results []aged
	entries, _ := os.ReadDir(filepath.Join(s.dir, "results"))
	for _, e := range entries {
		if !e.IsDir() || !validID(e.Name()) {
			continue
		}
		m, err := s.LoadMetaIgnoringExpiry(e.Name())
		if err != nil {
			// Corrupt or unreadable metadata: remove the orphan —
			// unless the directory is brand new, in which case a
			// concurrent CreateResult may not have written its
			// meta.json yet (cleanup now also runs mid-flight, when
			// a write hits the quota).
			if info, ierr := e.Info(); ierr == nil && time.Since(info.ModTime()) < time.Minute {
				continue
			}
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
			if err == nil && !info.IsDir() {
				sz += info.Size()
			}
			return nil
		})
		if m.Complete {
			results = append(results, aged{id: e.Name(), created: m.CreatedAt, size: sz, kind: m.Kind})
		}
	}
	scans, _ := os.ReadDir(filepath.Join(s.dir, "scans"))
	for _, e := range scans {
		if !e.IsDir() || !validID(e.Name()) {
			continue
		}
		job, err := s.loadScanIgnoringExpiry(e.Name())
		if err != nil {
			// A brand-new directory may belong to a CreateScan
			// that has not written its job.json yet.
			if info, ierr := e.Info(); ierr == nil && time.Since(info.ModTime()) < time.Minute {
				continue
			}
			_ = os.RemoveAll(filepath.Join(s.dir, "scans", e.Name()))
			removed++
			continue
		}
		if now.After(job.ExpiresAt) {
			_ = os.RemoveAll(filepath.Join(s.dir, "scans", e.Name()))
			removed++
		}
	}
	if s.quota > 0 {
		total := s.TotalBytes()
		if total+need > s.quota {
			// Retention contract: a fragment referenced by a
			// surviving records result is pinned and skipped
			// here (see referencedFragments). If the remaining
			// candidates cannot make room, ensureCapacity
			// reports ErrFull to the caller.
			pinned := s.referencedFragments(results)
			sort.Slice(results, func(i, j int) bool {
				return results[i].created.Before(results[j].created)
			})
			for _, r := range results {
				if total+need <= s.quota {
					break
				}
				if pinned[r.id] {
					continue
				}
				_ = os.RemoveAll(s.resultDir(r.id))
				total -= r.size
				removed++
			}
		}
	}
	return removed, nil
}

// aged is one eviction candidate: a complete result cleanup
// may remove under quota pressure.
type aged struct {
	id      string
	created time.Time
	size    int64
	kind    Kind
}

// referencedFragments returns the subset of the given eviction
// candidates that must be retained: value fragments (text or
// binary results) whose ID appears in a surviving records
// result. The retention contract is that a value_ref stays
// valid exactly as long as the result that cites it: quota
// eviction may remove unreferenced results at any time, but
// never a fragment out from under a live records result. When
// the citing result is itself evicted or expires, the fragment
// becomes an ordinary candidate in a later pass.
//
// The scan is a substring search over the records' segment
// bytes: fragment IDs are random 128-bit values stored in the
// records' value_ref objects, so a plain byte match is exact
// for practical purposes. It runs only on the eviction path,
// under quota pressure.
func (s *Store) referencedFragments(candidates []aged) map[string]bool {
	pinned := map[string]bool{}
	var fragIDs []string
	for _, c := range candidates {
		if c.kind == KindText || c.kind == KindBinary {
			fragIDs = append(fragIDs, c.id)
		}
	}
	if len(fragIDs) == 0 {
		return pinned
	}
	entries, _ := os.ReadDir(filepath.Join(s.dir, "results"))
	for _, e := range entries {
		if !e.IsDir() || !validID(e.Name()) {
			continue
		}
		m, err := s.LoadMetaIgnoringExpiry(e.Name())
		if err != nil || m.Kind != KindRecords {
			continue
		}
		for _, seg := range m.Segments {
			data, err := os.ReadFile(filepath.Join(s.resultDir(e.Name()), seg.File))
			if err != nil {
				continue
			}
			for _, id := range fragIDs {
				if !pinned[id] && bytes.Contains(data, []byte(id)) {
					pinned[id] = true
				}
			}
		}
	}
	return pinned
}

// TotalBytes reports the current disk usage of the whole store:
// results AND scan checkpoints. Checkpoints live outside results/;
// leaving them out let scan-heavy workloads fill the disk entirely
// off the quota's books.
func (s *Store) TotalBytes() int64 {
	var total int64
	_ = filepath.Walk(s.dir, func(_ string, info os.FileInfo, err error) error {
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
