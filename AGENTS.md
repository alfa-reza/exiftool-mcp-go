# AGENTS.md

Guidelines for agents working in this repository.

## Project

An MCP (Model Context Protocol) server for ExifTool, written in Go.

## Rules

- Run `go test ./...` before every commit. Failing tests = no commit.
- Code must be `gofmt`-clean and pass `go vet ./...`.
- Never commit or push without the maintainer's review and explicit approval.
  Show the diff and test results first.
- Small, focused commits. One purpose per commit; say what and why.
- Prefer the standard library. New dependencies need a strong reason.
- Don't delete files or features without explicit instruction.

## Philosophy

**SOLID, adapted to Go**

- **S** — One responsibility per package. Keep MCP transport, tool
  definitions, and exiftool execution in separate layers.
- **O** — Add tools by registering new implementations of the tool
  interface, not by editing the dispatcher.
- **L** — Anything behind an interface (e.g. the exiftool `Runner`) must be
  swappable, including fakes in tests.
- **I** — Small interfaces: one to three methods. No god interfaces.
- **D** — Depend on interfaces, not concrete implementations. Inject the
  exiftool runner; never hardcode it inside business logic.

**KISS** — The simplest solution that works wins. No speculative layers,
no concurrency without a concrete need, readable beats clever.

**DRY** — Share helpers for argument building, output parsing, and error
handling. But don't abstract prematurely: extract only after a pattern
appears three times.

**YAGNI** — Build only what is needed now. No flags, config options,
plugins, or extra output formats "just in case".

## Go conventions

- Wrap errors with `%w`. Never swallow an error silently; surface it as a
  clear MCP tool error.
- Use `context` for timeouts and cancellation of exiftool processes.
- Pass arguments to exiftool as an argv array via `exec.Command` — never
  through a shell string.
- Validate file paths coming from tool input before use.

## Testing

- Unit tests must run without the real exiftool binary — use a fake `Runner`.
- Tests that need the real binary must skip automatically when it is not
  installed, so `go test ./...` stays green anywhere.
