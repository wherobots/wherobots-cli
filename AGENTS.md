# wherobots-cli

Guidance for AI coding agents (Claude Code, Copilot, Codex) working in this repository.

## What This Is

`wherobots` CLI — a Go command-line tool for the Wherobots Cloud API. It has three command groups:
- **`job-runs`** — curated commands for submitting Spark jobs, streaming logs, listing runs, and viewing metrics
- **`files`** — curated commands for Wherobots Files (`files my-files ls|mkdir|upload|download|cat|mv|rm|rmdir`)
- **Dynamic API commands** — generated at runtime from the Wherobots OpenAPI spec, so every API endpoint is available as a CLI command

## Build & Development Commands

```bash
make build          # compile to bin/wherobots
make test           # go test ./...
make fmt            # go fmt ./...
make tidy           # go mod tidy
make run ARGS='...' # run without building (e.g., make run ARGS='job-runs list')
make clean          # remove bin/
```

Run a single test:
```bash
go test -run TestName ./internal/commands/
```

Runtime credentials: `wherobots auth login` (OAuth device flow, stored session) or the `WHEROBOTS_API_KEY` env var. The env var takes precedence when both are present.

## Architecture

### Dynamic Command Generation

The CLI builds its command tree at startup from a live OpenAPI spec. `internal/spec/loader.go` fetches and caches the spec (`~/.cache/wherobots/spec.json`, 15min TTL), `internal/spec/parser.go` extracts operations, and `internal/commands/builder.go` converts each operation into a Cobra command with flags for path params, query params, and request body fields.

### Curated `job-runs` Commands

`internal/commands/jobs.go` defines hand-written commands (`create`, `logs`, `list`, `running`, `failed`, `completed`, `metrics`) that layer workflow logic on top of the API: auto-uploading local scripts to S3 via presigned URLs, log streaming with polling, status watching, and formatted output.

### Curated `files` Commands

`internal/commands/files.go` builds the `files` → `my-files` → verb tree over the `/storage/{storage_id}/...` file routes (group skipped if the spec lacks them). `internal/files` holds the logic: `drive.go` maps a drive to its storage id (`my-files` → `user_files::<region>`; a future shared drive adds a kind there), and `ops.go` implements every operation once against a drive — path checks, `limit`+cursor listing, level-by-level `mkdir`, presigned upload/download, and mapping of 401/404 to "not signed in", "Files is not enabled for <drive> in region <r>" and "no such file or folder". Region is `--region`, else the org's `defaultRegion`.

### Request Execution Pipeline

`internal/executor/request.go` builds authenticated HTTP requests via the `Credentials` interface (implemented by `internal/auth.Resolver`: `x-api-key` header for API keys, `Authorization: Bearer` for OAuth sessions, with proactive refresh and a one-shot 401 refresh-replay in `DoWithReauth`). `dryrun.go` outputs the equivalent curl command when `--dry-run` is used. `upload.go` handles S3 presigned-URL uploads with a 500MB limit. `download.go` resolves a download redirect without following it and fetches the presigned URL with no auth headers. `BuildRequestMultiSegment` keeps `/` in named greedy path params (the Files `{path}`), escaping each segment; `BuildRequest` escapes `/` as `%2F`.

### Authentication

`internal/auth` implements OAuth sign-in against WorkOS AuthKit using the RFC 8628 device flow (`device.go`), an on-disk session store keyed by OAuth domain (`store.go`, `credentials.json` under `os.UserConfigDir()/wherobots/`, 0600, atomic writes), unverified JWT claim decoding for display (`jwt.go`), and the request-time credential resolver (`resolver.go`; env API key always beats a stored session). `auth login|logout|status` live in `internal/commands/auth.go` and are dispatched spec-free from `main.go` (see `commands.IsSpecFreeInvocation`), so they work with no credentials, cached spec, or API connectivity. Provisioning a new OAuth client is documented in `docs/oauth-setup.md`.

### Key Packages

| Package | Role |
|---------|------|
| `internal/commands` | Cobra command builders — both dynamic (builder.go) and curated (jobs.go, files.go) |
| `internal/files` | Wherobots Files drives and file operations used by `files` commands |
| `internal/spec` | OpenAPI spec fetching, caching, and parsing |
| `internal/executor` | HTTP request construction, execution, dry-run, file upload and download |
| `internal/config` | Env-var-based configuration loading |
| `internal/hints` | Schema-aware error messages for invalid arguments |
| `internal/version` | Background update checking via `gh release view` |

### Version Injection

`main.go` has `buildVersion`, `commit`, `date` vars injected via ldflags at build time. Local builds show `dev`.

## CI/CD

- **PR validation** (`.github/workflows/pr-validate.yml`): runs `go test` and `go build` on every PR
- **Release** (`.github/workflows/release.yml`): on push to main, builds all 6 platform binaries (darwin/linux/windows x amd64/arm64), generates SHA-256 checksums, publishes as rolling `latest-prerelease` GitHub release
- **GoReleaser** (`.goreleaser.yaml`): configures cross-platform builds and archive formats
