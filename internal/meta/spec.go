package meta

import (
	"fmt"
	"regexp"
	"strings"
)

// ReadSpec is the typed representation of ExifTool read options. It is
// the ONLY way handlers influence the read argv: selectors are validated
// against the ExifTool tag grammar and flags come from a fixed mapping.
type ReadSpec struct {
	IncludeTags        []string // selectors to read (empty = default read)
	ExcludeTags        []string // selectors to exclude
	IncludeDuplicates  bool     // -a
	UnknownTags        int      // 0=off, 1=-u, 2=-U
	EmbeddedLevel      int      // 0=off, 1=-ee, 2=-ee2, 3=-ee3
	NumericValues      bool     // -n
	PreserveStructures bool     // -struct
	FastScanLevel      int      // 0=off, 1=-fast, 2..5=-fast2..-fast5
}

// Selector grammar: Name | Group:Name | Group:All. Each segment starts
// with a letter and contains letters, digits and underscores; a dash
// may appear between alphanumerics so real group families such as
// "XMP-dc" work ("XMP-dc:Title"). Anything that could be confused with
// a CLI option or an expression is rejected.
var selectorRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(-[A-Za-z0-9_]+)*(:([A-Za-z][A-Za-z0-9_]*(-[A-Za-z0-9_]+)*|All))?$`)

// ValidateSelector checks one agent-supplied tag/group selector.
func ValidateSelector(sel string) error {
	if sel == "" {
		return fmt.Errorf("empty tag selector")
	}
	if strings.HasPrefix(sel, "-") || strings.ContainsAny(sel, "=<>+*/\\ \t\n\r\"'#@") {
		return fmt.Errorf("invalid tag selector %q", sel)
	}
	if !selectorRe.MatchString(sel) {
		return fmt.Errorf("invalid tag selector %q", sel)
	}
	// An unqualified selector becomes the argv element "-" + selector.
	// If the name is one ExifTool treats as a command-line option, the
	// process would execute the option instead of selecting the tag,
	// breaking the read-only boundary. Option families also take
	// numeric parameters (-fast2, -ee3, -g4), so a name that is an
	// option name plus trailing digits is an option too. The
	// qualified Group:Name form can never be an option, so it stays
	// available for such tags.
	if !strings.Contains(sel, ":") {
		lower := strings.ToLower(sel)
		if exiftoolOptionNames[lower] || exiftoolOptionNames[strings.TrimRight(lower, "0123456789")] {
			return fmt.Errorf("tag selector %q collides with an ExifTool option; use the qualified Group:Name form", sel)
		}
	}
	return nil
}

// Validate checks the whole spec.
func (s ReadSpec) Validate() error {
	for _, sel := range s.IncludeTags {
		if err := ValidateSelector(sel); err != nil {
			return err
		}
	}
	for _, sel := range s.ExcludeTags {
		if err := ValidateSelector(sel); err != nil {
			return err
		}
	}
	if s.UnknownTags < 0 || s.UnknownTags > 2 {
		return fmt.Errorf("unknown_tags must be 0, 1 or 2")
	}
	if s.EmbeddedLevel < 0 || s.EmbeddedLevel > 3 {
		return fmt.Errorf("embedded_level must be between 0 and 3")
	}
	if s.FastScanLevel < 0 || s.FastScanLevel > 5 {
		return fmt.Errorf("fast_scan_level must be between 0 and 5")
	}
	return nil
}

// Args returns the ExifTool read/output arguments for this spec
// (excluding -config and file operands).
func (s ReadSpec) Args() []string {
	args := []string{"-j", "-G0:1"}
	if s.EmbeddedLevel > 0 {
		// Document/instance families only appear for embedded content.
		args = append(args, "-G3")
		// The documented form is -ee[NUM]: plain -ee for level 1,
		// -ee2/-ee3 above that (not -eee/-eeee).
		if s.EmbeddedLevel == 1 {
			args = append(args, "-ee")
		} else {
			args = append(args, fmt.Sprintf("-ee%d", s.EmbeddedLevel))
		}
	}
	if s.IncludeDuplicates {
		// -G4 adds the copy number to the group so duplicate tags with
		// the same group and name stay distinct in JSON output; with
		// plain -a, ExifTool silently drops all but the first of them.
		args = append(args, "-a", "-G4")
	}
	switch s.UnknownTags {
	case 1:
		args = append(args, "-u")
	case 2:
		args = append(args, "-U")
	}
	if s.NumericValues {
		args = append(args, "-n")
	}
	if s.PreserveStructures {
		args = append(args, "-struct")
	}
	switch {
	case s.FastScanLevel == 1:
		args = append(args, "-fast")
	case s.FastScanLevel > 1:
		args = append(args, fmt.Sprintf("-fast%d", s.FastScanLevel))
	}
	for _, sel := range s.IncludeTags {
		args = append(args, "-"+sel)
	}
	for _, sel := range s.ExcludeTags {
		args = append(args, "--"+sel)
	}
	return args
}

// Describe returns the effective options in a JSON-friendly form for
// tool output (never raw argv with file paths).
func (s ReadSpec) Describe() map[string]any {
	return map[string]any{
		"include_tags":        s.IncludeTags,
		"exclude_tags":        s.ExcludeTags,
		"include_duplicates":  s.IncludeDuplicates,
		"unknown_tags":        s.UnknownTags,
		"embedded_level":      s.EmbeddedLevel,
		"numeric_values":      s.NumericValues,
		"preserve_structures": s.PreserveStructures,
		"fast_scan_level":     s.FastScanLevel,
	}
}
