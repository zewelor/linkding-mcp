# Linkding MCP

A minimal local MCP server for an existing Linkding instance: search bookmarks, read notes and tags, and save URLs. Transport: **stdio**. It does not open a port.

## Getting started

Requires Go 1.27.1. `just` provides convenient shortcuts; `go build -o bin/linkding-mcp .` also builds the binary directly.

```sh
just build
export LINKDING_URL='https://linkding.example.com'
read -rs LINKDING_TOKEN
export LINKDING_TOKEN
./bin/linkding-mcp
```

The MCP client launches the process and passes these two environment variables. `LINKDING_URL` is the instance's base URL, such as `https://example.com/linkding/`, without the `/api` suffix. The API token is available in Linkding's settings. Diagnostics go to stderr; stdout is reserved for MCP.

Example configuration for a client that supports MCP configuration in JSON (the parent process provides the environment variables):

```json
{
  "mcpServers": {
    "linkding": {
      "command": "/absolute/path/to/linkding-mcp/bin/linkding-mcp"
    }
  }
}
```

### Docker (Linux)

```sh
docker run --rm -i --pull=always \
  --read-only --user 65532:65532 --cap-drop ALL \
  --security-opt no-new-privileges:true --log-driver none \
  -e LINKDING_URL -e LINKDING_TOKEN ghcr.io/zewelor/linkding-mcp:latest
```

Use `-i` without `-t`. The image contains a static binary, CA certificates, and user `65532:65532`. The Linkding instance must be reachable from the container.

`latest` is updated only after CI passes on `main`. The exact image tested by E2E is published, also with the tag `sha-<full commit SHA>`; the publishing job verifies that the image IDs match. Currently, Linux/amd64 is supported. `--pull=always` downloads the current image at every launch and requires GHCR to be available. For local development, use `just docker-build` (tag `linkding-mcp:e2e`).

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
  "-e", "LINKDING_URL", "-e", "LINKDING_TOKEN", "ghcr.io/zewelor/linkding-mcp:latest",
]
env_vars = ["LINKDING_URL", "LINKDING_TOKEN"]
startup_timeout_sec = 120
```

`env_vars` forwards variables from the Codex process environment, and `docker -e` passes them into the container. This does not load `.zshrc`. Export both variables **before starting Codex**; starting a new conversation does not change the environment of an already running process. The CLI may use a shared daemon that retains the environment from an earlier launch. To launch an independent CLI with the current exports, use `zsh -ic 'exec codex --no-daemon'`. See the [official Codex MCP documentation](https://developers.openai.com/codex/mcp).

## Tools

| Tool | Arguments | Result |
| --- | --- | --- |
| `list_bookmarks` | optional `query` (including `#tag` syntax), optional `offset >= 0` | `count`, up to 20 bookmarks |
| `list_tags` | optional `offset >= 0` | `count`, up to 20 tags (`id`, `name`) |
| `get_bookmark` | positive `id` | bookmark including `notes` |
| `save_bookmark` | HTTP/HTTPS URL | `created` or `already_exists`, `id`, `url`, `title` |

Results are structured MCP JSON. A bookmark contains `id`, `url`, `title`, `description`, and `tag_names`; bookmark details also include `notes`. A tag contains `id` and `name`. Empty lists are arrays: `[]`.

Search uses [Linkding's syntax](https://linkding.link/search/), such as `#go`. The offset is the index of the first result and defaults to 0. Fetch subsequent pages by increasing the offset by the number of returned items until you reach `count`. The API does not return web page contents; `notes` are notes stored with the bookmark.

Saving sends only the URL; Linkding determines metadata and automatic tags. An existing bookmark does not trigger a POST. Checking and saving **are not atomic**: a concurrent save may update an existing bookmark. An interrupted POST is not retried, and its outcome may be unknown; check the bookmark before trying again. `/check/` may fetch page metadata.

An error or malformed `/check/` response does not trigger a POST. API errors, invalid JSON, and incomplete save responses are reported as tool errors. There are no automatic retries. `created` means that a POST succeeded after the earlier check found no bookmark; it does not guarantee exclusive creation. A concurrent save may change an existing bookmark's tags, notes, and flags.

Configuration requires an absolute HTTP/HTTPS instance URL without credentials, a query, or a fragment, and a nonempty token without invalid header characters. HTTP requests propagate cancellation, have a 30-second timeout and a 2 MiB response limit, and do not follow redirects. The token is sent only to the configured instance. Invalid configuration terminates the process with diagnostics on stderr; diagnostics do not expose the token or raw API error responses.

## Verification

```sh
just test                # binary over stdio + local API fixture
just docker-build
just test-docker         # the same scenarios through the container
just show_dockerignore   # actual build context
just ci                 # full local validation
```

During development: `go test -count=1 -run 'TestE2E/new_url_saved_once' ./...`. E2E uses only a local fixture and a dummy token. Artifacts: `artifacts/e2e.json` and `artifacts/e2e-docker.json`; they contain results, HTTP sequences, and authorization checks without the token value. The test container uses the host network to reach the fixture on loopback. Compatibility with a live instance and hosted CI requires separate validation.

Development and validation rules: [AGENTS.md](AGENTS.md). Local skills are stored only in `.agents/skills`; `skills-lock.json` records their sources and versions.

Official Linkding sources: [REST API](https://linkding.link/api/), [search syntax](https://linkding.link/search/), [source code](https://github.com/sissbruecker/linkding), and [releases](https://github.com/sissbruecker/linkding/releases). [AGENTS.md](AGENTS.md) describes source verification rules for agents.
