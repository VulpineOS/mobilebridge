# mobilebridge — repo rules for Claude

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

## Public docs surface

Keep these docs current as the public integration story evolves:

- `README.md`
- `docs/integration.md`
- `docs/device-farm.md`
