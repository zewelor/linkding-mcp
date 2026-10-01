# Linkding

A minimal local CLI and MCP server for one existing Linkding instance: search active or archived bookmarks, read notes and tags, and save or edit bookmark metadata. Both interfaces use the same API methods and result types. MCP transport: **stdio**. The program does not open a port.

## Getting started

Requires Go 1.27.1. `just` provides convenient shortcuts; `go build -o bin/linkding .` also builds the binary directly.

```sh
just build
export LINKDING_URL='https://linkding.example.com'
read -rs LINKDING_TOKEN
export LINKDING_TOKEN
./bin/linkding bookmarks list --json
```

`LINKDING_URL` is the instance's base URL, such as `https://example.com/linkding/`, without the `/api` suffix. The API token is available in Linkding's settings. The program reads its process environment; it does not source shell startup files or prompt for login. Never pass the token as a command argument.

Without arguments, the binary shows help. `linkding mcp` starts the MCP server; the client launches that process and passes the same environment variables. Diagnostics go to stderr; stdout contains JSON in CLI mode and protocol messages in MCP mode.

### CLI

```sh
./bin/linkding bookmarks list --query 'kubernetes (#go or #rust)' --json
./bin/linkding bookmarks list --archived --offset 20 --json
./bin/linkding bookmarks get 123 --json
./bin/linkding tags list --json
./bin/linkding skills get core
./bin/linkding schema bookmarks save --json
```

Flags follow their resource and operation. `get` accepts its ID before or after flags. API operations return JSON: indented by default and compact with `--json`. Empty results succeed and contain `[]`. Exit codes: `0` success, `1` API/transport/configuration error, `2` invalid arguments or input, `130` interrupted. Help, version, skills, and schema work offline without credentials.

Save or update metadata with exactly one JSON object on stdin:

```sh
./bin/linkding bookmarks save --input - --json <<'JSON'
{
  "url": "https://example.com/article",
  "notes": "Read later.\nCheck the examples.",
  "tag_names": ["go", "reading"]
}
JSON
```

The input is limited to 2 MiB. Field names are case-sensitive; unknown or repeated fields, invalid types, and trailing JSON are rejected before HTTP. The save behavior, optional-field semantics, pagination, and search syntax below apply to both CLI and MCP. Only `url` is required. A supplied `tag_names` list replaces all manually assigned tags.

`skills get core` serves the workflow guide embedded in the installed binary. `schema [resource operation] --json` returns the CLI input bindings and exact MCP input/output schemas, including descriptions and read-only hints, plus the CLI exit codes. It uses an in-memory SDK session without an API client or network connection. There is one workflow skill, `core`.

### Native MCP

Example configuration for a client that supports MCP configuration in JSON (the parent process provides the environment variables):

```json
{
  "mcpServers": {
    "linkding": {
      "command": "/absolute/path/to/linkding/bin/linkding",
      "args": ["mcp"]
    }
  }
}
```

### Docker (Linux)

```sh
docker run --rm -i --pull=always \
  --read-only --user 65532:65532 --cap-drop ALL \
  --security-opt no-new-privileges:true --log-driver none \
  -e LINKDING_URL -e LINKDING_TOKEN ghcr.io/zewelor/linkding:latest
```

Use `-i` without `-t`. The image contains a static binary, its embedded CLI guide, CA certificates, and user `65532:65532`. It defaults to `mcp`; append arguments after the image name to use CLI, for example `bookmarks list --query '#go' --json`. The Linkding instance must be reachable from the container.

`latest` is updated only after CI passes on `main`. The exact image tested by E2E is published, also with the tag `sha-<full commit SHA>`; the publishing job verifies that the image IDs match. Currently, Linux/amd64 is supported. `--pull=always` downloads the current image at every launch and requires GHCR to be available. For local development, use `just docker-build` (tag `linkding:e2e`).

The container has no writable filesystem, additional capabilities, exposed ports, or host mounts. Docker's default seccomp/AppArmor filters apply where supported by the host. No additional CPU, memory, process, or ulimit restrictions are configured. Docker log persistence is disabled. Network access is required for Linkding's API; a regular bridge network does not restrict outbound access to a single domain.

### Codex via Docker

In `~/.codex/config.toml`:

```toml
[mcp_servers.linkding]
command = "docker"
args = [
  "run", "--rm", "-i", "--pull=always",
  "--read-only", "--user", "65532:65532", "--cap-drop", "ALL",
  "--security-opt", "no-new-privileges:true", "--log-driver", "none",
  "-e", "LINKDING_URL", "-e", "LINKDING_TOKEN", "ghcr.io/zewelor/linkding:latest",
]
env_vars = ["LINKDING_URL", "LINKDING_TOKEN"]
startup_timeout_sec = 120
```

`env_vars` forwards variables from the Codex process environment, and `docker -e` passes them into the container. This does not load `.zshrc`. Export both variables **before starting Codex**; starting a new conversation does not change the environment of an already running process. The CLI may use a shared daemon that retains the environment from an earlier launch. To launch an independent CLI with the current exports, use `zsh -ic 'exec codex --no-daemon'`. See the [official Codex MCP documentation](https://developers.openai.com/codex/mcp).

## Tools

The CLI equivalents are `bookmarks list`, `tags list`, `bookmarks get`, and `bookmarks save --input -`. MCP tool names remain unchanged.

| Tool | Arguments | Result |
| --- | --- | --- |
| `list_bookmarks` | optional `query` (including `#tag` syntax), optional `offset >= 0`, optional `archived` (default `false`) | `count`, up to 20 bookmarks |
| `list_tags` | optional `offset >= 0` | `count`, up to 20 tags (`id`, `name`) |
| `get_bookmark` | positive `id` | bookmark including `notes` |
| `save_bookmark` | HTTP/HTTPS `url`; optional `title`, `description`, `notes`, `tag_names` | `created`, `already_exists` or `updated`, `id`, `url`, `title` |

Results are structured MCP JSON. A bookmark contains `id`, `url`, `title`, `description`, `tag_names`, `date_added`, and `date_modified`; bookmark details also include `notes`. Dates are ISO 8601 strings passed through from Linkding, preserving precision and timezone. A tag contains `id` and `name`. Empty lists are arrays: `[]`.

`list_bookmarks` lists non-archived bookmarks by default. Set `archived: true` to search only archived bookmarks using the same query and pagination options. Search both states with two calls. The tool preserves the API's order. In Linkding 1.47.0, the endpoint defaults to newest first by `date_added`; offset 0 therefore returns the newest matching bookmarks. Use `date_added` to determine recency, not IDs or `date_modified`. This behavior was checked against the version's [API route](https://github.com/sissbruecker/linkding/blob/v1.47.0/bookmarks/api/routes.py), [search defaults](https://github.com/sissbruecker/linkding/blob/v1.47.0/bookmarks/models.py), and [ordering](https://github.com/sissbruecker/linkding/blob/v1.47.0/bookmarks/queries.py).

Search uses [Linkding's syntax](https://linkding.link/search/) across title, description, notes, and URL. Omit `query` or leave it empty to list all bookmarks in the selected archive state. With the search engine introduced in Linkding 1.44.0 and legacy search disabled:

| Query | Meaning |
| --- | --- |
| `#go` | Bookmarks tagged `go` |
| `"exact phrase"` | Match an exact phrase |
| `kubernetes #go` | Match both the word and the tag (implicit AND) |
| `#go or #rust` | Match either tag |
| `#go not #readlater` | Match `go` and exclude `readlater` |
| `kubernetes (#go or #rust)` | Combine text with a grouped tag expression |

The offset is the index of the first result and defaults to 0. `count` is the total number of matches across all pages, not the current page size. Fetch subsequent pages by increasing the offset by the number of returned items until you reach `count`. The API does not return web page contents; `notes` are notes stored with the bookmark.

With only `url`, `save_bookmark` keeps its original behavior: Linkding determines metadata and automatic tags for a new bookmark; an existing bookmark is returned unchanged with `already_exists`. If optional fields are supplied for an existing URL, the tool sends a PATCH containing only those fields and returns `updated`. This also works for an archived bookmark and does not change its archive state. The tool does not change bookmark URLs or flags.

| Optional field | Omitted or `null` | Explicit empty value |
| --- | --- | --- |
| `title`, `description` | Preserve existing value; fetch metadata on creation | `""` clears an existing value; Linkding fetches metadata on creation |
| `notes` | Preserve existing notes; empty on creation | `""` clears notes |
| `tag_names` | Preserve existing tags; use automatic tags on creation | `[]` clears manually assigned tags |

`tag_names` is the complete replacement tag list. Call `list_tags` first to reuse existing names, and include every tag you want to keep when editing. Linkding can create new tag names and adds tags from matching automatic rules on creation and updates, even when `tag_names` is omitted or empty. These semantics follow the [official API contract](https://linkding.link/api/).

Empty-field updates and tag replacement were also checked against Linkding 1.47.0's [serializer](https://github.com/sissbruecker/linkding/blob/v1.47.0/bookmarks/api/serializers.py#L152-L161) and [tag assignment](https://github.com/sissbruecker/linkding/blob/v1.47.0/bookmarks/services/bookmarks.py#L226-L244). The [existing-URL query](https://github.com/sissbruecker/linkding/blob/v1.47.0/bookmarks/models.py#L105-L113) includes archived bookmarks.

For example, use `save_bookmark` with:

```json
{
  "url": "https://github.com/germondai/trawl",
  "description": "Self-hosted web scraping with JavaScript, CAPTCHA handling and MCP tools.",
  "tag_names": ["scraping", "selfhosted"]
}
```

Checking and writing **are not atomic**: a concurrent save may update an existing bookmark. Interrupted POST and PATCH requests are not retried, and their outcome may be unknown; check the bookmark before trying again. `/check/` may fetch page metadata.

An error or malformed `/check/` response does not trigger a write. API errors, invalid JSON, and incomplete save responses are reported as tool errors. There are no automatic retries. `created` means that a POST succeeded after the earlier check found no bookmark; it does not guarantee exclusive creation. A concurrent save may change an existing bookmark's tags, notes, and flags.

Configuration requires an absolute HTTP/HTTPS instance URL without credentials, a query, or a fragment, and a nonempty token without invalid header characters. HTTP requests propagate cancellation, have a 30-second timeout and a 2 MiB response limit, and do not follow redirects. The token is sent only to the configured instance. Invalid configuration terminates the process with diagnostics on stderr; diagnostics do not expose the token or raw API error responses.

## Verification

```sh
just test                # binary over stdio + local API fixture
just docker-build
just test-docker         # the same scenarios through the container
just show_dockerignore   # actual build context
just ci                 # full local validation
```

During development: `go test -count=1 -run 'TestE2E/new_url_saved_once' ./...`. E2E uses only a local fixture and a dummy token. Artifacts: `artifacts/e2e.json` and `artifacts/e2e-docker.json`; they contain the published tool schemas, results, HTTP sequences, and authorization checks without the token value. The bookmark pagination scenarios verify active and archive routes, date passthrough (including subsecond precision and timezone), API order preservation with IDs that do not follow date order, and total match counts across pages. Metadata scenarios cover creation, partial updates, clearing fields and tags, URL-only no-op, and interrupted PATCH without retry. The detail scenario verifies dates together with stored notes. These scenarios verify the adapter contract; the fixture does not establish Linkding's ordering or search behavior. The test container uses the host network to reach the fixture on loopback. Compatibility with a live instance and hosted CI requires separate validation.

CLI E2E also exercises a multi-step search/read/create/edit/clear workflow, null versus omitted fields, interrupted PATCH without retry, API error sanitization, offline discovery, invalid input, and equality between CLI schema output and the actual MCP tool inventory. Artifacts: `artifacts/e2e-cli-native.json` and `artifacts/e2e-cli-docker.json`, including arguments, stdin, stdout, stderr, exit codes, and observed HTTP requests. All tests use a local fixture and dummy token.

Development and validation rules: [AGENTS.md](AGENTS.md). Development skills are stored only in `.agents/skills`; `skills-lock.json` records their sources and versions. The distributable Linkding skill belongs to the plugin below.

## Agent skill and Codex plugin

The repo ships a portable Agent Plugins package at [plugins/linkding](plugins/linkding/plugin.json), with OpenAI presentation metadata under `extensions.com.openai`, one discovery skill, and a repo marketplace at `.agents/plugins/marketplace.json`. This follows the [current OpenAI plugin format](https://developers.openai.com/plugins/build/plugins). The package provides CLI instructions; install the Linkding executable separately and make it available on the agent process's `PATH`.

From a checkout:

```sh
just build
export PATH="$PWD/bin:$PATH"
codex plugin marketplace add .
codex plugin add linkding@linkding
```

Alternatively, add the marketplace directly from GitHub after installing the executable:

```sh
codex plugin marketplace add zewelor/linkding
codex plugin add linkding@linkding
```

Export `LINKDING_URL` and `LINKDING_TOKEN` before starting the agent, as described above. Restart or start a new agent session after installing the plugin. The skill loads `linkding skills get core` at runtime, so detailed instructions match the installed CLI. Use `linkding schema ... --json` for exact contracts. Plugin and binary updates are separate; the plugin does not download or install executables.

The skill can also be installed independently from `plugins/linkding/skills/linkding` into an agent's supported skills directory. Keep the short discovery skill intact; the binary supplies the detailed guide. This workflow requires a local environment where the agent can execute `linkding`. The package does not bundle a hosted MCP connection or runtime hooks. Use the native or Docker MCP configuration above when that interface is desired.

## Migration from linkding-mcp

The executable, Go module, repository URLs, image references, and build tags now use `linkding`. Native MCP clients must change their executable path and add `args = ["mcp"]`; bare native invocation now prints help. Docker retains its default MCP behavior through `CMD ["mcp"]` and uses the new image reference. The four MCP tool names, environment variables, and API behavior stay the same.

The GitHub repository is `zewelor/linkding`. Update an existing checkout's remote with `git remote set-url origin git@github.com:zewelor/linkding.git` and rename its directory to `linkding`; update any absolute client or local marketplace paths accordingly. GHCR publication uses `ghcr.io/zewelor/linkding` and is gated on pushes to `main`; it still transfers the tested image to a separate publishing job without rebuilding. Existing image tags remain available for clients that have not migrated.

Official Linkding sources: [REST API](https://linkding.link/api/), [search syntax](https://linkding.link/search/), [source code](https://github.com/sissbruecker/linkding), and [releases](https://github.com/sissbruecker/linkding/releases). [AGENTS.md](AGENTS.md) describes source verification rules for agents.
