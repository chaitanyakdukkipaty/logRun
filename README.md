# LogRun

A CLI + Web UI system for wrapping shell commands with comprehensive logging and observability.

## Architecture

```
+-------------+        +----------------+        +-------------------+
| CLI Wrapper | -----> | Local Agent /  | -----> | Storage Layer     |
| (logrun)    |        | Ingest Service |        | (DB + Log Files)  |
+-------------+        +----------------+        +-------------------+
        |                          |
        |                          v
        |                 +----------------+
        |                 | Web API        |
        |                 +----------------+
        |                          |
        |                          v
        |                 +----------------+
        |                 | Web UI         |
        |                 +----------------+
```

## Components

- **CLI (`cmd/logrun/`)**: Go-based command wrapper
- **API (`api/`)**: Node.js backend API
- **Web UI (`web/`)**: React frontend
- **Storage**: SQLite + JSONL log files

## Quick Start

1. Build the CLI:
   ```bash
   cd cmd/logrun && go build -o ../../bin/logrun
   ```

2. Start the API:
   ```bash
   cd api && npm install && npm start
   ```

3. Start the Web UI:
   ```bash
   cd web && npm install && npm run dev
   ```

4. Use the CLI:
   ```bash
   ./bin/logrun npm run build
   ./bin/logrun --name "test-run" pytest tests/
   ```

## Features

- ✅ Wrap any shell command
- ✅ Capture stdout, stderr, exit codes
- ✅ Multiple concurrent processes
- ✅ Persistent logging (files + database)
- ✅ Web interface for log viewing
- ✅ Real-time log streaming
- ✅ Search and filtering
- ✅ Process metadata inspection

## Storage Strategy

- **Primary**: Append-only JSONL log files (`logs/{process_id}.log`)
- **Secondary**: SQLite for metadata and indexing
- **Benefits**: Fast writes, easy tailing, queryable metadata