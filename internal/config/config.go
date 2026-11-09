// Package config loads server configuration from the environment.
//
// Configuration is only read at startup. MCP tool calls never accept
// executable paths or ExifTool config file paths from the agent.
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Config holds all runtime configuration for the server.
type Config struct {
	// ExifToolPath is the explicit exiftool executable path (EXIFTOOL_PATH).
	// Empty means: look up "exiftool" in PATH.
	ExifToolPath string
	// ExifToolConfig is the explicit trusted ExifTool config file
	// (EXIFTOOL_CONFIG). Empty means the standard behaviour: run with
	// `-config ""` so no user config file is loaded.
	ExifToolConfig string
	// StoreDir is the result-store directory. Empty means the default
	// per-user cache directory.
	StoreDir string

	// Limits (engineering defaults, see docs/configuration.md).
	CallTimeout      time.Duration // per exiftool invocation
	MaxConcurrency   int           // concurrent exiftool processes
	MaxResponseBytes int           // structured response budget per tool call
	DefaultPageSize  int           // records per first page
	MaxInlineBinary  int           // max bytes for inline binary delivery
	ResultTTL        time.Duration // lifetime of stored results/scans
	StoreQuotaBytes  int64         // disk quota for the result store
	ScanBatchFiles   int           // files evaluated per search invocation
	ScanBatchTime    time.Duration // time budget per search invocation
}

// Load reads configuration from environment variables, applying defaults.
func Load() Config {
	c := Config{
		ExifToolPath:     os.Getenv("EXIFTOOL_PATH"),
		ExifToolConfig:   os.Getenv("EXIFTOOL_CONFIG"),
		StoreDir:         os.Getenv("EXIFTOOL_MCP_STORE"),
		CallTimeout:      60 * time.Second,
		MaxConcurrency:   4,
		MaxResponseBytes: 1 << 20,
		DefaultPageSize:  100,
		MaxInlineBinary:  1 << 20,
		ResultTTL:        24 * time.Hour,
		StoreQuotaBytes:  1 << 30,
		ScanBatchFiles:   200,
		ScanBatchTime:    20 * time.Second,
	}
	if c.StoreDir == "" {
		if cache, err := os.UserCacheDir(); err == nil {
			c.StoreDir = filepath.Join(cache, "exiftool-mcp-go")
		} else {
			c.StoreDir = filepath.Join(os.TempDir(), "exiftool-mcp-go")
		}
	}
	if v := os.Getenv("EXIFTOOL_MCP_TTL_HOURS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.ResultTTL = time.Duration(n) * time.Hour
		}
	}
	return c
}

// ConfigMode reports "standard" or "custom" for diagnostics output.
func (c Config) ConfigMode() string {
	if c.ExifToolConfig != "" {
		return "custom"
	}
	return "standard"
}
