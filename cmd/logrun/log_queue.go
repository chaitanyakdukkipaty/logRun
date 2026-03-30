package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ── Queue constants ────────────────────────────────────────────────────────────

const (
	logQueueCapacity    = 8192  // in-memory channel buffer
	overflowBatchSize   = 512   // entries per overflow file
	overflowDrainPeriod = 250 * time.Millisecond
	overflowMaxFiles    = 200   // hard cap on overflow files on disk
)

// ── Types ──────────────────────────────────────────────────────────────────────

type logQueueItem struct {
	processID string
	entry     map[string]interface{}
}

// ── Package-level queue state ──────────────────────────────────────────────────

var (
	logQueue         chan logQueueItem
	logQueueOnce     sync.Once
	overflowDirPath  string

	// metrics (read via /health/queue)
	queueDropped   atomic.Int64
	queueOverflow  atomic.Int64
	queueProcessed atomic.Int64
)

// initLogQueue starts the queue workers. Called once from StartEmbeddedServer.
func initLogQueue(dataDir string) {
	logQueueOnce.Do(func() {
		overflowDirPath = filepath.Join(dataDir, "overflow")
		if err := os.MkdirAll(overflowDirPath, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "log queue: could not create overflow dir: %v\n", err)
		}

		logQueue = make(chan logQueueItem, logQueueCapacity)

		go logQueueWorker()
		go overflowDrainer()
	})
}

// ── Enqueue (called by HTTP handlers) ─────────────────────────────────────────

// enqueueLog puts a log entry on the queue. If the in-memory queue is full the
// entry is spilled to a temp file in the overflow directory. If overflow files
// exceed overflowMaxFiles the entry is dropped and the drop counter incremented.
func enqueueLog(processID string, entry map[string]interface{}) {
	item := logQueueItem{processID: processID, entry: entry}

	select {
	case logQueue <- item:
		// happy path – queued in memory
	default:
		// queue full → spill to disk
		if err := spillToOverflow(item); err != nil {
			queueDropped.Add(1)
			fmt.Fprintf(os.Stderr, "log queue: dropped entry for %s: %v\n", processID, err)
		} else {
			queueOverflow.Add(1)
		}
	}
}

// ── Primary worker ─────────────────────────────────────────────────────────────

func logQueueWorker() {
	for item := range logQueue {
		if err := appendLogEntry(item.processID, item.entry); err != nil {
			fmt.Fprintf(os.Stderr, "log queue: write error for %s: %v\n", item.processID, err)
		} else {
			broadcastSSE(item.processID, item.entry)
			queueProcessed.Add(1)
		}
	}
}

// ── Overflow spill / drain ─────────────────────────────────────────────────────

// spillToOverflow writes one log entry to a new temp file in the overflow dir.
// Each overflow file holds up to overflowBatchSize entries (JSONL).
func spillToOverflow(item logQueueItem) error {
	// Refuse to create more overflow files than the hard cap.
	entries, err := filepath.Glob(filepath.Join(overflowDirPath, "*.jsonl"))
	if err != nil {
		return err
	}
	if len(entries) >= overflowMaxFiles {
		return fmt.Errorf("overflow dir full (%d files)", len(entries))
	}

	// Find the newest (last) overflow file and append if it has room, otherwise
	// create a fresh file.
	sort.Strings(entries)
	var target string
	if len(entries) > 0 {
		last := entries[len(entries)-1]
		if lineCount(last) < overflowBatchSize {
			target = last
		}
	}
	if target == "" {
		target = filepath.Join(overflowDirPath,
			fmt.Sprintf("overflow-%d.jsonl", time.Now().UnixNano()))
	}

	f, err := os.OpenFile(target, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	line, err := json.Marshal(map[string]interface{}{
		"process_id": item.processID,
		"entry":      item.entry,
	})
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

// overflowDrainer runs every overflowDrainPeriod and re-queues entries from
// overflow files when the in-memory queue has room.
func overflowDrainer() {
	for range time.Tick(overflowDrainPeriod) {
		drainOneOverflowFile()
	}
}

func drainOneOverflowFile() {
	files, err := filepath.Glob(filepath.Join(overflowDirPath, "*.jsonl"))
	if err != nil || len(files) == 0 {
		return
	}
	sort.Strings(files)
	path := files[0] // drain oldest file first

	f, err := os.Open(path)
	if err != nil {
		return
	}

	var lines [][]byte
	dec := json.NewDecoder(f)
	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			break
		}
		lines = append(lines, raw)
	}
	f.Close()

	// Try to drain as many lines as the queue will accept right now.
	drained := 0
	for _, raw := range lines {
		var wrapper struct {
			ProcessID string                 `json:"process_id"`
			Entry     map[string]interface{} `json:"entry"`
		}
		if err := json.Unmarshal(raw, &wrapper); err != nil {
			drained++ // skip malformed lines
			continue
		}

		select {
		case logQueue <- logQueueItem{processID: wrapper.ProcessID, entry: wrapper.Entry}:
			drained++
		default:
			// Queue still full — stop draining, leave file for next tick.
			break
		}
	}

	if drained == len(lines) {
		// All lines re-queued — remove the file.
		os.Remove(path)
	} else if drained > 0 {
		// Partially drained — rewrite file with remaining lines.
		remaining := lines[drained:]
		rewriteOverflowFile(path, remaining)
	}
}

func rewriteOverflowFile(path string, lines [][]byte) {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	for _, line := range lines {
		f.Write(append(line, '\n'))
	}
	f.Close()
	os.Rename(tmp, path)
}

// lineCount returns the number of newlines in a file (≈ number of JSONL entries).
func lineCount(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n := 0
	for _, b := range data {
		if b == '\n' {
			n++
		}
	}
	return n
}

// WaitForQueueDrain blocks until the in-memory queue is empty or the timeout
// elapses. Call this before the process exits to avoid losing buffered entries.
func WaitForQueueDrain(timeout time.Duration) {
	if logQueue == nil {
		return
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(logQueue) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ── Queue metrics (exposed via /health/queue) ──────────────────────────────────

type queueStats struct {
	Depth         int   `json:"depth"`
	Capacity      int   `json:"capacity"`
	Processed     int64 `json:"processed"`
	Overflowed    int64 `json:"overflowed"`
	Dropped       int64 `json:"dropped"`
	OverflowFiles int   `json:"overflow_files"`
}

func getQueueStats() queueStats {
	files, _ := filepath.Glob(filepath.Join(overflowDirPath, "*.jsonl"))
	return queueStats{
		Depth:         len(logQueue),
		Capacity:      logQueueCapacity,
		Processed:     queueProcessed.Load(),
		Overflowed:    queueOverflow.Load(),
		Dropped:       queueDropped.Load(),
		OverflowFiles: len(files),
	}
}
