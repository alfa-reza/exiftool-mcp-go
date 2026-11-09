package meta

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Candidate is one file in a frozen enumeration snapshot.
type Candidate struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// EnumError describes one input path that produced no candidates.
type EnumError struct {
	Path    string `json:"path"`
	Code    string `json:"code"` // PATH_NOT_FOUND | PERMISSION_DENIED | UNSUPPORTED_PATH_TYPE
	Message string `json:"message"`
}

// Enumerate expands input paths (files and directories) into a sorted,
// de-duplicated candidate list. Policy: symlinked directories are never
// descended into (no loops), symlinked regular files are followed,
// non-regular files (devices, FIFOs, sockets) are skipped, hidden files
// are included, and the extension filter is case-insensitive.
func Enumerate(paths []string, recursive bool, extensions []string) ([]Candidate, []EnumError) {
	extSet := make(map[string]bool, len(extensions))
	for _, e := range extensions {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		extSet[e] = true
	}
	matchExt := func(p string) bool {
		if len(extSet) == 0 {
			return true
		}
		return extSet[strings.ToLower(filepath.Ext(p))]
	}
	seen := make(map[string]bool)
	var out []Candidate
	var errs []EnumError
	add := func(abs string, info os.FileInfo) {
		if seen[abs] {
			return
		}
		if !matchExt(abs) {
			return
		}
		seen[abs] = true
		out = append(out, Candidate{Path: abs, Size: info.Size(), ModTime: info.ModTime()})
	}
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			errs = append(errs, EnumError{Path: p, Code: "PATH_NOT_FOUND", Message: err.Error()})
			continue
		}
		fi, err := os.Stat(abs) // follows symlinks for the top-level input
		if err != nil {
			code := "PATH_NOT_FOUND"
			if os.IsPermission(err) {
				code = "PERMISSION_DENIED"
			}
			errs = append(errs, EnumError{Path: abs, Code: code, Message: err.Error()})
			continue
		}
		if fi.Mode().IsRegular() {
			add(abs, fi)
			continue
		}
		if !fi.IsDir() {
			errs = append(errs, EnumError{Path: abs, Code: "UNSUPPORTED_PATH_TYPE",
				Message: fmt.Sprintf("not a regular file or directory: %s", fi.Mode())})
			continue
		}
		if !recursive {
			entries, err := os.ReadDir(abs)
			if err != nil {
				errs = append(errs, EnumError{Path: abs, Code: "PERMISSION_DENIED", Message: err.Error()})
				continue
			}
			for _, e := range entries {
				child := filepath.Join(abs, e.Name())
				cfi, err := os.Stat(child)
				if err != nil || !cfi.Mode().IsRegular() {
					continue
				}
				add(child, cfi)
			}
			continue
		}
		_ = filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // skip unreadable entries; read errors surface later per file
			}
			if d.IsDir() {
				if path != abs && d.Type()&fs.ModeSymlink != 0 {
					return filepath.SkipDir
				}
				return nil
			}
			info, err := d.Info()
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			add(path, info)
			return nil
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, errs
}
