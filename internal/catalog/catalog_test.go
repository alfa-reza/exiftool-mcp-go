package catalog

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alfa-reza/exiftool-mcp-go/internal/runner"
)

const listxDoc = `<?xml version='1.0' encoding='UTF-8'?>
<taginfo>
<table name='EXIF::Main' g0='EXIF' g1='IFD0' g2='Image'>
 <tag id='1' name='Make' type='string' writable='true'>
  <desc lang='en'>Make</desc>
 </tag>
</table>
<table name='XMP::dc' g0='XMP' g1='XMP-dc' g2='Image'>
 <tag id='2' name='Title' type='string' writable='true'>
  <desc lang='en'>Title</desc>
 </tag>
</table>
</taginfo>
`

func TestParseListXComplete(t *testing.T) {
	defs, err := ParseListX([]byte(listxDoc))
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 2 || defs[0].Name != "Make" || defs[0].Group != "EXIF" || defs[1].Group != "XMP" {
		t.Fatalf("unexpected defs: %+v", defs)
	}
}

func TestParseListXTruncated(t *testing.T) {
	// Cut after the first complete table: the partial catalog must
	// NOT be accepted as if it were the whole one.
	cut := strings.Index(listxDoc, "</table>") + len("</table>")
	if _, err := ParseListX([]byte(listxDoc[:cut])); err == nil {
		t.Fatal("truncated -listx output (missing </taginfo>) was accepted")
	}
	// Cut inside the second table.
	if _, err := ParseListX([]byte(listxDoc[:len(listxDoc)-30])); err == nil {
		t.Fatal("truncated -listx output (inside a table) was accepted")
	}
	// Malformed table content must error, not be skipped.
	bad := strings.Replace(listxDoc, "<tag id='2'", "<tag id='2' name=", 1)
	if _, err := ParseListX([]byte(bad)); err == nil {
		t.Fatal("malformed table was silently skipped")
	}
}

type listRunner struct{ failArg string }

func (r listRunner) Path() string       { return "fake" }
func (r listRunner) ConfigMode() string { return "standard" }
func (r listRunner) Run(_ context.Context, args ...string) (runner.Result, error) {
	arg := args[len(args)-1]
	if arg == r.failArg {
		return runner.Result{}, errors.New("boom")
	}
	var out string
	switch arg {
	case "-listx":
		out = listxDoc
	case "-listg":
		out = "Groups in family 0:\nEXIF XMP\n"
	case "-listr", "-listf":
		out = "JPG PNG\n"
	}
	return runner.Result{Stdout: []byte(out)}, nil
}

func TestLoadPropagatesListingFailures(t *testing.T) {
	ctx := context.Background()
	c, err := Load(ctx, listRunner{})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Tags) != 2 || len(c.Groups) != 2 || len(c.Readable) != 2 {
		t.Fatalf("incomplete catalog: %+v", c)
	}
	for _, arg := range []string{"-listg", "-listr", "-listf"} {
		if _, err := Load(ctx, listRunner{failArg: arg}); err == nil {
			t.Fatalf("Load ignored a failed %s", arg)
		}
	}
}
