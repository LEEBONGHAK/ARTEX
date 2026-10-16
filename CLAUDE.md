# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

ARTEX is an AI autonomous penetration-testing system: a Go backend plus a Next.js
frontend. Code comments and user-facing strings are predominantly Chinese; keep that
convention when editing nearby code.

## Commands

Toolchain: **Go 1.26+**, **Node 22 / npm**, **PostgreSQL** (required at runtime; an
LLM provider via `ANTHROPIC_API_KEY`/`OPENAI_API_KEY` or the UI is required for exploration).

Development (run from repo root):
- `./dev.sh` — runs backend (`:8787`) + traffic proxy (`:8788`) via `go run` and `next dev` (`:5173`) together; the dev frontend reverse-proxies `/api/*` to the backend.
- `go test ./...` — all Go tests. Single package: `go test ./agent`. Single test: `go test ./agent -run TestName`.
- `cd web && npm run lint` — Biome lint. `npm run check:fix` auto-fixes lint+format. `npm run format` formats only. (A husky pre-commit hook runs `biome check --write` on staged JS/TS.)
- The frontend has no JS test suite.

Build (single self-contained binary — frontend must be exported and copied first; the `embedui` tag is what actually embeds it):
```bash
cd web && npm ci && npm run build:static && cd ..   # → web/out
cp -r web/out server/webui/dist
CGO_ENABLED=0 go build -tags embedui -o artex ./cmd/artex
```
Without `-tags embedui` the binary runs with no bundled UI (expects a separate dev frontend). `./build.sh` wraps the full pipeline (frontend export → cross-compile → zip); `./build.sh --release` builds all five platform targets. `start.sh`/`start.bat` are the real entrypoints (supervisor scripts) — see Self-update below; do **not** launch `./artex` directly in production.

Run flags (passed through by `start.sh`): `-addr :8787` (HTTP+API+SSE, same origin), `-proxy 127.0.0.1:8788` (traffic-recording MITM proxy, empty to disable), `-data <dir>` (SQLite stores for jwt.key etc.).

## Architecture

**Agent framework.** The agent layer is built on `github.com/Autumn-27/norma` (external dep): `agentcore`, `llm`, `harness`, `tool`, `permission`, `skill`, `mcp`, `transcript`, `compaction`. Understand these imports before changing agent behavior — most orchestration primitives live there, not in this repo.

**Event-driven exploration loop** (`server/engine.go`, `agent/`). Each task runs three kinds of LLM agents:
- **Planner** (`agent/planner.go`) — the *sole* intent generator. On a debounced asset/exploration-graph change it reads the exploration route, queries assets, judges whether the goal is met, and emits 0..N exploration intents into the frontier.
- **Worker** (`agent/worker.go`) — N run concurrently; each claims **one** intent, executes it with real tools (Bash/kali tooling routed through the recording proxy), writes discovered facts back into the graph, then stops. Workers never generate new directions.
- **MainAgent** (`agent/mainagent.go`) — the human chat interface; observes via read tools and steers the planner/workers (hints or direct high-priority intents). It does not run the autonomous loop.

There is no simulation mode — a real LLM provider is required; the loops always run but idle until one is configured. The `Engine` holds extensive per-task runtime state (pause/cancel/delete barriers, per-work steer boxes, deadline coordinator). Planner/worker pairs are resolved per task by the server acting as authoritative task-router (`resolve` in `Engine`), supporting per-task LLM profiles and failover chains.

Code comments reference design-doc sections (e.g. "docs §4.3", "docs/任务级超时与收尾设计.md") that are **not** checked into `docs/`; treat them as historical pointers, not files you can read.

**Data layer** (`db/`, PostgreSQL via `jackc/pgx` using the `database/sql` `pgx` driver). `db/schema.sql` is embedded and **re-applied idempotently on every startup** under an advisory lock ("restart = migrate"); write migrations as additive `ADD COLUMN` / `CREATE INDEX IF NOT EXISTS` in that file rather than separate migration scripts. DSN comes from `config.json` or `ARTEX_PG_DSN`. Core domain tables: `companies`/`assets`/`company_scope`, `explorations`/`exploration_nodes`/`exploration_edges`/`exploration_anchors` (the exploration graph), `tasks`, `activity` (live trace), `findings`, `intercept_rules`/`intercept_pending`, `llm_profiles`, `agents`/`agent_prompts`. A small local SQLite/`data/` dir holds jwt.key and similar.

**Tool interception / human-in-the-loop** (`intercept/`). A `PreToolUse` approval layer evaluates DB-backed rules in priority order: `allow` / `deny` / `ask`. `ask` creates an `intercept_pending` row, writes an activity, and blocks the tool call until the user decides via `/api/intercept/pending/{id}/decide`.

**Traffic** (`traffic/`) — MITM recording proxy (`go-mitmproxy`) capturing agent HTTP(S) traffic; evidence snapshots bind to findings (`evidence/`, `db/finding_traffic*`). **notify** (`notify/`) — multi-channel notifications (email, dingtalk, feishu, telegram, wecom, generic webhook) with SSRF guards and secret masking. **selfupdate** (`selfupdate/`) — staged binary swap driven by process exit code: `main()` returns `selfupdate.ExitRestart` (75) to signal `start.sh`/`start.bat` to relaunch and install a staged `artex.new`; auto-rollback to `artex.old` after repeated boot failures. This exit-code protocol is why the supervisor script is the entrypoint. **skills/** — on-disk agent skills (SKILL.md + scripts; e.g. `api-recon`, `playwright-cli`, `scopesentry`), bind-mounted in Docker so they persist/update independently of the binary.

**Frontend** (`web/`, Next.js 16 / React 19 / Tailwind 4 / shadcn+radix, Zustand stores, Biome for lint/format). Built as a **static export** (`NEXT_EXPORT=1`, `output: "export"`) copied into `server/webui/dist` (generated — not in git) and served by the Go backend from the same origin as the API and SSE activity stream. `web/src/proxy.ts` is the auth-gate middleware (cookie `artex_token`; redirects to `/login` or `/setup`). Real-time updates use SSE (same-origin; behind a reverse proxy, buffering must be disabled). See [`web/CLAUDE.md`](web/CLAUDE.md) for frontend-specific conventions (API client, SSE, mock mode, Biome rules).
