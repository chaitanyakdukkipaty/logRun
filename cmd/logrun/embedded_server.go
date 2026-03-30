package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

var (
	embeddedDB      *sql.DB
	embeddedLogsDir string
	sseClients      sync.Map // map[float64]*sseClient  — per-process log streams
	sseClientsMu    sync.Mutex

	// processListSSE broadcasts full process-list snapshots to subscribed browsers.
	processListSSE   sync.Map // map[float64]chan string
	processListSSEMu sync.Mutex
)

type sseClient struct {
	processID string
	ch        chan string
	done      chan struct{}
}

const embeddedSchema = `
CREATE TABLE IF NOT EXISTS processes (
  process_id TEXT PRIMARY KEY,
  name TEXT,
  commands TEXT NOT NULL,
  status TEXT NOT NULL,
  start_time DATETIME NOT NULL,
  end_time DATETIME,
  tags TEXT,
  cwd TEXT,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_processes_status ON processes(status);
CREATE INDEX IF NOT EXISTS idx_processes_start_time ON processes(start_time);
CREATE INDEX IF NOT EXISTS idx_processes_name ON processes(name);
`

// StartEmbeddedServer initialises the DB, creates the HTTP mux, and starts
// listening on the given port. It returns immediately (runs in a goroutine).
func StartEmbeddedServer(port int) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("could not determine home dir: %w", err)
	}

	dataDir := filepath.Join(home, ".logrun")
	embeddedLogsDir = filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(embeddedLogsDir, 0755); err != nil {
		return fmt.Errorf("could not create logs dir: %w", err)
	}

	dbPath := filepath.Join(dataDir, "logrun.db")
	embeddedDB, err = sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("could not open DB: %w", err)
	}

	if _, err := embeddedDB.Exec(embeddedSchema); err != nil {
		return fmt.Errorf("could not initialise schema: %w", err)
	}

	// Start the log queue workers (in-memory queue + overflow to disk).
	initLogQueue(dataDir)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", withCORS(handleHealth))
	mux.HandleFunc("/health/queue", withCORS(handleHealthQueue))
	mux.HandleFunc("/api/processes/stream", withCORS(handleProcessListSSE))
	mux.HandleFunc("/api/processes", withCORS(handleProcesses))
	mux.HandleFunc("/api/processes/", withCORS(handleProcessesSubroute))
	mux.Handle("/", webFileServer())

	go func() {
		addr := fmt.Sprintf(":%d", port)
		if err := http.ListenAndServe(addr, mux); err != nil {
			fmt.Fprintf(os.Stderr, "embedded server error: %v\n", err)
		}
	}()

	// Background goroutine: periodically clean up stale running processes.
	// Replaces the frontend /api/health polling loop.
	go func() {
		cleanupStaleRunningProcesses()
		for range time.Tick(30 * time.Second) {
			cleanupStaleRunningProcesses()
		}
	}()

	return nil
}

// withCORS wraps a handler to add CORS headers and handle OPTIONS preflight.
func withCORS(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Cache-Control")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ── /health ───────────────────────────────────────────────────────────────────

func handleHealth(w http.ResponseWriter, r *http.Request) {
	cleaned, running := cleanupStaleRunningProcesses()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":             "OK",
		"timestamp":          time.Now().UTC().Format(time.RFC3339),
		"cleanedUpProcesses": cleaned,
		"runningProcesses":   running,
	})
}

// cleanupStaleRunningProcesses marks commands as 'cancelled' when their OS
// process is no longer alive. Returns (cleaned, stillRunning) counts.
func cleanupStaleRunningProcesses() (int, int) {
	if embeddedDB == nil {
		return 0, 0
	}
	rows, err := embeddedDB.Query(`SELECT process_id, commands FROM processes WHERE status = 'running'`)
	if err != nil {
		return 0, 0
	}
	defer rows.Close()

	type prow struct {
		id       string
		commands string
	}
	var procs []prow
	for rows.Next() {
		var p prow
		if err := rows.Scan(&p.id, &p.commands); err == nil {
			procs = append(procs, p)
		}
	}

	cleaned := 0
	for _, p := range procs {
		var cmds []map[string]interface{}
		if err := json.Unmarshal([]byte(p.commands), &cmds); err != nil {
			continue
		}
		changed := false
		for i, cmd := range cmds {
			if cmd["status"] != "running" {
				continue
			}
			pidVal, ok := cmd["pid"]
			if !ok {
				continue
			}
			pid := 0
			switch v := pidVal.(type) {
			case float64:
				pid = int(v)
			case int:
				pid = v
			}
			if pid <= 0 {
				continue
			}
			proc, err := os.FindProcess(pid)
			if err != nil {
				cmds[i]["status"] = "cancelled"
				changed = true
				continue
			}
			if err := proc.Signal(syscall.Signal(0)); err != nil {
				cmds[i]["status"] = "cancelled"
				changed = true
			}
		}
		if changed {
			newCmds, _ := json.Marshal(cmds)
			_, _ = embeddedDB.Exec(
				`UPDATE processes SET commands = ?, status = 'cancelled' WHERE process_id = ?`,
				string(newCmds), p.id,
			)
			cleaned++
		}
	}

	running := len(procs) - cleaned
	if running < 0 {
		running = 0
	}
	if cleaned > 0 {
		go broadcastProcessList()
	}
	return cleaned, running
}

func handleHealthQueue(w http.ResponseWriter, r *http.Request) {
	stats := getQueueStats()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":        "OK",
		"depth":         stats.Depth,
		"capacity":      stats.Capacity,
		"processed":     stats.Processed,
		"overflowed":    stats.Overflowed,
		"dropped":       stats.Dropped,
		"overflow_files": stats.OverflowFiles,
	})
}

// ── /api/processes ────────────────────────────────────────────────────────────

func handleProcesses(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		handleListProcesses(w, r)
	case http.MethodPost:
		handleCreateProcess(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleCreateProcess(w http.ResponseWriter, r *http.Request) {
	var body map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	processID, _ := body["process_id"].(string)
	name, _ := body["name"].(string)
	status, _ := body["status"].(string)
	startTime, _ := body["start_time"].(string)
	endTime, _ := body["end_time"].(string)
	cwdVal, _ := body["cwd"].(string)

	if status == "" {
		status = "running"
	}
	if startTime == "" {
		startTime = time.Now().UTC().Format(time.RFC3339)
	}

	commandsJSON := marshalField(body["commands"], "[]")
	tagsJSON := marshalField(body["tags"], "[]")

	consolidated := false

	// If name is set, check for a running process with that name to consolidate.
	if name != "" {
		var existingID, existingCmds string
		err := embeddedDB.QueryRow(
			`SELECT process_id, commands FROM processes WHERE name = ? AND status = 'running' LIMIT 1`,
			name,
		).Scan(&existingID, &existingCmds)
		if err == nil {
			// Append new commands to the existing process.
			var existing []interface{}
			var incoming []interface{}
			_ = json.Unmarshal([]byte(existingCmds), &existing)
			_ = json.Unmarshal([]byte(commandsJSON), &incoming)
			merged, _ := json.Marshal(append(existing, incoming...))
			_, _ = embeddedDB.Exec(
				`UPDATE processes SET commands = ? WHERE process_id = ?`,
				string(merged), existingID,
			)
			processID = existingID
			consolidated = true
			go broadcastProcessList()
			writeJSON(w, http.StatusCreated, map[string]interface{}{
				"message":      "Process created",
				"process_id":   processID,
				"consolidated": consolidated,
			})
			return
		}
	}

	var endVal interface{}
	if endTime != "" {
		endVal = endTime
	}

	_, err := embeddedDB.Exec(
		`INSERT INTO processes (process_id, name, commands, status, start_time, end_time, tags, cwd)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		processID, name, commandsJSON, status, startTime, endVal, tagsJSON, cwdVal,
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	go broadcastProcessList()
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"message":      "Process created",
		"process_id":   processID,
		"consolidated": consolidated,
	})
}

func handleListProcesses(w http.ResponseWriter, r *http.Request) {
	rows, err := embeddedDB.Query(`
		SELECT process_id, name, commands, status, start_time, end_time, tags, cwd
		FROM processes
		ORDER BY CASE WHEN status='running' THEN 0 ELSE 1 END, start_time DESC
	`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	result := []map[string]interface{}{}
	for rows.Next() {
		p := scanProcess(rows)
		if p != nil {
			result = append(result, p)
		}
	}
	writeJSON(w, http.StatusOK, result)
}

// ── /api/processes/* subrouter ────────────────────────────────────────────────

func handleProcessesSubroute(w http.ResponseWriter, r *http.Request) {
	// Strip "/api/processes/" prefix and split remaining path.
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/processes/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")

	// /api/processes/by-name/{name}
	if len(parts) >= 2 && parts[0] == "by-name" {
		name := strings.Join(parts[1:], "/")
		handleByName(w, r, name)
		return
	}

	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	id := parts[0]

	switch len(parts) {
	case 1:
		// /api/processes/{id}
		switch r.Method {
		case http.MethodGet:
			handleGetProcess(w, r, id)
		case http.MethodPut:
			handleUpdateProcess(w, r, id)
		case http.MethodDelete:
			handleDeleteProcess(w, r, id)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}

	case 2:
		switch parts[1] {
		case "logs":
			// /api/processes/{id}/logs
			switch r.Method {
			case http.MethodGet:
				handleGetLogs(w, r, id)
			case http.MethodPost:
				handlePostLog(w, r, id)
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
		case "health":
			// /api/processes/{id}/health
			handleProcessHealth(w, r, id)
		default:
			http.NotFound(w, r)
		}

	case 3:
		switch {
		case parts[1] == "logs" && parts[2] == "batch":
			// /api/processes/{id}/logs/batch
			if r.Method == http.MethodPost {
				handlePostLogBatch(w, r, id)
			} else {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
		case parts[1] == "logs" && parts[2] == "stream":
			// /api/processes/{id}/logs/stream
			if r.Method == http.MethodGet {
				handleSSEStream(w, r, id)
			} else {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
		case parts[1] == "commands":
			// /api/processes/{id}/commands/{cmdId}
			if r.Method == http.MethodPut {
				handleUpdateCommand(w, r, id, parts[2])
			} else {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
		default:
			http.NotFound(w, r)
		}

	default:
		http.NotFound(w, r)
	}
}

// ── Individual route handlers ─────────────────────────────────────────────────

func handleByName(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	row := embeddedDB.QueryRow(`
		SELECT process_id, name, commands, status, start_time, end_time, tags, cwd
		FROM processes WHERE name = ? AND status = 'running' LIMIT 1`, name)
	p := scanProcessRow(row)
	if p == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func handleGetProcess(w http.ResponseWriter, r *http.Request, id string) {
	row := embeddedDB.QueryRow(`
		SELECT process_id, name, commands, status, start_time, end_time, tags, cwd
		FROM processes WHERE process_id = ?`, id)
	p := scanProcessRow(row)
	if p == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func handleUpdateProcess(w http.ResponseWriter, r *http.Request, id string) {
	var body map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	commandsJSON := marshalFieldOpt(body["commands"])
	tagsJSON := marshalFieldOpt(body["tags"])

	res, err := embeddedDB.Exec(`
		UPDATE processes SET
			name      = COALESCE(?, name),
			commands  = COALESCE(?, commands),
			status    = COALESCE(?, status),
			start_time= COALESCE(?, start_time),
			end_time  = COALESCE(?, end_time),
			tags      = COALESCE(?, tags),
			cwd       = COALESCE(?, cwd)
		WHERE process_id = ?`,
		nilStr(body["name"]),
		commandsJSON,
		nilStr(body["status"]),
		nilStr(body["start_time"]),
		nilStr(body["end_time"]),
		tagsJSON,
		nilStr(body["cwd"]),
		id,
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	go broadcastProcessList()
	writeJSON(w, http.StatusOK, map[string]string{"message": "Process updated"})
}

func handleDeleteProcess(w http.ResponseWriter, r *http.Request, id string) {
	res, err := embeddedDB.Exec(`DELETE FROM processes WHERE process_id = ?`, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	n, _ := res.RowsAffected()
	logPath := filepath.Join(embeddedLogsDir, id+".log")
	_ = os.Remove(logPath)
	go broadcastProcessList()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message":      "Process deleted",
		"deleted_rows": n,
	})
}

func handleUpdateCommand(w http.ResponseWriter, r *http.Request, processID, cmdID string) {
	var body map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	var cmdsJSON string
	err := embeddedDB.QueryRow(`SELECT commands FROM processes WHERE process_id = ?`, processID).Scan(&cmdsJSON)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	} else if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	var cmds []map[string]interface{}
	if err := json.Unmarshal([]byte(cmdsJSON), &cmds); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "bad commands JSON"})
		return
	}

	found := false
	for i, cmd := range cmds {
		if cmd["command_id"] == cmdID {
			if v, ok := body["status"]; ok {
				cmds[i]["status"] = v
			}
			if v, ok := body["exit_code"]; ok {
				cmds[i]["exit_code"] = v
			}
			if v, ok := body["end_time"]; ok {
				cmds[i]["end_time"] = v
			}
			found = true
			break
		}
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "command not found"})
		return
	}

	// Recalculate process status from all commands.
	newStatus := "completed"
	for _, cmd := range cmds {
		s, _ := cmd["status"].(string)
		if s == "running" {
			newStatus = "running"
			break
		}
		if s == "failed" {
			newStatus = "failed"
		}
	}

	newCmds, _ := json.Marshal(cmds)
	_, err = embeddedDB.Exec(
		`UPDATE processes SET commands = ?, status = ? WHERE process_id = ?`,
		string(newCmds), newStatus, processID,
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	go broadcastProcessList()
	writeJSON(w, http.StatusOK, map[string]string{"message": "Command updated", "status": newStatus})
}

// ── Log handlers ──────────────────────────────────────────────────────────────

func handleGetLogs(w http.ResponseWriter, r *http.Request, id string) {
	q := r.URL.Query()
	search := q.Get("q")
	streamFilter := q.Get("stream")
	from := q.Get("from")
	to := q.Get("to")
	limit := 10000
	offset := 0
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	logPath := filepath.Join(embeddedLogsDir, id+".log")
	logs, err := readLogLines(logPath, search, streamFilter, from, to, limit+1, offset)
	if err != nil && !os.IsNotExist(err) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	hasMore := false
	if len(logs) > limit {
		hasMore = true
		logs = logs[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"logs":     logs,
		"total":    len(logs),
		"has_more": hasMore,
	})
}

func handlePostLog(w http.ResponseWriter, r *http.Request, id string) {
	var entry map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&entry); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if entry["timestamp"] == nil {
		entry["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	}
	entry["process_id"] = id

	enqueueLog(id, entry)
	writeJSON(w, http.StatusCreated, map[string]string{"message": "Log entry queued"})
}

func handlePostLogBatch(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Logs []map[string]interface{} `json:"logs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	accepted, rejected := 0, 0
	for _, entry := range body.Logs {
		if entry["message"] == nil && entry["timestamp"] == nil {
			rejected++
			continue
		}
		if entry["timestamp"] == nil {
			entry["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
		}
		entry["process_id"] = id
		enqueueLog(id, entry)
		accepted++
	}

	stats := getQueueStats()
	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"message":       "Logs queued",
		"accepted":      accepted,
		"rejected":      rejected,
		"queueDepth":    stats.Depth,
		"overflowFiles": stats.OverflowFiles,
	})
}

func handleProcessHealth(w http.ResponseWriter, r *http.Request, id string) {
	var cmdsJSON string
	err := embeddedDB.QueryRow(
		`SELECT commands FROM processes WHERE process_id = ? AND status = 'running'`, id,
	).Scan(&cmdsJSON)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"process_id": id,
			"status":     "not_running",
		})
		return
	} else if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	var cmds []map[string]interface{}
	_ = json.Unmarshal([]byte(cmdsJSON), &cmds)

	changed := false
	for i, cmd := range cmds {
		if cmd["status"] != "running" {
			continue
		}
		pidVal, ok := cmd["pid"]
		if !ok {
			continue
		}
		pid := 0
		switch v := pidVal.(type) {
		case float64:
			pid = int(v)
		case int:
			pid = v
		}
		if pid <= 0 {
			continue
		}
		proc, err := os.FindProcess(pid)
		if err != nil {
			cmds[i]["status"] = "cancelled"
			changed = true
			continue
		}
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			cmds[i]["status"] = "cancelled"
			changed = true
		}
	}

	if changed {
		newCmds, _ := json.Marshal(cmds)
		_, _ = embeddedDB.Exec(
			`UPDATE processes SET commands = ? WHERE process_id = ?`,
			string(newCmds), id,
		)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"process_id": id,
		"commands":   cmds,
		"healthy":    !changed,
	})
}

// ── SSE stream ────────────────────────────────────────────────────────────────

func handleSSEStream(w http.ResponseWriter, r *http.Request, id string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// Send connected event.
	connected, _ := json.Marshal(map[string]string{"type": "connected", "process_id": id})
	fmt.Fprintf(w, "data: %s\n\n", connected)
	flusher.Flush()

	// Send last 50 log lines immediately.
	logPath := filepath.Join(embeddedLogsDir, id+".log")
	recent, _ := readLogLines(logPath, "", "", "", "", 50, 0)
	// Rewind: read last 50 by reading all and slicing.
	all, _ := readLogLines(logPath, "", "", "", "", 1<<30, 0)
	if len(all) > 50 {
		recent = all[len(all)-50:]
	} else {
		recent = all
	}
	for _, line := range recent {
		data, _ := json.Marshal(line)
		fmt.Fprintf(w, "data: %s\n\n", data)
	}
	flusher.Flush()

	client := &sseClient{
		processID: id,
		ch:        make(chan string, 200),
		done:      make(chan struct{}),
	}

	// Use a unique key per client (monotonically increasing counter).
	sseClientsMu.Lock()
	var clientKey float64
	sseClients.Range(func(k, _ interface{}) bool {
		if f, ok := k.(float64); ok && f >= clientKey {
			clientKey = f + 1
		}
		return true
	})
	clientKey++
	sseClients.Store(clientKey, client)
	sseClientsMu.Unlock()

	defer func() {
		close(client.done)
		sseClients.Delete(clientKey)
	}()

	ctx := r.Context()
	for {
		select {
		case msg := <-client.ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		case <-ctx.Done():
			return
		}
	}
}

// ── Process-list SSE ──────────────────────────────────────────────────────────

// handleProcessListSSE streams process-list snapshots to the browser.
// A snapshot (full array) is pushed immediately on connect, then again
// whenever any process is created, updated, or deleted.
func handleProcessListSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := make(chan string, 16)

	processListSSEMu.Lock()
	var key float64
	processListSSE.Range(func(k, _ interface{}) bool {
		if f, ok := k.(float64); ok && f >= key {
			key = f + 1
		}
		return true
	})
	key++
	processListSSE.Store(key, ch)
	processListSSEMu.Unlock()

	defer func() {
		processListSSE.Delete(key)
		close(ch)
	}()

	// Send initial snapshot immediately.
	if snap := processListSnapshot(); snap != "" {
		fmt.Fprintf(w, "data: %s\n\n", snap)
		flusher.Flush()
	}

	ctx := r.Context()
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		case <-ctx.Done():
			return
		}
	}
}

// broadcastProcessList pushes a fresh process-list snapshot to all SSE subscribers.
// Call after any mutation (create / update / delete / cleanup).
func broadcastProcessList() {
	snap := processListSnapshot()
	if snap == "" {
		return
	}
	processListSSE.Range(func(k, v interface{}) bool {
		ch := v.(chan string)
		select {
		case ch <- snap:
		default: // subscriber too slow — skip
		}
		return true
	})
}

// processListSnapshot returns the current process list as a JSON string.
func processListSnapshot() string {
	if embeddedDB == nil {
		return ""
	}
	rows, err := embeddedDB.Query(`
		SELECT process_id, name, commands, status, start_time, end_time, tags, cwd
		FROM processes
		ORDER BY CASE WHEN status='running' THEN 0 ELSE 1 END, start_time DESC
	`)
	if err != nil {
		return ""
	}
	defer rows.Close()
	result := []map[string]interface{}{}
	for rows.Next() {
		if p := scanProcess(rows); p != nil {
			result = append(result, p)
		}
	}
	data, _ := json.Marshal(result)
	return string(data)
}

// ── SSE broadcast (per-process log stream) ────────────────────────────────────

func broadcastSSE(processID string, entry interface{}) {
	data, _ := json.Marshal(entry)
	sseClients.Range(func(k, v interface{}) bool {
		c := v.(*sseClient)
		if c.processID == processID {
			select {
			case c.ch <- string(data):
			default: // drop if full
			}
		}
		return true
	})
}

// ── Log file helpers ──────────────────────────────────────────────────────────

func appendLogEntry(processID string, entry map[string]interface{}) error {
	logPath := filepath.Join(embeddedLogsDir, processID+".log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

// readLogLines reads a JSONL log file applying optional filters.
func readLogLines(logPath, q, streamFilter, from, to string, limit, offset int) ([]map[string]interface{}, error) {
	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []map[string]interface{}{}, nil
		}
		return nil, err
	}
	defer f.Close()

	var fromT, toT time.Time
	if from != "" {
		fromT, _ = time.Parse(time.RFC3339Nano, from)
		if fromT.IsZero() {
			fromT, _ = time.Parse(time.RFC3339, from)
		}
	}
	if to != "" {
		toT, _ = time.Parse(time.RFC3339Nano, to)
		if toT.IsZero() {
			toT, _ = time.Parse(time.RFC3339, to)
		}
	}

	var result []map[string]interface{}
	skipped := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}

		// Apply stream filter.
		if streamFilter != "" {
			s, _ := entry["stream"].(string)
			if s != streamFilter {
				continue
			}
		}

		// Apply text search.
		if q != "" {
			msg, _ := entry["message"].(string)
			if !strings.Contains(strings.ToLower(msg), strings.ToLower(q)) {
				continue
			}
		}

		// Apply time range filters.
		if !fromT.IsZero() || !toT.IsZero() {
			tsStr, _ := entry["timestamp"].(string)
			ts, err := time.Parse(time.RFC3339Nano, tsStr)
			if err != nil {
				ts, _ = time.Parse(time.RFC3339, tsStr)
			}
			if !fromT.IsZero() && ts.Before(fromT) {
				continue
			}
			if !toT.IsZero() && ts.After(toT) {
				continue
			}
		}

		// Apply offset.
		if skipped < offset {
			skipped++
			continue
		}

		result = append(result, entry)
		if limit > 0 && len(result) >= limit {
			break
		}
	}

	if result == nil {
		result = []map[string]interface{}{}
	}
	return result, scanner.Err()
}

// ── DB scan helpers ───────────────────────────────────────────────────────────

type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanProcessRow(row rowScanner) map[string]interface{} {
	var processID, commandsJSON, status, startTime string
	var name, endTime, tagsJSON, cwd sql.NullString
	err := row.Scan(&processID, &name, &commandsJSON, &status, &startTime, &endTime, &tagsJSON, &cwd)
	if err != nil {
		return nil
	}
	return buildProcessMap(processID, name.String, commandsJSON, status, startTime, endTime.String, tagsJSON.String, cwd.String)
}

func scanProcess(rows *sql.Rows) map[string]interface{} {
	var processID, commandsJSON, status, startTime string
	var name, endTime, tagsJSON, cwd sql.NullString
	if err := rows.Scan(&processID, &name, &commandsJSON, &status, &startTime, &endTime, &tagsJSON, &cwd); err != nil {
		return nil
	}
	return buildProcessMap(processID, name.String, commandsJSON, status, startTime, endTime.String, tagsJSON.String, cwd.String)
}

func buildProcessMap(processID, name, commandsJSON, status, startTime, endTime, tagsJSON, cwd string) map[string]interface{} {
	var commands interface{}
	if err := json.Unmarshal([]byte(commandsJSON), &commands); err != nil {
		commands = []interface{}{}
	}
	var tags interface{}
	if tagsJSON == "" || tagsJSON == "null" {
		tags = []interface{}{}
	} else if err := json.Unmarshal([]byte(tagsJSON), &tags); err != nil {
		tags = []interface{}{}
	}

	p := map[string]interface{}{
		"process_id": processID,
		"name":       name,
		"commands":   commands,
		"status":     status,
		"start_time": startTime,
		"tags":       tags,
		"cwd":        cwd,
	}

	if endTime != "" {
		p["end_time"] = endTime
	}

	// Return start_time and end_time; clients compute duration client-side.
	return p
}

// ── Small utilities ───────────────────────────────────────────────────────────

// marshalField marshals v to JSON string; returns fallback on nil/error.
func marshalField(v interface{}, fallback string) string {
	if v == nil {
		return fallback
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fallback
	}
	return string(b)
}

// marshalFieldOpt returns nil if v is nil, otherwise the JSON string.
func marshalFieldOpt(v interface{}) interface{} {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return string(b)
}

// nilStr returns nil if v is nil or empty string, otherwise the string pointer.
func nilStr(v interface{}) interface{} {
	if v == nil {
		return nil
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return nil
	}
	return s
}

// Ensure io and strconv are used (avoids unused import errors if trimmed).
var _ = io.EOF
var _ = strconv.Itoa
