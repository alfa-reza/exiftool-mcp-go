package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/alfa-reza/exiftool-mcp-go/internal/runner"
)

// Reader performs batched metadata reads through the runner.
type Reader struct {
	Runner    runner.Runner
	ChunkSize int // files per exiftool invocation (default 100)
}

func (r *Reader) chunkSize() int {
	if r.ChunkSize > 0 {
		return r.ChunkSize
	}
	return 100
}

// Read reads metadata for the given files with the given spec. A hard
// error is returned only when exiftool itself cannot run; per-file
// problems are reported inside the corresponding FileResult.
func (r *Reader) Read(ctx context.Context, files []string, spec ReadSpec) ([]FileResult, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	results := make([]FileResult, 0, len(files))
	for start := 0; start < len(files); start += r.chunkSize() {
		end := min(start+r.chunkSize(), len(files))
		chunk := files[start:end]
		part, err := r.readChunk(ctx, chunk, spec)
		if err != nil {
			return results, err
		}
		results = append(results, part...)
	}
	return results, nil
}

func (r *Reader) readChunk(ctx context.Context, files []string, spec ReadSpec) ([]FileResult, error) {
	args := spec.Args()
	for _, f := range files {
		op, err := runner.Operand(f)
		if err != nil {
			return nil, err
		}
		args = append(args, op)
	}
	res, err := r.Runner.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(res.Stdout)) == 0 {
		return nil, fmt.Errorf("exiftool produced no output (exit %d): %s",
			res.ExitCode, truncate(strings.TrimSpace(res.Stderr), 500))
	}
	objects, err := ParseJSONArray(res.Stdout)
	if err != nil {
		return nil, fmt.Errorf("parsing exiftool JSON output: %w (stderr: %s)",
			err, truncate(strings.TrimSpace(res.Stderr), 300))
	}
	bySource := make(map[string][]Pair, len(objects))
	for _, obj := range objects {
		for _, p := range obj {
			if p.Key == "SourceFile" {
				if s, ok := p.Value.(string); ok {
					bySource[s] = obj
					bySource[pathKey(s)] = obj
				}
			}
		}
	}
	out := make([]FileResult, 0, len(files))
	for _, f := range files {
		obj := bySource[f]
		if obj == nil {
			// ExifTool reports the resolved SourceFile; try the operand form.
			if op, err := runner.Operand(f); err == nil {
				obj = bySource[op]
			}
		}
		if obj == nil {
			// Slash styles differ across platforms (Windows ExifTool
			// echoes forward slashes); correlate on the normalized form.
			obj = bySource[pathKey(f)]
		}
		if obj == nil {
			out = append(out, FileResult{Path: f, Status: "error",
				Error: "no output from exiftool for this file"})
			continue
		}
		out = append(out, FileResultFromPairs(f, obj))
	}
	applyStderrDiagnostics(out, files, res.Stderr)
	return out, nil
}

// pathKey normalizes a path for correlation between requested files
// and ExifTool's own path echo: on Windows ExifTool reports paths
// with forward slashes while callers pass backslashes, so correlation
// must not depend on the slash style. (filepath.ToSlash would be a
// no-op on Unix, so the replacement is unconditional; the exact path
// is always tried first, this is only an additional index key.)
func pathKey(p string) string {
	return strings.ReplaceAll(p, "\\", "/")
}

// applyStderrDiagnostics correlates stderr diagnostics with files.
//
// When tags are selected, ExifTool omits the Error field from the JSON
// object of a failed file and reports "Error: <msg> - <operand>" on
// stderr instead (verified against ExifTool 13.55). The operand is
// echoed after the last " - " separator; it is matched exactly or in
// normalized slash form (Windows echo style), never by guessing.
func applyStderrDiagnostics(out []FileResult, files []string, stderr string) {
	if stderr == "" {
		return
	}
	type diag struct{ errors, warnings []string }
	byOperand := make(map[string]*diag)
	for _, line := range strings.Split(stderr, "\n") {
		// Windows child output ends lines with CRLF; a trailing \r
		// would poison the operand comparison.
		line = strings.TrimSuffix(line, "\r")
		var kind, rest string
		switch {
		case strings.HasPrefix(line, "Error: "):
			kind, rest = "error", strings.TrimPrefix(line, "Error: ")
		case strings.HasPrefix(line, "Warning: "):
			kind, rest = "warning", strings.TrimPrefix(line, "Warning: ")
		default:
			continue
		}
		idx := strings.LastIndex(rest, " - ")
		if idx < 0 {
			continue
		}
		msg, tail := rest[:idx], rest[idx+3:]
		for _, f := range files {
			op, err := runner.Operand(f)
			if err != nil {
				continue
			}
			if tail != op && tail != f && pathKey(tail) != pathKey(op) && pathKey(tail) != pathKey(f) {
				continue
			}
			d := byOperand[f]
			if d == nil {
				d = &diag{}
				byOperand[f] = d
			}
			if kind == "error" {
				d.errors = append(d.errors, msg)
			} else {
				d.warnings = append(d.warnings, msg)
			}
			break
		}
	}
	for i := range out {
		d, ok := byOperand[out[i].Path]
		if !ok {
			continue
		}
		if len(d.errors) > 0 && out[i].Error == "" {
			out[i].Error = strings.Join(d.errors, "; ")
			out[i].Status = "error"
		}
		for _, w := range d.warnings {
			found := false
			for _, existing := range out[i].Warnings {
				if existing == w {
					found = true
					break
				}
			}
			if !found {
				out[i].Warnings = append(out[i].Warnings, w)
			}
		}
	}
}

// Pair is one raw key/value pair of an ExifTool JSON object, in order,
// with duplicate keys preserved.
type Pair struct {
	Key   string
	Value any
}

// ParseJSONArray parses exiftool -j output preserving duplicate keys and
// numeric literals (json.Number).
func ParseJSONArray(data []byte) ([][]Pair, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, fmt.Errorf("expected JSON array")
	}
	var objects [][]Pair
	for dec.More() {
		obj, err := parseObject(dec)
		if err != nil {
			return nil, err
		}
		objects = append(objects, obj)
	}
	// The array must be closed. Output truncated after a complete
	// object (a killed or output-capped ExifTool run) would otherwise
	// parse as a successful, complete read.
	tok, err = dec.Token()
	if err != nil {
		return nil, fmt.Errorf("truncated JSON array: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != ']' {
		return nil, fmt.Errorf("expected closing ']' in JSON array")
	}
	return objects, nil
}

func parseObject(dec *json.Decoder) ([]Pair, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("expected JSON object")
	}
	var pairs []Pair
	for dec.More() {
		ktok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := ktok.(string)
		if !ok {
			return nil, fmt.Errorf("expected object key")
		}
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		pairs = append(pairs, Pair{Key: key, Value: v})
	}
	_, err = dec.Token() // closing '}'
	return pairs, err
}

// FileResultFromPairs converts one ExifTool JSON object into a FileResult,
// separating ExifTool diagnostics (Error/Warning) from metadata entries.
func FileResultFromPairs(path string, pairs []Pair) FileResult {
	fr := FileResult{Path: path, Status: "ok"}
	for _, p := range pairs {
		if p.Key == "SourceFile" {
			continue
		}
		_, name, _, _, _ := ParseKey(p.Key)
		if isExifToolKey(p.Key, "Error") {
			if s, ok := p.Value.(string); ok && s != "" {
				fr.Error = s
				fr.Status = "error"
			}
			continue
		}
		if isExifToolKey(p.Key, "Warning") {
			if s, ok := p.Value.(string); ok && s != "" {
				fr.Warnings = append(fr.Warnings, s)
			}
			continue
		}
		_ = name
		fr.Entries = append(fr.Entries, EntryFromPair(p.Key, p.Value))
	}
	return fr
}

// isExifToolKey reports whether key is the ExifTool diagnostic key for
// the given leaf name ("Error" / "Warning"), with or without group prefix.
func isExifToolKey(key, leaf string) bool {
	parts := strings.Split(key, ":")
	if parts[len(parts)-1] != leaf {
		return false
	}
	if len(parts) == 1 {
		return true
	}
	for _, p := range parts[:len(parts)-1] {
		if p == "ExifTool" {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
