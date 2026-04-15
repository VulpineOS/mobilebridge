# mobilebridge — repo rules for AI coding agents

This is a **public MIT-licensed** repository. Assume every commit is world-readable forever.

## Hard rules

- **Scope:** Android-only CDP bridge. Nothing else belongs in this repo.
- **Never reference or import** any private sibling project, private directory, private product codename, or internal tooling by name. The only acceptable mention of iOS is a one-liner saying iOS support is part of the broader VulpineOS commercial offering.
- **Push only** to `VulpineOS/mobilebridge`. Never to `CloverLabsAI` or any unrelated remote. Verify with `gh repo view` if unsure.
- **Commits:** one-line messages, no co-authors, no `Co-Authored-By` trailers, no `Generated with Claude Code` footers. Commit and push after each cohesive change.
- **License:** MIT. Any new file that needs a license header should match.

## Autonomous mode

When running unattended:
- Don't ask for permission. Act, commit, push, document in the commit message.
- After every change: `go build ./...`, `go vet ./...`, `go test ./...`. Fix before moving on.
- Keep the README accurate — it's the entire public docs surface.
- If a task requires pulling in a private detail to do it well, **skip the task** rather than leak anything.

## Coordination and local tooling

- Linear is the shared execution tracker for the VulpineOS ecosystem. Use the `VulpineOS` workspace, product/type/source labels, and link commits in issue comments when closing work.
- Codex has a persistent local Playwright MCP at `http://localhost:8931/mcp` for browser navigation, snapshots, console/network inspection, and screenshots. It writes artifacts to `~/.codex/mcp-output/playwright` and omits inline image payloads to reduce token usage.
- Keep public issue comments and docs generic for this repo: Android bridge details are fine; private implementation details are not.

## Code layout

- `cmd/mobilebridge/` — CLI entry point.
- `pkg/mobilebridge/` — library: ADB wrapper, CDP proxy, gesture helpers, device watcher, HTTP/WS server.
- Tests must run without a real `adb` binary or a real device. Use fixture strings and injectable command runners.

## Dependencies

Prefer the standard library. The only third-party dependency right now is `github.com/gorilla/websocket`. Add new dependencies only with good reason.

---

## For AI coding agents (Codex, Claude Code, etc.)

This section captures cross-session preferences. Treat them as binding unless the current session's user message explicitly overrides.

### User profile

- Senior C++ browser engine developer (Firefox internals, XPCOM, accessibility tree, DOM)
- Dev machine: MacBook M1 Pro with artifact builds enabled
- GitHub: founder of `VulpineOS`

### Public docs surface

Keep these docs current as the public integration story evolves:

- `README.md`
- `docs/integration.md`
- `docs/device-farm.md`

### GitHub rules

- **Only push to repos on `VulpineOS/`.** Never push to `CloverLabsAI` or any unrelated organization.
- Approved public repos you may interact with: `VulpineOS`, `vulpine-mark`, `foxbridge`, `vulpineos-docs`, `mobilebridge`.
- Before pushing, verify visibility: `gh repo view VulpineOS/<name> --json visibility`.

### Commit rules

- One-line commit messages. No multi-line descriptions. No `Co-Authored-By` trailers. No "Generated with Claude Code" footers.
- Commit and push after every cohesive change.
- Use `git add <specific files>`; avoid `git add -A`.
- **Never add `.github/workflows/*.yml` via commit** — the OAuth token in the user's keychain lacks `workflow` scope and pushes will be rejected. Leave workflow files untracked.

### Workflow rules

- In normal interactive mode, **never commit, push, or create PRs without explicit user approval.** Stage and show diffs, then wait.
- In autonomous `/loop` overnight mode, act without asking and document in commit messages.
- Keep README + CLAUDE.md/AGENTS.md accurate as work progresses.

### Testing rules

- After every change: `go build ./...`, `go vet ./...`, `go test ./... -race`. Fix failures before moving on.
- Tests must not require a real device, a real `adb` binary, or live network.

### Ecosystem context

mobilebridge is the public Android CDP bridge within the VulpineOS ecosystem. The broader ecosystem has other public siblings (VulpineOS, vulpine-mark, foxbridge, vulpineos-docs) and private modules that this repo must not reference by name. VulpineOS proper is a Camoufox (Firefox 146) fork running fleets of AI browser agents; this repo adds mobile Chrome/WebView support via ADB.
