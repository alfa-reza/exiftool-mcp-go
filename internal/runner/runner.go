// Package runner executes the ExifTool CLI safely.
//
// The runner is the only place that knows how to spawn exiftool. It always
// passes arguments as an argv array (never through a shell), always puts
// the -config option first, and captures the child's stdout and stderr
// separately so nothing but MCP frames ever reach the server's stdout.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/alfa-reza/exiftool-mcp-go/internal/config"
)

// ErrNotFound is returned when the exiftool executable cannot be located
// or executed.
var ErrNotFound = errors.New("exiftool executable not found")

// Result is the outcome of one exiftool invocation.
type Result struct {
	Stdout   []byte
	Stderr   string
	ExitCode int
}

// Runner runs exiftool with the given arguments (excluding -config, which
// the runner prepends itself).
type Runner interface {
	Run(ctx context.Context, args ...string) (Result, error)
	// Path returns the resolved executable path ("" if unknown yet).
	Path() string
	// ConfigMode returns "standard" or "custom".
	ConfigMode() string
}

// ExecRunner is a Runner backed by os/exec.
type ExecRunner struct {
	cfg  config.Config
	path string
	sem  chan struct{}
}

// New resolves the exiftool executable and returns a runner. Resolution
// failure is not fatal here: it is reported on every Run call so the MCP
// server can still start and answer with a clear diagnostic.
func New(cfg config.Config) *ExecRunner {
	r := &ExecRunner{cfg: cfg}
	if cfg.MaxConcurrency > 0 {
		r.sem = make(chan struct{}, cfg.MaxConcurrency)
	}
	p := cfg.ExifToolPath
	if p == "" {
		if found, err := exec.LookPath("exiftool"); err == nil {
			p = found
		}
	}
	r.path = p
	return r
}

func (r *ExecRunner) Path() string { return r.path }

func (r *ExecRunner) ConfigMode() string { return r.cfg.ConfigMode() }

// configArgs returns the leading -config arguments. With no custom config
// this is `-config ""`, which prevents loading any user .ExifTool_config.
func (r *ExecRunner) configArgs() []string {
	return []string{"-config", r.cfg.ExifToolConfig}
}

func (r *ExecRunner) Run(ctx context.Context, args ...string) (Result, error) {
	if r.path == "" {
		return Result{}, fmt.Errorf("%w: set EXIFTOOL_PATH or install exiftool", ErrNotFound)
	}
	if r.sem != nil {
		select {
		case r.sem <- struct{}{}:
			defer func() { <-r.sem }()
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline && r.cfg.CallTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.cfg.CallTimeout)
		defer cancel()
	}
	full := append(r.configArgs(), args...)
	cmd := exec.CommandContext(ctx, r.path, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.String()}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if err != nil {
		// Cancellation and timeout come first: when CommandContext
		// kills the process, cmd.Run reports an *exec.ExitError
		// ("signal: killed"), which must not be mistaken for an
		// ordinary non-zero exit and fed to the parser as data.
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// Non-zero exit is data, not a Go error: callers decide based
			// on stdout/stderr (batch reads exit 1 on partial failures).
			return res, nil
		}
		if errors.Is(err, exec.ErrNotFound) {
			return res, fmt.Errorf("%w: %s", ErrNotFound, r.path)
		}
		return res, fmt.Errorf("running exiftool: %w", err)
	}
	return res, nil
}

// Version returns the output of `exiftool -ver`.
func Version(ctx context.Context, r Runner) (string, error) {
	res, err := r.Run(ctx, "-ver")
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("exiftool -ver failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return strings.TrimSpace(string(res.Stdout)), nil
}

// Operand normalizes a file path for use as a CLI operand so it can never
// be interpreted as an option: relative paths get a "./" prefix when they
// could be mistaken for a flag. It returns an error for the "-" operand
// (stdin), which this server never reads from.
func Operand(path string) (string, error) {
	if path == "-" {
		return "", errors.New("operand \"-\" (stdin) is not supported")
	}
	if path == "" {
		return "", errors.New("empty path")
	}
	if !filepath.IsAbs(path) && strings.HasPrefix(path, "-") {
		return "./" + path, nil
	}
	return path, nil
}

// LooksLikeOption reports whether s could be confused with a CLI flag.
// It is used to validate agent-supplied selectors before they become argv.
func LooksLikeOption(s string) bool {
	return strings.HasPrefix(s, "-") || strings.ContainsAny(s, "=<>+\t\n\r ")
}

var _ Runner = (*ExecRunner)(nil)
