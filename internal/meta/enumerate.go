package meta

import (
	"context"
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
//
// Enumeration honors ctx: walking a huge tree must not run past
// the caller's deadline or cancellation, so the walk checks the
// context as it goes and returns its error (with the partial
// results, which the caller discards) when it fires.
func Enumerate(ctx context.Context, paths []string, recursive bool, extensions []string) ([]Candidate, []EnumError, error) {
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
		if err := ctx.Err(); err != nil {
			return out, errs, err
		}
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
				if err := ctx.Err(); err != nil {
					return out, errs, err
				}
				child := filepath.Join(abs, e.Name())
				cfi, err := os.Stat(child)
				if err != nil {
					code := "PATH_NOT_FOUND"
					if os.IsPermission(err) {
						code = "PERMISSION_DENIED"
					}
					errs = append(errs, EnumError{Path: child, Code: code, Message: err.Error()})
					continue
				}
				if !cfi.Mode().IsRegular() {
					continue
				}
				add(child, cfi)
			}
			continue
		}
		// Resolve a symlinked root before walking: WalkDir examines
		// the root with Lstat, so a symlink-to-directory root would
		// otherwise be visited as one non-directory entry and
		// silently yield zero candidates. Symlinks BELOW the root
		// are still never descended into.
		walkRoot := abs
		if lst, err := os.Lstat(abs); err == nil && lst.Mode()&fs.ModeSymlink != 0 {
			if resolved, err := filepath.EvalSymlinks(abs); err == nil {
				walkRoot = resolved
			}
		}
		werr := filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, err error) error {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			if err != nil {
				// A subtree that cannot be traversed is reported,
				// not silently skipped: a scan that missed files
				// must not look complete.
				code := "PATH_NOT_FOUND"
				if os.IsPermission(err) {
					code = "PERMISSION_DENIED"
				}
				errs = append(errs, EnumError{Path: path, Code: code, Message: err.Error()})
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				if path != walkRoot && d.Type()&fs.ModeSymlink != 0 {
					return filepath.SkipDir
				}
				return nil
			}
			info, err := d.Info()
			if err != nil {
				errs = append(errs, EnumError{Path: path, Code: "PATH_NOT_FOUND", Message: err.Error()})
				return nil
			}
			if !info.Mode().IsRegular() {
				// A symlink to a regular file is a candidate,
				// exactly as in the non-recursive listing above
				// (which Stats its children). Symlinks to
				// directories are not followed, and other
				// non-regular files are skipped.
				if d.Type()&fs.ModeSymlink != 0 {
					if target, serr := os.Stat(path); serr == nil && target.Mode().IsRegular() {
						add(path, target)
					}
				}
				return nil
			}
			add(path, info)
			return nil
		})
		if werr != nil {
			return out, errs, werr
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, errs, nil
}
