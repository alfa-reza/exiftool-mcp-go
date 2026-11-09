// Package server builds the MCP server and registers all tools.
package server

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alfa-reza/exiftool-mcp-go/internal/tools"
)

// New creates an MCP server with all seven tools registered.
func New(app *tools.App, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "exiftool-mcp-go",
		Version: version,
	}, nil)
	app.Register(s)
	return s
}
