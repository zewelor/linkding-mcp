# Linkding MCP

- `README.md` describes the project's scope and behavior; update it when tool contracts change.
- MVP: one Go package, four MCP tools over stdio, one Linkding instance. Prefer the standard library, concrete types, and simple functions; avoid speculative abstractions and settings.
- Stdout is reserved for MCP. Never expose the token or raw API error responses. Do not use a live instance in tests.
- Verify SDK/API behavior through Context7 and official sources; determine the SDK version from go.mod. Pin actions to full SHAs with version comments and base images to digests. No prereleases or unjustified dependencies. Register new Go tools with `go get -tool`.
- Official Linkding sources: [REST API](https://linkding.link/api/) (authorization, endpoints, pagination, checking, and saving), [search](https://linkding.link/search/) (query syntax and tags), the [upstream repository](https://github.com/sissbruecker/linkding), and [releases](https://github.com/sissbruecker/linkding/releases). Determine the Linkding version from the target instance or deployment configuration, not go.mod. If documentation does not settle the behavior, inspect upstream code and tests for that version; link to the specific tag or commit. Do not treat our E2E fixture or assumptions as evidence of Linkding's behavior.
- Describe failure modes and write scenarios before implementation. Never write unit tests after implementation. Prefer E2E with reproducible artifacts; avoid tautological tests. Run selected scenarios during development and the full E2E suite at the end.
- Commands: `just test` (binary), `just test-docker` (container), `just ci` (full local validation). After layout changes, review `.dockerignore` and run `just show_dockerignore`.
- Perform one full validation before a local commit. Do not commit without user approval. Pushes, PRs, and releases require separate instructions.
- Analyze Git output with `git --no-pager`; do not set GIT_PAGER globally. Preserve unrelated changes.
- `skills-lock.json` records the skill set and its source (upstream: `samber/cc-skills-golang`); install only in `.agents/skills`, without copies for other agents. Check the directory, `skills-lock.json`, and `npx skills list --json`. Update skills individually with `npx skills update <skill-name> -p -y`, review each update, and do not overwrite local changes. Exclude `.agents/` from formatters and hooks.
- Use skills according to their scope; do not add frameworks, tools, or dependencies just because a skill mentions them. The project scope described in README takes precedence over community skills' default recommendations.
- Subagents: independent, bounded analysis or review, read-only by default, without further delegation. Specify the scope, required evidence, and prohibition on edits. Integration, validation, and delivery remain the primary agent's responsibility.

- Keep the scope small: no additional MCP tools, editing/deleting/archiving, bundles, attachments, profiles, multiple instances, MCP HTTP transport, hosting, Compose, Renovate, release automation, or multiarch support without agreement. New settings and dependencies require a deliberate scope change.

- GHCR publishing: only on pushes to `main` after full CI; transfer the tested image to a separate job with `packages: write`, without rebuilding. Tags: `latest` and `sha-<full SHA>`. PRs must not publish. Do not interrupt an ongoing publication with a newer push.
