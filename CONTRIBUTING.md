# Contributing to Kikubot

Thanks for considering a contribution. Kikubot powers production deployments at mxHERO, and the open-source release exists so the community can build on the same framework we use ourselves. Issues, pull requests, and discussions are all welcome.

This document explains how we work, what we're most likely to merge, and where help is most useful.

## How we work

Kikubot's source of truth is our internal repository; the public GitHub repo is updated by periodic pushes. That means:

- **We review issues and PRs in batches**, typically every 1–2 weeks. If something is urgent for you and your timeline can't wait, fork freely — MIT covers it.
- **Direct commits to `main` happen from internal work.** External contributions land via PR.
- **We can't always merge everything**, even good work. Sometimes a PR conflicts with an internal direction we haven't shared publicly yet. When that happens, we'll say so and explain why rather than letting the PR rot.

## Where help is most useful

Ordered roughly by impact:

1. **Microsoft 365 / Exchange compatibility.** Our internal deployments run on docker-mailserver and standard IMAP/SMTP. Office 365 has quirks we haven't fully ironed out — IMAP behavior, threading headers, OAuth flows. Real-world testing reports and fixes here are very welcome.
2. **New tools.** The `internal/tools/registry.go` pattern makes it easy to add an integration — see "Writing your own tool" in the [README](README.md). We're especially interested in: GitHub, Linear, Jira, Notion, Discord, Telegram, calendar tools, and additional MCP bridges.
3. **Documentation and examples.** Real deployment write-ups, sample `agents.yaml` rosters for common use cases (customer support team, content pipeline, devops on-call), and knowledge-base examples.
4. **Tests.** Coverage is uneven. Tests for the snooze scheduler, thread-history serialization, and the MCP bridges are especially helpful.
5. **The configurator** (`scripts/configurator/`). Works, but rough. Improvements to UX, validation, and docker-compose generation are appreciated.
6. **Bug fixes** with a clear reproduction.

If you want to work on something larger — a new core abstraction, a change to the agent loop, a new LLM provider — open an issue first so we can sanity-check direction before you sink time into it.

## What we're most likely to merge

- Self-contained changes: one tool, one bug fix, or one doc improvement per PR.
- Code that follows existing patterns. New tools should use `LocalMCPBridge`, `MCPBridge`, or `CLIToolConfig` where applicable rather than rolling new subprocess plumbing.
- Changes with a clear "why" in the PR description — what problem does this solve, what did you try, what's the smallest change that fixes it.
- Anything labeled [`good first issue`](https://github.com/mxaiorg/kikubot/labels/good%20first%20issue) is fair game without prior discussion.

## What's less likely to merge without discussion first

- Large refactors or new abstractions layered on existing ones.
- New top-level dependencies — we keep the dependency tree intentionally small.
- Speculative features without a concrete use case.
- Changes that significantly alter the agent loop, message handling, or memory format.

A two-line issue ("I'd like to add X, here's why") is enough to save both of us from a wasted PR.

## How to submit a PR

1. Fork the repo and create a branch off `main`.
2. Make your change. If it's a new tool, register it in `internal/tools/registry.go` and add a row to the built-in catalogue in the README.
3. Run `go vet ./...` and `go test ./...`. Build the Docker image locally if your change touches the runtime.
4. Sign your commits (DCO — see below).
5. Open a PR with a clear description. Reference any related issue.

## Developer Certificate of Origin (DCO)

We use the [Developer Certificate of Origin](https://developercertificate.org/) instead of a CLA. Every commit must be signed off with:

```
Signed-off-by: Your Name <your-email@example.com>
```

Add this automatically with `git commit -s`. Configure once:

```bash
git config --global user.name "Your Name"
git config --global user.email "your-email@example.com"
```

The sign-off certifies you wrote the patch (or have the right to submit it) and that you're contributing it under the project's MIT license.

## License

By contributing, you agree your contributions will be licensed under the [MIT License](LICENSE) that covers the project.

## Code of conduct

Be decent to each other. We follow the spirit of the [Contributor Covenant](https://www.contributor-covenant.org/) — assume good faith, focus on the work, and disagree without being personal. If something needs escalation, email opensource@mxhero.com.

## Questions

- **Usage questions** — open a [GitHub Discussion](https://github.com/mxaiorg/kikubot/discussions).
- **Bugs and feature requests** — open an [Issue](https://github.com/mxaiorg/kikubot/issues).
- **Security or partnership inquiries** — email opensource@mxhero.com.

Thanks for being here.
