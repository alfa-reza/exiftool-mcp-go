// Package meta is the metadata read core: candidate enumeration, typed
// read specifications mapped to safe ExifTool argv, and parsing of
// ExifTool JSON output with full tag identity preservation.
package meta

import (
	"encoding/json"
	"strings"
)

// Entry is a single metadata tag occurrence. Key is the raw key from
// ExifTool's JSON output; it is never rewritten, so duplicate tags from
// different groups, documents or instances stay distinct entries.
type Entry struct {
	Key      string `json:"key"`                // raw JSON key, e.g. "EXIF:IFD0:Make"
	Group    string `json:"group"`              // group portion, e.g. "EXIF:IFD0"
	Name     string `json:"name"`               // tag name, e.g. "Make"
	Value    any    `json:"value"`              // scalar, array or struct from ExifTool
	Document string `json:"document,omitempty"` // document id if ExifTool provided one (e.g. "Doc1")
	Instance string `json:"instance,omitempty"` // instance id if provided
	Copy     string `json:"copy,omitempty"`     // duplicate copy id if provided (e.g. "Copy1", from -G4)
	// ValueRef is set when the value was too large for the response and
	// was moved to the result store as a text fragment.
	ValueRef     *ValueRef `json:"value_ref,omitempty"`
	ValueOmitted bool      `json:"value_omitted,omitempty"`
}

// ValueRef points at a stored fragment holding an oversized value.
type ValueRef struct {
	ResultID string `json:"result_id"`
	Kind     string `json:"kind"` // "text_fragment"
	Size     int64  `json:"size_bytes"`
}

// FileResult is the read outcome for one file.
type FileResult struct {
	Path     string   `json:"path"`
	Status   string   `json:"status"` // "ok" | "error"
	Entries  []Entry  `json:"entries,omitempty"`
	Error    string   `json:"error,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	// ChangedSinceDiscovery is set by the search layer when the file's
	// size/mtime differ from the frozen candidate snapshot.
	ChangedSinceDiscovery bool `json:"changed_since_discovery,omitempty"`
}

// ParseKey splits a raw ExifTool JSON key into group, tag name, and
// document/instance/copy provenance when present.
func ParseKey(key string) (group, name, document, instance, copyID string) {
	parts := strings.Split(key, ":")
	name = parts[len(parts)-1]
	// Strip a trailing copy suffix like " (1)" from the display name but
	// keep it in Key (identity).
	if i := strings.Index(name, " ("); i > 0 && strings.HasSuffix(name, ")") {
		name = name[:i]
	}
	var groupParts []string
	for _, p := range parts[:len(parts)-1] {
		switch {
		case isDocPart(p) || p == "Main":
			// Family-3 document segment: "Main" for the main document,
			// "Doc1", "Doc2", ... for embedded documents (-ee reads).
			// Sub-documents are numbered like "Doc2-3".
			document = p
		case strings.HasPrefix(p, "Instance"):
			instance = p
		case isCopyPart(p):
			// Family-4 copy segment from -G4: "Copy1", "Copy2", ...
			// distinguishes duplicate tags within one group.
			copyID = p
		default:
			groupParts = append(groupParts, p)
		}
	}
	group = strings.Join(groupParts, ":")
	return group, name, document, instance, copyID
}

// isDocPart reports whether p is a family-3 document segment: "Doc"
// followed by a document number, optionally with dash-separated
// sub-document numbers ("Doc1", "Doc2-3", "Doc1-2-1").
func isDocPart(p string) bool {
	if !strings.HasPrefix(p, "Doc") {
		return false
	}
	rest := p[3:]
	if rest == "" {
		return false
	}
	for _, seg := range strings.Split(rest, "-") {
		if seg == "" {
			return false
		}
		for _, r := range seg {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// isCopyPart reports whether p is a family-4 copy segment ("Copy1", ...).
func isCopyPart(p string) bool {
	if !strings.HasPrefix(p, "Copy") || len(p) <= 4 {
		return false
	}
	for _, r := range p[4:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// EntryFromPair builds an Entry from a raw JSON key/value pair.
func EntryFromPair(key string, value any) Entry {
	g, n, d, inst, cp := ParseKey(key)
	return Entry{Key: key, Group: g, Name: n, Value: value, Document: d, Instance: inst, Copy: cp}
}

// MarshalValue normalizes a value for size checks.
func MarshalValue(v any) ([]byte, error) {
	return json.Marshal(v)
}
