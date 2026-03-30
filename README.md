# LogRun

A CLI + Web UI system for wrapping shell commands and Kubernetes pod logs with comprehensive logging, observability, and team sharing.

## Architecture

```
+------------------+       +----------------+       +-------------------+
| CLI (logrun)     | ----> | API (Node.js)  | ----> | Storage Layer     |
| shell / kubectl  |       | REST + SQLite  |       | (DB + JSONL logs) |
+------------------+       +----------------+       +-------------------+
        |                          |
        |                          v
        | (auto-start)    +----------------+
        +---------------> | Web UI (React) |
                          +----------------+
                                  |
                    (optional)    v
                         +------------------+
                         | zrok / ngrok     |
                         | Public Tunnels   |
                         +------------------+
```

## Components

| Component | Location | Description |
|-----------|----------|-------------|
| CLI | `cmd/logrun/` | Go binary — wraps commands, kubectl, tunnel sharing |
| API | `api/` | Node.js REST API + SQLite storage |
| Web UI | `web/` | React + Vite dashboard |

---

## Quick Start

### Install (macOS / Linux)

```bash
curl -sSL https://raw.githubusercontent.com/chaitanyakdukkipaty/logRun/main/install.sh | bash
```

Or [download the latest release](https://github.com/chaitanyakdukkipaty/logRun/releases/latest) for your platform.

### Build from source

```bash
# CLI only
cd cmd/logrun && go build -o ../../bin/logrun .

# Everything (CLI + API deps + Web UI)
./build.sh
```

### Run

The CLI automatically starts the API and web services on first use — no manual setup required.

```bash
logrun npm run build          # wrap any shell command
logrun pytest tests/          # capture test output
logrun kubectl                # stream Kubernetes pod logs
```

Then open **http://localhost:3000** to view your logs.

---

## Features

### Core — Wrap any command

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
| `--env KEY=VALUE` | Inject extra environment variables |
| `--follow` | Stream logs live to stdout (default: on) |
| `--detach` | Run in background |
| `--api-url` | Custom API URL (default: `http://localhost:3001`) |

### Service auto-start

When you run any `logrun` command the API and web services start automatically if they aren't already running. Port state is stored in `.logrun-services.json` so multiple CLI instances always find the right ports.

### Kubernetes pod log streaming

```bash
logrun kubectl                            # fully interactive
logrun kubectl -n prod -p my-app          # direct
logrun kubectl -n staging -p "worker-" --tail 200
logrun kubectl --all-namespaces -p api    # search all namespaces
```

- Interactive namespace and pod selection with type-to-filter search
- Pod `--pod` flag accepts **comma-separated patterns** (substring or regex)
- Add more patterns interactively via the confirmation menu
- All matching pods are streamed **in parallel**, each tracked separately
- `--follow` defaults to **on** — logs stream live until Ctrl+C
- Ctrl+C marks all pods as `cancelled` and cleans up gracefully

| Flag | Description |
|------|-------------|
| `-n, --namespace` | Kubernetes namespace (interactive if omitted) |
| `-p, --pod` | Pod name / substring / regex pattern (comma-separated) |
| `-c, --container` | Container name for multi-container pods |
| `-f, --follow` | Stream live (default: on; `--follow=false` for snapshot) |
| `--tail` | Lines from end (`--tail` alone = 100, `--tail N` = N, no flag = all) |
| `--since` | Only logs newer than a duration, e.g. `1h`, `30m` |
| `--all-namespaces` | Search pods across all namespaces |

### Team sharing via zrok / ngrok

Share your local LogRun dashboard with anyone on your team using a public tunnel.

**Requirements:** [zrok](https://zrok.io) (`zrok enable <token>`) or [ngrok](https://ngrok.com) must be installed.

```bash
# Standalone — block until Ctrl+C
logrun share

# Combined with any command
logrun --share npm run build

# Combined with kubectl
logrun kubectl --share -n prod -p my-app
```

On startup, tunnel URLs are printed before any prompts:

```
Starting tunnels…
  ✓ API tunnel: https://abc123.share.zrok.io
  ✓ Web tunnel: https://xyz456.share.zrok.io

Share this with your team:
  🌐 Dashboard: https://xyz456.share.zrok.io
  📡 API:       https://abc123.share.zrok.io
```

Teammates open the **Dashboard URL** in their browser — the Vite proxy routes all API calls transparently through the web tunnel to your local API.

Tunnels stay alive until you press **Ctrl+C**.

### Web UI

- **Process list** — all captured runs with status, tags, timestamps
- **Log viewer** — full stdout/stderr with search and filtering
- **Grep-style context** — `-B N` / `-A N` show N lines before/after each match
- **Real-time streaming** — live log tail while a process is running
- **Process metadata** — exit code, duration, environment, tags

---

## Storage

| Layer | Purpose |
|-------|---------|
| SQLite (`logrun.db`) | Process metadata, command info, indexing |
| JSONL (`logs/<id>.log`) | Append-only log lines — fast writes, easy tailing |

---

## Development

```bash
# API (port 3001)
cd api && npm install && npm start

# Web UI dev server (port 3000)
cd web && npm install && npm run dev

# CLI
cd cmd/logrun && go build -o ../../bin/logrun .
```

### Release

```bash
./build.sh v1.x.x true          # cross-compile all platforms → dist/
./upload-release.sh v1.x.x      # upload to GitHub Releases
```

Or push a `v*` tag to trigger the GitHub Actions release workflow automatically.

---

## Configuration

The API port and web port are discovered automatically and written to `.logrun-services.json` at the project root. All three components (CLI, API, Vite) read and update this file so every instance finds the correct ports regardless of what was available at startup.

```json
{
  "api_port": 3001,
  "web_port": 3000,
  "api_tunnel": "https://abc.share.zrok.io",
  "web_tunnel": "https://xyz.share.zrok.io",
  "updated_at": "2026-03-30T17:00:00Z"
}
```

Set `VITE_API_URL` to point the web dev server's proxy at a non-default API URL.
