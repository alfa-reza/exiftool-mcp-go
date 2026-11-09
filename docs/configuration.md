# Configuration and operational semantics

## Environment variables (startup only)

| Variable | Default | Meaning |
|---|---|---|
| `EXIFTOOL_PATH` | lookup of `exiftool` in `PATH` | Explicit ExifTool executable. |
| `EXIFTOOL_CONFIG` | *(none)* | Absolute path to a trusted custom ExifTool config file. When unset, every invocation starts with `-config ""` so no user `.ExifTool_config` is loaded. |
| `EXIFTOOL_MCP_STORE` | `os.UserCacheDir()/exiftool-mcp-go` | Result-store directory. |
| `EXIFTOOL_MCP_TTL_HOURS` | `24` | Lifetime of stored results and scan checkpoints. |

Tool calls never accept executable paths, config paths, or free-form
ExifTool arguments.

## Trust boundary: custom ExifTool configs

A file selected with `EXIFTOOL_CONFIG` is arbitrary Perl code executed
by the ExifTool process with the server user's permissions. Treat it
exactly like a program you would run yourself. The server reports only
the config *mode* (`standard` / `custom`) in `get_exiftool_info`, never
the config's path or contents.

## Default limits

| Limit | Default | Kind |
|---|---|---|
| Per-invocation timeout | 60 s | work |
| Concurrent ExifTool processes | 4 | work |
| Files per search batch | 200 | work |
| Search batch wall-clock budget | 20 s | work |
| Structured response budget | 1 MiB | response |
| Default page size | 100 records | response |
| Inline binary limit | 1 MiB | response |
| Result TTL | 24 h | storage |
| Store quota | 1 GiB | storage |

Work limits bound what one call does; response limits bound what one
response carries. Overflow is always stored and retrievable — limits
never silently truncate results. `scan_complete=true` is reported only
when every candidate of the frozen snapshot was actually processed;
`has_more=true` only when more stored data can actually be retrieved.

## Result store

Layout under the store directory (permissions `0700`, files `0600`):

```
results/<id>/meta.json        result metadata (source of truth)
results/<id>/seg-NNNNNN.jsonl committed record segments
results/<id>/blob.bin         binary payload
results/<id>/text.txt         text payload
scans/<id>/job.json           search scan checkpoint
```

Handles (`result_id`, `resume_token`) are opaque 128-bit random values;
they contain no filesystem paths. Metadata writes are atomic (temporary
file + rename). A segment not referenced by `meta.json` is ignored, so a
crash mid-batch leaves only committed batches visible. On resume, the
checkpoint's committed-record count is reconciled with the store; a
mismatch fails with `CHECKPOINT_CORRUPT` rather than duplicating
records.

Cleanup runs at startup and removes expired results/scans; if the quota
is exceeded, the oldest completed results are removed first.

## Scan snapshot semantics

When a search starts, the candidate list (paths plus size/mtime at
discovery) is frozen **before** the first batch is committed:

- Files created later never join the scan (a new search sees them).
- A file that disappears before its turn is reported as an error.
- A file whose size/mtime changed since discovery is processed from its
  **current** contents and flagged `changed_since_discovery=true` in its
  result record. `false` is not cryptographic proof of no change.
- The snapshot freezes the candidate list, not file bytes: results
  reflect each file's metadata at the time its batch ran.

## Enumeration policy

- Symlinked directories are never descended into (no loops).
- Symlinked regular files are followed.
- Non-regular files (devices, FIFOs, sockets) are skipped.
- Hidden files are included.
- The extension filter is case-insensitive; candidates are sorted by
  path so snapshots are deterministic.

## Datetime search semantics

- Naive vs naive values compare as wall-clock times; aware vs aware as
  absolute instants.
- Mixed comparisons require an explicit `time_context.timezone` (IANA
  name or fixed offset); without one they evaluate to `indeterminate`.
- A bare date bound (`YYYY-MM-DD`) denotes the full calendar day:
  inclusive as a lower bound and through end-of-day as an upper bound of
  `between`.

## Error codes

Stable codes used in tool output: `INVALID_ARGUMENT`,
`EXIFTOOL_NOT_FOUND`, `PATH_NOT_FOUND`, `PERMISSION_DENIED`,
`EXTRACTION_FAILED`, `PARTIAL_READ`, `TIMEOUT`, `CANCELLED`,
`RESULT_EXPIRED`, `INVALID_CURSOR`, `RESULT_STORE_FULL`,
`CHECKPOINT_CORRUPT`, `BINARY_NOT_FOUND`, `BINARY_TOO_LARGE`.
`INDETERMINATE` is a per-file search evaluation status, not a tool
error.
