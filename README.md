# wherobots CLI

A command-line interface for the [Wherobots](https://wherobots.com) Cloud API. Submit and manage Spark job runs, stream logs, and access the full Wherobots API surface — all from your terminal.

## Prerequisites

- A Wherobots Cloud account
- **Go 1.25.7+** (only if building from source)

## Installation

### Quick install (curl | bash)

```bash
curl -fsSL https://raw.githubusercontent.com/wherobots/wherobots-cli/main/scripts/install-release.sh | bash
```

This downloads the latest release binary for your OS/arch, verifies its SHA-256 checksum, and installs it to `~/.local/bin/wherobots`.

To pass options (e.g. a custom install directory or release tag), use `bash -s --`:

```bash
curl -fsSL https://raw.githubusercontent.com/wherobots/wherobots-cli/main/scripts/install-release.sh \
  | bash -s -- --install-dir /usr/local/bin --tag latest-prerelease
```

Available flags: `--install-dir`, `--tag`, `--repo`, `--binary-name`, `--skip-checksum`.

### Build from source

```bash
git clone https://github.com/wherobots/wherobots-cli.git
cd wherobots-cli
make build        # produces bin/wherobots
```

## Getting started

1. **Sign in** (opens your browser):

   ```bash
   wherobots auth login
   ```

   Or, for CI and scripts, use an API key instead — see [Authentication](#authentication).

2. **Explore available commands:**

   ```bash
   wherobots --help
   wherobots --tree          # print the full command tree
   ```

3. **Submit a job run:**

   ```bash
   wherobots job-runs create s3://bucket/script.py --name my-job-001 --watch
   ```

## Authentication

The CLI supports two credentials:

- **Browser sign-in (OAuth)** — `wherobots auth login` prints a one-time confirmation code and opens your browser (pass `--no-browser` to get just the URL — handy over SSH, since the confirmation can happen on any device). The session is stored in your user config dir (`~/.config/wherobots/credentials.json` on Linux, `~/Library/Application Support/wherobots/credentials.json` on macOS) with `0600` permissions and refreshes automatically.
- **API key** — create one at your [API keys settings page](https://cloud.wherobots.com/settings#api-keys) and `export WHEROBOTS_API_KEY='<your-api-key>'`. Best for CI and scripts.

When both are present, **`WHEROBOTS_API_KEY` wins** — an explicitly-set env var always overrides a stored sign-in, so scripts behave predictably.

```bash
wherobots auth status    # which credential is active, account, token expiry
wherobots auth logout    # remove the stored session (--all for every environment)
```

To sign in against a non-production environment, point both OAuth variables at its AuthKit tenant (mirroring how `WHEROBOTS_API_URL` selects the API host):

```bash
export WHEROBOTS_API_URL='https://api.staging.wherobots.com'
export WHEROBOTS_OAUTH_DOMAIN='<staging-authkit-domain>'
export WHEROBOTS_OAUTH_CLIENT_ID='<staging-client-id>'
```

Sessions are stored per OAuth domain, so production and staging sign-ins coexist.

## Commands

The CLI has three command groups:

| Group | Description |
|-------|-------------|
| `wherobots job-runs <subcommand>` | Purpose-built commands for creating, monitoring, and listing job runs. |
| `wherobots files my-files <subcommand>` | Commands for your personal area in Wherobots Files: list, create folders, upload, download, rename, delete. |
| `wherobots api <resource> ... <verb>` | Dynamically generated commands covering every Wherobots API endpoint. |

### `job-runs` — Job run management

Curated commands for the most common job-run workflows: submitting runs, streaming logs, listing by status, and viewing metrics.

#### `job-runs create`

Submit a new job run. Accepts an S3 URI or a local file path (which is auto-uploaded to your Wherobots managed directory).

```bash
# submit from an S3 path
wherobots job-runs create s3://bucket/script.py --name my-job-001

# submit a local script (auto-uploaded)
wherobots job-runs create ./script.py --name my-job-001

# submit and stream logs until the run reaches a terminal status
wherobots job-runs create s3://bucket/script.py --name my-job-001 --watch

# override the upload destination for local files
wherobots job-runs create ./script.py --name my-job-001 --upload-path s3://my-bucket/custom/root
```

| Flag | Description | Default |
|------|-------------|---------|
| `-n, --name` | **Required.** Name for the job run. | — |
| `-r, --runtime` | Wherobots runtime size; accepts any runtime string. Leave unset to use your organization's default. Shell completion lists your available runtimes. | _(org default)_ |
| `--run-region` | Region to run the job in; accepts any region string (BYOC regions included). Leave unset to use your organization's default. Shell completion lists your available regions. | _(org default)_ |
| `--timeout` | Run timeout in seconds. | `3600` |
| `--args` | Arguments passed to the script. | — |
| `-c, --spark-config` | Repeatable Spark config key=value pairs. | — |
| `--dep-pypi` | PyPI dependency to install. | — |
| `--dep-file` | File dependency to include. | — |
| `--jar-main-class` | Main class for JAR jobs. | — |
| `-w, --watch` | Stream logs after submission until the run completes. | `false` |
| `--no-upload` | Skip auto-upload of local files. | `false` |
| `--upload-path` | Custom S3 root for uploading local files. | — |
| `--output` | Output format: `text` or `json`. | `json` |

#### `job-runs logs`

Fetch or stream logs for a job run.

```bash
# fetch logs once
wherobots job-runs logs <run-id>

# stream logs until the run completes
wherobots job-runs logs <run-id> --follow

# show only the last 50 lines
wherobots job-runs logs <run-id> --tail 50
```

| Flag | Description | Default |
|------|-------------|---------|
| `-f, --follow` | Stream logs continuously until the run finishes. | `false` |
| `-t, --tail` | Number of most recent lines to display. | — |
| `--interval` | Poll interval in seconds (used with `--follow`). | `2.0` |
| `--output` | Output format: `text` or `json`. | `text` |

#### `job-runs list`

List job runs, optionally filtered by status, name, or region.

```bash
# list recent runs (JSON output)
wherobots job-runs list

# human-readable table
wherobots job-runs list --output text

# filter by status
wherobots job-runs list --status RUNNING --status FAILED
```

| Flag | Description | Default |
|------|-------------|---------|
| `-s, --status` | Repeatable status filter (e.g. `RUNNING`, `FAILED`, `COMPLETED`). | — |
| `--name` | Filter by run name. | — |
| `--after` | Return runs created after this cursor/timestamp. | — |
| `-l, --limit` | Maximum number of results. | `20` |
| `--region` | Filter by region. | — |
| `--output` | Output format: `text` or `json`. | `json` |

#### `job-runs running` / `job-runs failed` / `job-runs completed`

Shorthand aliases equivalent to `job-runs list --status RUNNING`, `--status FAILED`, or `--status COMPLETED`. They accept the same flags as `job-runs list` except `--status`.

```bash
wherobots job-runs running
wherobots job-runs failed --output text
wherobots job-runs completed --limit 5
```

#### `job-runs metrics`

Display instant metrics (CPU, memory, etc.) for a running or recently completed job run.

```bash
wherobots job-runs metrics <run-id>
wherobots job-runs metrics <run-id> --output text
```

| Flag | Description | Default |
|------|-------------|---------|
| `--output` | Output format: `text` or `json`. | `json` |

### `files` — Wherobots Files

Work with one file or folder at a time in your personal Files area (`my-files`). Every path is a remote path, relative to the area's root: a leading `/` is ignored, a trailing `/` names a folder, and empty levels, `.` and `..` are refused.

```bash
wherobots files my-files ls                              # list the root
wherobots files my-files ls reports/2026 --output json   # list a folder as JSON
wherobots files my-files mkdir reports/2026/q3           # creates each missing level in turn
wherobots files my-files upload reports/q3.csv ./q3.csv  # <remote path> <local file>
wherobots files my-files upload reports/ ./q3.csv        # keeps the local name: reports/q3.csv
wherobots files my-files download reports/q3.csv         # saves ./q3.csv
wherobots files my-files download reports/q3.csv ./out/  # saves ./out/q3.csv
wherobots files my-files cat reports/q3.csv              # prints to stdout
wherobots files my-files mv reports/q3.csv q3-final.csv  # rename within the same folder
wherobots files my-files rm reports/q3-final.csv
wherobots files my-files rmdir reports/2026/q3           # refused if not empty
wherobots files my-files rmdir reports --recursive       # deletes the folder and everything in it
```

**Region:** each region has its own files area. Commands use `--region` when given (for example `wherobots files my-files --region aws-us-west-2 ls`), otherwise your organization's default region.

| Flag | Applies to | Description |
|------|------------|-------------|
| `--region` | all | Region of the files area. Default: your organization's default region. |
| `--output` | `ls` | `text` (TYPE / SIZE / MODIFIED / NAME table) or `json`. |
| `--recursive`, `-r` | `rmdir` | Delete a folder that is not empty, with everything in it. |
| `--dry-run` | all | Print each API request as `curl` without sending it. |

Notes:

- `upload` accepts files up to 500 MB.
- `mv` renames within a folder; moving a file to another folder is not supported.
- Downloads go straight to storage through a presigned link; your API key or sign-in token is never sent there. A failed download leaves no partial file.
- Uploads and downloads have no overall time limit; press Ctrl-C to stop a stalled transfer.
- "Files is not enabled for my-files in region X" means the Files area is not available to you in that region; try another `--region`.

### `api` — Full Wherobots API access

The `api` command group is **generated at runtime** from the Wherobots OpenAPI 3.x specification. Every endpoint Wherobots exposes is available as a CLI command — no CLI update required when new API endpoints are released.

> **Note:** Because these commands are generated dynamically from the API spec, the exact set of available resources and verbs may change as the Wherobots API evolves.

```bash
# discover all available api commands
wherobots api --tree

# general form
wherobots api <resource> [<sub-resource> ...] <verb> [flags]
```

**How it works:**

- Resource hierarchy is derived from API URL paths.
- Verbs are derived from `operationId` or inferred from the HTTP method (`GET` → `list`/`get`, `POST` → `create`, etc.).
- Path and query parameters become named flags (e.g. `--id`, `--limit`).
- Object/array request body fields become `*-json` flags (e.g. `--metadata-json '{"k":"v"}'`).
- Run `--help` on any generated command to see all available flags with types and examples.

**Common flags for api commands:**

| Flag | Description |
|------|-------------|
| `--json '<raw-json>'` | Raw JSON request body (overrides individual body-field flags). |
| `-q, --query key=value` | Repeatable query parameter (last value wins for duplicate keys). |
| `--dry-run` | Print the equivalent `curl` command instead of executing the request. |
| `--tree` | Print the command tree from this point. |

## Configuration

All configuration is done through environment variables.

| Variable | Required | Description | Default |
|----------|----------|-------------|---------|
| `WHEROBOTS_API_KEY` | No | API key credential. Sent as `x-api-key` header; overrides a stored OAuth session. Either this or `wherobots auth login` is required. | — |
| `WHEROBOTS_API_URL` | No | Base URL for the Wherobots API. | `https://api.cloud.wherobots.com` |
| `WHEROBOTS_OAUTH_DOMAIN` | No | AuthKit domain used by `wherobots auth login`. | `https://login.cloud.wherobots.com` |
| `WHEROBOTS_OAUTH_CLIENT_ID` | No | OAuth client ID used by `wherobots auth login`. | _(production CLI client)_ |
| `WHEROBOTS_UPLOAD_PATH` | No | Default S3 root for local file uploads in `job-runs create`. | _(auto-resolved from your account)_ |
| `OPENAPI_CACHE_TTL` | No | How long to cache the OpenAPI spec (Go duration, e.g. `15m`). | `15m` |
| `OPENAPI_HTTP_TIMEOUT` | No | Timeout for fetching the OpenAPI spec (Go duration). | _(default)_ |

The OpenAPI spec is cached locally at `~/.cache/wherobots/spec.json`. If a fresh fetch fails, the cached version is used as a fallback.

## Output

- **Success:** raw JSON response body printed to `stdout`.
- **Failure:** error message printed to `stderr` with a non-zero exit code.
- `job-runs` commands support `--output text` for human-readable table output.
- For non-JSON API responses or endpoints requiring file upload/download, use `--dry-run` to get a `curl` command you can execute and customize.

## Dependencies

Built with:

| Dependency | Purpose |
|------------|---------|
| [cobra](https://github.com/spf13/cobra) | CLI framework and command routing |
| [libopenapi](https://github.com/pb33f/libopenapi) | OpenAPI 3.x spec parsing |
| [gjson](https://github.com/tidwall/gjson) / [sjson](https://github.com/tidwall/sjson) | JSON reading and mutation |
| [shlex](https://github.com/google/shlex) | Shell-safe argument quoting (for `--dry-run` curl output) |

## Development

```bash
make test          # run tests
make build         # compile to bin/wherobots
make fmt           # format source code
make tidy          # tidy go.mod dependencies
make run ARGS='--tree'   # run without building
```

- PR validation runs `go test` and `go build` automatically via GitHub Actions.
- Merges to `main` publish a rolling `latest-prerelease` release with binaries for Linux, macOS, and Windows (amd64 and arm64).
- To cut a stable `vX.Y.Z` release, see [RELEASE.md](./RELEASE.md).
