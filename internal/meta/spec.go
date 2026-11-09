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
	EmbeddedLevel      int      // 0=off, 1=-ee, 2=-eee, 3=-eeee
	NumericValues      bool     // -n
	PreserveStructures bool     // -struct
	FastScanLevel      int      // 0=off, 1=-fast, 2..5=-fast2..-fast5
}

// selector grammar: Name | Group:Name | Group:All. Names start with a
// letter and contain letters, digits and underscores. Anything that
// could be confused with a CLI option or an expression is rejected.
var selectorRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(:([A-Za-z][A-Za-z0-9_]*|All))?$`)

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
		args = append(args, "-"+strings.Repeat("e", s.EmbeddedLevel+1))
	}
	if s.IncludeDuplicates {
		args = append(args, "-a")
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
