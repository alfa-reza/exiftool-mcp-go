package server_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const tinyJPEG = "/9j/4AAQSkZJRgABAQEASABIAAD/2wBDAP//////////////////////////////////////////////////////////////////////////////////////2wBDAf//////////////////////////////////////////////////////////////////////////////////////wAARCAABAAEDASIAAhEBAxEB/8QAFAABAAAAAAAAAAAAAAAAAAAACf/EABQQAQAAAAAAAAAAAAAAAAAAAAD/xAAUAQEAAAAAAAAAAAAAAAAAAAAA/8QAFBEBAAAAAAAAAAAAAAAAAAAAAP/aAAwDAQACEQMRAD8AvwH/2Q=="

func buildServer(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(wd, "..", "..")
	bin := filepath.Join(t.TempDir(), "exiftool-mcp-go")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/exiftool-mcp-go")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// TestStdioEndToEnd drives the real binary over stdio with the official
// SDK client: tools/list must show exactly the seven PRD tools, stdout
// must carry only protocol frames (any stray output breaks the client),
// and basic tool calls must succeed.
func TestStdioEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("exiftool"); err != nil {
		t.Skip("exiftool not installed")
	}
	bin := buildServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-test", Version: "0.0.1"}, nil)
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "EXIFTOOL_MCP_STORE="+t.TempDir())
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	want := map[string]bool{
		"read_metadata": false, "search_metadata": false, "compare_metadata": false,
		"discover_tags": false, "extract_binary": false, "get_exiftool_info": false,
		"get_result_page": false,
	}
	for _, tool := range tools.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %s missing read-only annotation", tool.Name)
		}
	}
	if len(tools.Tools) != 7 {
		t.Fatalf("expected exactly 7 tools, got %d", len(tools.Tools))
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("tool %s missing from tools/list", name)
		}
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "get_exiftool_info", Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("call get_exiftool_info: %v", err)
	}
	if res.IsError {
		t.Fatalf("get_exiftool_info returned error: %+v", res.Content)
	}
	sc, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("no structured content: %T", res.StructuredContent)
	}
	verOut, _ := exec.Command("exiftool", "-ver").Output()
	if sc["version"] != strings.TrimSpace(string(verOut)) {
		t.Fatalf("version mismatch: %v vs %s", sc["version"], verOut)
	}

	// read_metadata over stdio on a bare fixture.
	jpeg, err := base64.StdEncoding.DecodeString(tinyJPEG)
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(t.TempDir(), "bare.jpg")
	if err := os.WriteFile(fixture, jpeg, 0o644); err != nil {
		t.Fatal(err)
	}
	res2, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "read_metadata",
		Arguments: map[string]any{"paths": []string{fixture}},
	})
	if err != nil {
		t.Fatalf("call read_metadata: %v", err)
	}
	if res2.IsError {
		t.Fatalf("read_metadata returned error: %+v", res2.Content)
	}
	sc2 := res2.StructuredContent.(map[string]any)
	items, _ := sc2["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %v", sc2["items"])
	}
	item := items[0].(map[string]any)
	if item["status"] != "ok" {
		t.Fatalf("fixture read not ok: %v", item["status"])
	}
	found := false
	for _, e := range item["entries"].([]any) {
		em := e.(map[string]any)
		if em["key"] == "File:FileType" && em["value"] == "JPEG" {
			found = true
		}
	}
	if !found {
		t.Fatal("File:FileType=JPEG entry not found in stdio read")
	}
}

// TestStdioBigNumberPrecision proves the number-precision fix
// end to end over the wire: the query literal 9007199254740993
// must reach the evaluator intact (the SDK decodes tool
// arguments with encoding/json, which honors the query node's
// custom decoder), so it matches a file whose Comment is that
// exact integer as text. With a plain float64 decode the value
// degrades to 9007199254740992 and the match is lost.
func TestStdioBigNumberPrecision(t *testing.T) {
	if _, err := exec.LookPath("exiftool"); err != nil {
		t.Skip("exiftool not installed")
	}
	bin := buildServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	jpeg, err := base64.StdEncoding.DecodeString(tinyJPEG)
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(t.TempDir(), "big.jpg")
	if err := os.WriteFile(fixture, jpeg, 0o644); err != nil {
		t.Fatal(err)
	}
	set := exec.Command("exiftool", "-overwrite_original", "-Comment=9007199254740993", fixture)
	if out, err := set.CombinedOutput(); err != nil {
		t.Fatalf("tag fixture: %v\n%s", err, out)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-test", Version: "0.0.1"}, nil)
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "EXIFTOOL_MCP_STORE="+t.TempDir())
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "search_metadata",
		Arguments: map[string]any{
			"paths": []string{fixture},
			"query": map[string]any{
				"type": "predicate", "tag": "Comment", "operator": "eq",
				"value": json.Number("9007199254740993"),
			},
		},
	})
	if err != nil {
		t.Fatalf("call search_metadata: %v", err)
	}
	if res.IsError {
		t.Fatalf("search_metadata returned error: %+v", res.Content)
	}
	sc, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("no structured content: %T", res.StructuredContent)
	}
	if sc["matched_count"] != float64(1) {
		t.Fatalf("matched_count = %v, want 1 (the big literal lost precision in transit)", sc["matched_count"])
	}
}

func sha256sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

// TestStdioSourceIntegrity is the read-only freeze gate: it
// hashes the source files, exercises all seven tools over stdio
// (including an argument-error call, a not-found extraction, a
// missing-path read, and a cancelled search), and requires the
// hashes to be byte-identical afterwards. A read-only server
// must never modify the files it inspects, on any path.
func TestStdioSourceIntegrity(t *testing.T) {
	if _, err := exec.LookPath("exiftool"); err != nil {
		t.Skip("exiftool not installed")
	}
	bin := buildServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	dir := t.TempDir()
	jpeg, err := base64.StdEncoding.DecodeString(tinyJPEG)
	if err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(dir, "a.jpg")
	b := filepath.Join(dir, "b.jpg")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, jpeg, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"-overwrite_original", "-Comment=integrity-a", a},
		{"-overwrite_original", "-Comment=integrity-b", "-EXIF:Make=Canon", b},
	} {
		if out, err := exec.Command("exiftool", args...).CombinedOutput(); err != nil {
			t.Fatalf("tag fixture: %v\n%s", err, out)
		}
	}
	hashAll := func() map[string]string {
		sums := map[string]string{}
		for _, p := range []string{a, b} {
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			sums[p] = hex.EncodeToString(sha256sum(data))
		}
		return sums
	}
	before := hashAll()

	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-test", Version: "0.0.1"}, nil)
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "EXIFTOOL_MCP_STORE="+t.TempDir())
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()
	call := func(name string, args map[string]any) *mcp.CallToolResult {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("call %s: %v", name, err)
		}
		return res
	}
	mustOK := func(name string, args map[string]any) *mcp.CallToolResult {
		res := call(name, args)
		if res.IsError {
			t.Fatalf("%s returned a tool error: %+v", name, res.Content)
		}
		return res
	}

	mustOK("get_exiftool_info", map[string]any{})
	mustOK("discover_tags", map[string]any{"scope": "present_in_files", "paths": []string{dir}})
	read := mustOK("read_metadata", map[string]any{
		"paths":            []string{dir},
		"response_options": map[string]any{"page_size": 1},
	})
	sc, _ := read.StructuredContent.(map[string]any)
	resultID, _ := sc["result_id"].(string)
	if resultID == "" {
		t.Fatalf("read_metadata over two files with page_size 1 returned no result_id: %v", sc)
	}
	mustOK("get_result_page", map[string]any{"result_id": resultID})
	mustOK("search_metadata", map[string]any{
		"paths": []string{dir},
		"query": map[string]any{"type": "predicate", "tag": "Comment", "operator": "eq", "value": "integrity-a"},
	})
	mustOK("compare_metadata", map[string]any{"left_path": a, "right_path": b})
	// Error paths are part of the gate: a missing binary tag, a
	// rejected selector, and a nonexistent input path.
	call("extract_binary", map[string]any{"path": a, "tag": "PreviewImage"})
	call("extract_binary", map[string]any{"path": a, "tag": "EXIF:all"})
	call("read_metadata", map[string]any{"paths": []string{filepath.Join(dir, "nope.jpg")}})
	// A cancelled call must not modify sources either.
	cctx, ccancel := context.WithCancel(ctx)
	ccancel()
	_, _ = session.CallTool(cctx, &mcp.CallToolParams{
		Name: "search_metadata",
		Arguments: map[string]any{
			"paths": []string{dir},
			"query": map[string]any{"type": "predicate", "tag": "Make", "operator": "exists"},
		},
	})

	after := hashAll()
	for p, sum := range before {
		if after[p] != sum {
			t.Fatalf("source file modified by the read-only server: %s", p)
		}
	}
}
