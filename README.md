# exiftool-mcp-go

A read-only [Model Context Protocol](https://modelcontextprotocol.io) (MCP)
server that gives AI agents safe, structured access to the metadata power of
[ExifTool](https://exiftool.org) — written in Go, speaking MCP over stdio.

The server does **not** parse metadata itself. It drives the installed
ExifTool CLI through a typed, validated, read-only argument builder, parses
its JSON output without losing tag identity, and adds the pieces a raw CLI
cannot offer an agent: typed search queries with three-valued logic,
resumable directory scans with durable checkpoints, paged results, and
byte-exact binary extraction.

## Features

- **7 tools**, all annotated read-only:
  - `get_exiftool_info` — executable path, version, config mode, format
    capabilities and server limits.
  - `read_metadata` — per-file metadata with full tag identity
    (`group`, `name`, `document`, `instance`), tag filters, duplicates,
    unknown tags, embedded (`-ee`) levels, numeric/structured values and
    fast-scan levels.
  - `search_metadata` — nestable typed predicate AST (`all`/`any`/`not`,
    15 operators, duplicate quantifiers, `same_document` scope,
    timezone-aware datetime semantics) evaluated with three-valued logic:
    `match` / `no_match` / `indeterminate`. Scans run in batches over a
    frozen candidate snapshot and resume after the response — or after a
    server restart — via `resume_token`.
  - `compare_metadata` — added / removed / changed tags between two files
    read with identical options.
  - `discover_tags` — tag/group discovery from the installed ExifTool
    catalog or observed in real files, with occurrence counts.
  - `extract_binary` — byte-identical extraction of one embedded binary
    tag (thumbnail, preview, ICC profile, …) with SHA-256 and MIME
    detection; images are returned as MCP image content, large payloads
    by reference.
  - `get_result_page` — paging and fragment retrieval for stored results.
    It never runs ExifTool and never resumes a scan.
- **Read-only by construction**: handlers pass only typed option objects;
  an internal mapping produces a fixed allowlist of read flags. File paths
  are operands, never options; no shell is involved anywhere.
- **Durable result store**: pages, binary/text fragments and scan
  checkpoints live in a private per-user cache directory with TTL and a
  disk quota, survive restarts, and commit atomically (a crash mid-batch
  can only lose the uncommitted batch — and resume detects that and
  reports `CHECKPOINT_CORRUPT` instead of silently duplicating records).
- **Honest results**: one file's failure never discards other files' data;
  empty value ≠ missing tag ≠ unevaluable condition ≠ read failure.

## Requirements

- **ExifTool** installed and on `PATH` (or pointed to by `EXIFTOOL_PATH`).
  Developed and tested against ExifTool 13.55.
- **Go 1.25+** to build from source.

## Install

```sh
go install github.com/alfa-reza/exiftool-mcp-go/cmd/exiftool-mcp-go@v1.0.0
```

or from a checkout:

```sh
git clone https://github.com/alfa-reza/exiftool-mcp-go
cd exiftool-mcp-go
go build -o exiftool-mcp-go ./cmd/exiftool-mcp-go
```

## Client configuration

The server is a stdio subprocess: the client launches the binary, with no
port and no API key. Generic MCP client configuration:

```json
{
  "mcpServers": {
    "exiftool": {
      "command": "/absolute/path/to/exiftool-mcp-go",
      "args": []
    }
  }
}
```

See [`examples/`](examples/) for a ready-to-copy generic configuration and
an OpenCode configuration.

## Configuration

All configuration happens at startup through environment variables —
never through tool parameters:

| Variable             | Meaning                                             |
|----------------------|-----------------------------------------------------|
| `EXIFTOOL_PATH`      | Explicit ExifTool executable path.                  |
| `EXIFTOOL_CONFIG`    | Path to a trusted custom ExifTool config file.      |
| `EXIFTOOL_MCP_STORE` | Result-store directory override.                    |
| `EXIFTOOL_MCP_TTL_HOURS` | Result lifetime in hours (default 24).          |

By default every ExifTool invocation starts with `-config ""`, so **no**
user `.ExifTool_config` is loaded. A custom config selected with
`EXIFTOOL_CONFIG` is arbitrary Perl code executed by ExifTool: only use
configs you trust. It is never sandboxed, and its contents are never
exposed through tool output — only the mode (`standard`/`custom`) is.

Limits, checkpoint semantics and the store layout are documented in
[`docs/configuration.md`](docs/configuration.md).

## Read-only boundary

- The server never writes to source files; tests assert SHA-256 equality
  of fixtures before and after reads, searches, comparisons and binary
  extraction.
- "Read-only" covers source files. The server does create its own cache
  files (stored results and checkpoints) under the store directory and
  cleans them up automatically by TTL and quota.
- Metadata values are untrusted data: they are transported as data, never
  evaluated as code or instructions.

## Development

```sh
go test ./...        # unit + integration + stdio end-to-end tests
go test -race ./...
go vet ./...
```

Integration tests skip automatically when no `exiftool` binary is
available. Fixtures are generated deterministically in test setup with the
real CLI and verified against it — no internet photos.

Measured on a modest 2-vCPU VPS (ExifTool 13.55): enumerating a frozen
snapshot of 2,000 candidate files takes ~3.5 ms; scan batches therefore
default to 200 files or 20 s per `search_metadata` call, whichever comes
first.

## License

Apache License 2.0 — see [`LICENSE`](LICENSE).

Third-party dependencies and their licenses are listed in
[`THIRD-PARTY-NOTICES.md`](THIRD-PARTY-NOTICES.md). ExifTool is an
external runtime prerequisite invoked as a separate program; it is not
bundled with or distributed by this project.
