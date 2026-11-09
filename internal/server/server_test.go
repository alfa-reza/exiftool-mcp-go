package server_test

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
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
