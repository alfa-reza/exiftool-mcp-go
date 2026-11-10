// Package catalog parses ExifTool's own listings (-listf, -listr,
// -listg, -listx) into searchable structures. Nothing is loaded until
// first use, and results are cached per ExifTool version + config.
package catalog

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/alfa-reza/exiftool-mcp-go/internal/runner"
)

// tableName returns the name attribute of a table start element
// for error messages.
func tableName(start xml.StartElement) string {
	for _, a := range start.Attr {
		if a.Name.Local == "name" {
			return a.Value
		}
	}
	return ""
}

// TagDef is one catalog tag definition from -listx.
type TagDef struct {
	Name        string `json:"name"`
	Group       string `json:"group"` // family-0 group, e.g. "EXIF"
	Table       string `json:"table,omitempty"`
	Type        string `json:"type,omitempty"`
	Writable    bool   `json:"writable,omitempty"`
	Description string `json:"description,omitempty"`
}

// Catalog holds parsed listings from one ExifTool installation.
type Catalog struct {
	Tags      []TagDef
	Groups    []string // family-0 group names from -listg
	Readable  []string // extensions from -listr
	Supported []string // extensions from -listf
}

// Load fetches and parses all listings through the runner. Every
// listing is required: a failed -listg/-listr/-listf used to be
// silently ignored, leaving the catalog quietly incomplete.
func Load(ctx context.Context, r runner.Runner) (*Catalog, error) {
	c := &Catalog{}
	out, err := runText(ctx, r, "-listx")
	if err != nil {
		return nil, err
	}
	tags, err := ParseListX(out)
	if err != nil {
		return nil, err
	}
	c.Tags = tags
	if out, err := runText(ctx, r, "-listg"); err != nil {
		return nil, err
	} else {
		c.Groups = ParseGroups(out)
	}
	if out, err := runText(ctx, r, "-listr"); err != nil {
		return nil, err
	} else {
		c.Readable = ParseExtensions(out)
	}
	if out, err := runText(ctx, r, "-listf"); err != nil {
		return nil, err
	} else {
		c.Supported = ParseExtensions(out)
	}
	return c, nil
}

func runText(ctx context.Context, r runner.Runner, arg string) ([]byte, error) {
	res, err := r.Run(ctx, arg)
	if err != nil {
		return nil, err
	}
	// The runner reports a nonzero ExifTool exit as data, not as a
	// Go error (batch reads rely on that); for catalog listings a
	// failed command must not masquerade as an empty listing.
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("exiftool %s failed with exit code %d: %s", arg, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

// ParseExtensions extracts extension tokens from -listf/-listr output.
func ParseExtensions(data []byte) []string {
	seen := map[string]bool{}
	var out []string
	for _, tok := range strings.Fields(string(data)) {
		if isExtensionToken(tok) && !seen[tok] {
			seen[tok] = true
			out = append(out, tok)
		}
	}
	sort.Strings(out)
	return out
}

func isExtensionToken(tok string) bool {
	if len(tok) < 2 || len(tok) > 8 {
		return false
	}
	for _, r := range tok {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// ParseGroups extracts family-0 group names from -listg output.
func ParseGroups(data []byte) []string {
	var groups []string
	family0 := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "Groups in family ") {
			family0 = strings.HasPrefix(line, "Groups in family 0:")
			continue
		}
		if !family0 {
			continue
		}
		for _, tok := range strings.Fields(line) {
			if len(tok) >= 2 {
				groups = append(groups, tok)
			}
		}
	}
	sort.Strings(groups)
	return groups
}

var errParse = &parseError{"no tag definitions could be parsed"}

type parseError struct{ s string }

func (e *parseError) Error() string { return e.s }

// --- -listx XML parsing ---

type xmlTagTable struct {
	XMLName xml.Name `xml:"table"`
	Name    string   `xml:"name,attr"`
	G0      string   `xml:"g0,attr"`
	G1      string   `xml:"g1,attr"`
	Tags    []xmlTag `xml:"tag"`
}

type xmlTag struct {
	Name     string `xml:"name,attr"`
	Type     string `xml:"type,attr"`
	Writable string `xml:"writable,attr"`
	Desc     []struct {
		Lang string `xml:"lang,attr"`
		Text string `xml:",chardata"`
	} `xml:"desc"`
}

// ParseListX parses the -listx XML stream into tag definitions.
// The stream must be COMPLETE: a truncated document (the decoder
// hits EOF before the closing </taginfo>, or a syntax error) and a
// malformed table are errors, not a silently partial catalog.
func ParseListX(data []byte) ([]TagDef, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var out []TagDef
	sawRootEnd := false
	for {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF && sawRootEnd {
				break
			}
			return nil, fmt.Errorf("parsing -listx output: %w", err)
		}
		if end, ok := tok.(xml.EndElement); ok && end.Name.Local == "taginfo" {
			sawRootEnd = true
			continue
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "table" {
			continue
		}
		var table xmlTagTable
		if err := dec.DecodeElement(&table, &start); err != nil {
			return nil, fmt.Errorf("parsing -listx table %q: %w", tableName(start), err)
		}
		group := table.G0
		for _, t := range table.Tags {
			def := TagDef{
				Name:     t.Name,
				Group:    group,
				Table:    table.Name,
				Type:     t.Type,
				Writable: t.Writable == "true" || t.Writable == "1",
			}
			for _, d := range t.Desc {
				if d.Lang == "" || d.Lang == "en" {
					def.Description = strings.TrimSpace(d.Text)
					break
				}
			}
			out = append(out, def)
		}
	}
	if len(out) == 0 {
		return nil, errParse
	}
	return out, nil
}

// Search returns tag definitions whose name or description contains the
// query (case-insensitive), optionally restricted to one family-0 group.
func (c *Catalog) Search(query, group string) []TagDef {
	q := strings.ToLower(query)
	var out []TagDef
	for _, t := range c.Tags {
		if group != "" && !strings.EqualFold(t.Group, group) {
			continue
		}
		if q == "" || strings.Contains(strings.ToLower(t.Name), q) ||
			strings.Contains(strings.ToLower(t.Description), q) {
			out = append(out, t)
		}
	}
	return out
}
