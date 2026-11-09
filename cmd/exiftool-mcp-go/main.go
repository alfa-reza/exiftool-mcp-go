// exiftool-mcp-go is a read-only MCP server that exposes the installed
// ExifTool CLI to AI agents over stdio.
package main

import (
	"context"
	"log"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alfa-reza/exiftool-mcp-go/internal/config"
	"github.com/alfa-reza/exiftool-mcp-go/internal/meta"
	"github.com/alfa-reza/exiftool-mcp-go/internal/runner"
	"github.com/alfa-reza/exiftool-mcp-go/internal/server"
	"github.com/alfa-reza/exiftool-mcp-go/internal/store"
	"github.com/alfa-reza/exiftool-mcp-go/internal/tools"
)

// version is the server version reported to MCP clients.
var version = "1.0.0"

func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(0)
	cfg := config.Load()

	r := runner.New(cfg)
	st, err := store.New(cfg.StoreDir, cfg.ResultTTL, cfg.StoreQuotaBytes)
	if err != nil {
		log.Fatalf("exiftool-mcp-go: cannot open result store: %v", err)
	}
	if removed, err := st.Cleanup(); err == nil && removed > 0 {
		log.Printf("exiftool-mcp-go: result store cleanup removed %d expired/overflow entries", removed)
	}

	app := &tools.App{
		Cfg:    cfg,
		Runner: r,
		Reader: &meta.Reader{Runner: r},
		Store:  st,
	}
	srv := server.New(app, version)
	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("exiftool-mcp-go: server exited: %v", err)
	}
}
