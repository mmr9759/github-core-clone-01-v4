[![github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/c/github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/mmr9759/github-core-clone-01-v4/rank.svg)](https://make.fun/c/github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/mmr9759/github-core-clone-01-v4/reward.svg)](https://make.fun/c/github-core-clone-01)

# GitForge v4 — static Go core

A GitHub-compatible git forge in one static binary, launched by a tiny
Python entrypoint. Built from scratch for github-core-clone-01.

Entry for [github-core-clone-01](https://make.fun/bounty/github-core-clone-01)
on [make.fun](https://make.fun/bounty/github-core-clone-01).

## Design

- **Static Go core** (`forge`, CGO_ENABLED=0, stripped): raw TCP HTTP/1.1
  without net/http, zero-alloc hot paths, cached clone packs, cached GET
  issue, deterministic clock.
- **Entrypoint** `main.py`: execs the `forge` binary beside it.
- **REST v3** (repos, branches, commits, git/refs, contents, labels,
  issues + comments, pulls + merge squash + conflict detection),
  **git smart HTTP v2/v0** (ls-refs, fetch incl. shallow/deepen/unshallow,
  receive-pack with OFS/REF delta resolution), **GraphQL**
  (own document parser, introspection, batch, cursor pagination,
  createIssue/closeIssue/createPullRequest/mergePullRequest/addComment),
  **MCP** (`server/discover` 2026-07-28 + legacy `initialize`, 6 tools).

## Local verification

Local conformance kit (`sdk/run.ts --perf`, 2 fresh runs): **120/120
checks, feature 600, agent 200, perf 173–174, score 973–974** (v3 server:
944, perf 144). Also tested manually with real git CLI: push, full clone,
`--depth 1` clone, pull, `--deepen=1` fetch — all pass, file contents
byte-identical.
