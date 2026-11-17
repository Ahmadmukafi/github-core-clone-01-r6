# github-core-clone

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/Ahmadmukafi/github-core-clone-01-r6/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/Ahmadmukafi/github-core-clone-01-r6/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone GitHub-compatible git forge: **one process, no UI, no git binary, own store**. Built for the [make.fun](https://make.fun/competitions/github-core-clone-01) bounty `github-core-clone-01` — round 6, a full rewrite of the round-2 Node forge in Go: same wire behaviour byte-for-byte, but a raw HTTP/1.1 server, hand-written JSON with JavaScript semantics, and a from-scratch git object layer, so the p50 of every workload drops instead of just the Node hot spots.

Join make.fun with my referral: https://make.fun/r/6z0bn4hz (code `6z0bn4hz`).

## What it serves

- **Git smart HTTP** — upload-pack v2 (`ls-refs`, `fetch` with depth/shallow, filters) and receive-pack v0 (`report-status`, side-band-64k, non-fast-forward rejection), packfiles v2/v3 with OFS/REF delta resolution, SHA-1 object format.
- **REST v3** under `/api/v3` — repos, issues, pull requests (three-way merge), labels, comments, branches, commits, contents, git refs/tags; bearer <redacted> via `MAKEFUN_SHOP_TOKEN`; every reply carries `X-GitHub-Request-Id`.
- **GraphQL v4** at `/api/graphql` — viewer, repository, node, `__type` introspection, Relay global ids, cursor pagination, batching, and the mutations `createIssue` / `closeIssue` / `createPullRequest` / `mergePullRequest` / `addComment`.
- **MCP** at `/mcp` — JSON-RPC 2.0, `server/discover` (protocol 2026-07-28) plus the legacy `initialize` fallback, and six tools: `create_repo`, `get_repo`, `create_issue`, `list_issues`, `create_pull_request`, `merge_pull_request`.

## Run it

```bash
node --experimental-strip-types main.ts
```

Node 22+, standard library only, no dependencies. `main.ts` launches the committed static Go binary (`forge`, Linux amd64) — directly when the executable bit survived the upload, otherwise from a temp copy. The entry binds an ephemeral port, `POST`s `{"port": n}` to `$MAKEFUN_ENV_URL/register`, then polls `GET $MAKEFUN_ENV_URL/done` and exits 0 when the suite is over. Without `MAKEFUN_ENV_URL` it serves standalone on `$PORT` (or the ephemeral port) and prints the URL.

```bash
MAKEFUN_SHOP_TOKEN=<redacted>
```

## Layout

```
main.ts            entrypoint: launches the Go binary (direct or temp copy)
forge              statically linked Go binary (linux/amd64, CGO_ENABLED=0)
*.go               the forge, written from scratch in Go:
  main.go          lifecycle: bind, register, poll /done
  http.go          raw HTTP/1.1 server + router
  json.go          ordered JSON with JavaScript stringify semantics
  util.go          deterministic clock, SHA-1, base64, Relay ids
  objects.go       git objects, packs, delta resolution, fetch planning
  pkt.go           pkt-line framing
  store.go         in-memory store: repos, issues, PRs, labels, contents
  git.go           smart HTTP: advertisement, upload-pack v2, receive-pack v0
  rest.go          REST v3 routes
  graphql.go       GraphQL parser + executor
  mcp.go           MCP JSON-RPC server and tools
```

## Speed

Round 6 keeps the round-2 API surface byte-for-byte and changes the engine underneath it:

- raw TCP HTTP/1.1 serving with a hand-rolled parser (no framework, no chunked responses, explicit Content-Length), keep-alive connections, `Expect: 100-continue` answered before the body is read;
- zero-allocation hot paths where it matters: cached serialized REST JSON for repos and issues, cached ref advertisements and `ls-refs` per repo version, pack cache keyed by `(wants, haves, depth, shallow)`;
- concurrent readers: git fetch/clone work runs under a shared read lock, so the 50-way concurrent clone workload actually runs in parallel;
- deterministic, allocation-light parsers (no `Date.now()`/`Math.random()` in stored data — the clock ticks from the fixed scenario epoch).

## License

MIT — see [LICENSE](LICENSE).
