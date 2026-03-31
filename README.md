# LogRun

A CLI + Web UI tool for wrapping shell commands and Kubernetes pod logs with comprehensive logging, real-time observability, and team sharing — all delivered as a **single self-contained binary**.

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                     logrun  (single binary)                     │
│                                                                 │
│  ┌──────────────┐   ┌──────────────────────────────────────┐   │
│  │  CLI layer   │   │   Embedded HTTP server (:4000)       │   │
│  │              │   │                                      │   │
│  │  logrun cmd  │──▶│  /api/*   REST API  (SQLite)         │   │
│  │  logrun      │   │  /api/*/stream  SSE  push            │   │
│  │    kubectl   │   │  /        React SPA  (embedded)      │   │
│  └──────────────┘   └──────────────────────────────────────┘   │
│         │                         │                             │
│         │ log queue (8192 cap)     │ SSE broadcasts             │
│         │ + overflow to disk       │ (process list + logs)      │
└─────────┼─────────────────────────┼─────────────────────────────┘
          │                         │
    ~/.logrun/                 browser tab
    ├── logrun.db   (SQLite)
    ├── logs/       (JSONL, one per process)
    ├── overflow/   (queue spill files)
    └── .services.json  (port + tunnel state)

                    (optional)
          ┌──────────────────────────┐
          │   zrok / ngrok tunnel    │
          │   public HTTPS URL       │
          └──────────────────────────┘
```

### Key design decisions

| Decision | Detail |
|----------|--------|
| **Single binary** | Go binary embeds the React build via `//go:embed`. No Node.js, no repo clone needed. |
| **One port** | Both the REST API and the React SPA are served from the same port (default 4000). |
| **Single server guarantee** | A file lock (`~/.logrun/.server.lock`) prevents duplicate servers. Multiple CLI instances share one. |
| **SSE for real-time updates** | Process list pushed via SSE on every DB mutation — zero polling. Log streaming also via SSE. |
| **Async log queue** | 8192-entry in-memory channel with JSONL overflow to `~/.logrun/overflow/`. Prevents data loss under log storms. |
| **Dynamic port** | Falls back to any free port if 4000 is taken. Port written to state file so all instances use the same one. |
| **Tunnel reuse** | Tunnel URL + PID persisted in `~/.logrun/.services.json`. Subsequent `--share` invocations reuse the running tunnel instead of spawning a new one. |

---

## Quick Start

### Install (macOS / Linux)

```bash
curl -sSL https://raw.githubusercontent.com/chaitanyakdukkipaty/logRun/main/install.sh | bash
```

Or [download the latest release](https://github.com/chaitanyakdukkipaty/logRun/releases/latest) for your platform and extract the binary to a directory on your `PATH`.

**Debug mode** (verbose output if install fails):
```bash
DEBUG=true curl -sSL https://raw.githubusercontent.com/chaitanyakdukkipaty/logRun/main/install.sh | bash
```

### Uninstall

```bash
curl -sSL https://raw.githubusercontent.com/chaitanyakdukkipaty/logRun/main/uninstall.sh | bash
```

**Keep your data** (remove binary only):
```bash
KEEP_DATA=true curl -sSL https://raw.githubusercontent.com/chaitanyakdukkipaty/logRun/main/uninstall.sh | bash
```

### Build from source

```bash
# Requires: Go 1.21+, Node.js 20+
./build.sh            # builds web UI then Go binary → bin/logrun
./build.sh v1.x.x true  # cross-compile all platforms → dist/
```

Push a `v*` tag to trigger the GitHub Actions release workflow automatically.

---

## Usage

The CLI auto-starts the embedded server on first use — no manual setup required. Open **http://localhost:4000** to view the dashboard.

### Wrap any command

```bash
logrun <command> [args]
logrun --name "nightly-build" --tags "ci,build" make all
logrun --cwd /path/to/project npm test
logrun --detach long-running-job   # run in background
```

| Flag | Description |
|------|-------------|
| `--name` | Friendly display name in the dashboard |
| `--tags` | Comma-separated tags for filtering |
| `--cwd` | Working directory for the command |
| `--follow` | Stream logs live to stdout (default: on) |
| `--detach` | Run in background |
| `--api-url` | Custom API URL (default: `http://localhost:4000`) |
| `--share` | Expose dashboard via zrok/ngrok tunnel |

### Kubernetes pod log streaming

```bash
logrun kubectl                            # fully interactive
logrun kubectl -n prod -p my-app          # direct
logrun kubectl -n staging -p "worker-" --tail 200
logrun kubectl --all-namespaces -p api    # search all namespaces
logrun kubectl --share -n prod -p my-app  # share dashboard while streaming
```

- Interactive namespace → pod selection with type-to-filter search
- `--pod` accepts **comma-separated patterns** (substring or regex) — all matching pods streamed in parallel
- Add more pods interactively via the confirmation menu before fetching begins
- `--follow` defaults to **on** — logs stream live until Ctrl+C
- Ctrl+C marks all pods as `cancelled` and cleans up gracefully

| Flag | Description |
|------|-------------|
| `-n, --namespace` | Kubernetes namespace (interactive if omitted) |
| `-p, --pod` | Pod name / substring / regex (comma-separated; interactive if omitted) |
| `-c, --container` | Container name for multi-container pods |
| `-f, --follow` | Stream live (default: on; `--follow=false` for snapshot) |
| `--tail` | Lines from end (`--tail` alone = 100, `--tail N` = N, omitted = all) |
| `--since` | Only logs newer than a duration, e.g. `1h`, `30m` |
| `--all-namespaces` | Search pods across all namespaces |

### Team sharing via zrok / ngrok

**Requirements:** [zrok](https://zrok.io) (`zrok enable <token>`) or [ngrok](https://ngrok.com) must be installed.

```bash
logrun share                          # standalone — block until Ctrl+C
logrun --share npm run build          # combined with any command
logrun kubectl --share -n prod -p app # combined with kubectl
```

On startup, the tunnel URL is printed and persisted:

```
Starting tunnel…
  ✓ Tunnel: https://abc123.share.zrok.io

Share this with your team:
  🌐 Dashboard + API: https://abc123.share.zrok.io
```

- **Single URL** serves both the dashboard and API (same port)
- Tunnel URL is stored in `~/.logrun/.services.json`; re-running `--share` while the tunnel is alive **reuses the existing URL** instead of spawning a new one
- Tunnels stay alive until Ctrl+C; logs streamed before sharing are accessible immediately

---

## Web UI Features

| Feature | Details |
|---------|---------|
| **Process list** | Real-time SSE push — updates instantly on any create/update/delete, zero polling |
| **Log viewer** | Full stdout/stderr with search, stream filter (stdout/stderr), time range filter |
| **Grep-style context** | `-B N` / `-A N` show N lines before/after each search match |
| **Live log streaming** | SSE tail while a process is running — new lines appear without page refresh |
| **Process metadata** | Status, exit code, duration, tags, CWD, all commands |
| **Multi-command view** | Kubernetes processes show each pod as a separate command with individual status |

---

## Storage

All data is stored under `~/.logrun/`:

| Path | Purpose |
|------|---------|
| `~/.logrun/logrun.db` | SQLite — process metadata, command info, status |
| `~/.logrun/logs/<id>.log` | JSONL — append-only log lines per process |
| `~/.logrun/overflow/*.jsonl` | Queue spill files when in-memory queue is full |
| `~/.logrun/.services.json` | Port, PID, tunnel URL — shared state across CLI instances |
| `~/.logrun/.server.lock` | File lock — ensures only one embedded server runs |

---

## Development

```bash
# Web UI (hot reload on :5173, proxies API to :4000)
cd web && npm install && npm run dev

# Go CLI (rebuild after changes)
cd cmd/logrun && go build -o ../../bin/logrun .

# Full build (web + Go, production binary)
./build.sh dev
```

The Vite dev server proxies `/api/*` to `http://localhost:4000` so you can develop the UI while the Go server handles data.
