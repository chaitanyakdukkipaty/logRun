package main

import (
"encoding/json"
"fmt"
"net"
"net/http"
"net/url"
"os"
"path/filepath"
"strconv"
"time"
)

const (
preferredPort       = 4000 // default combined API + web port
serviceStartTimeout = 15 * time.Second
stateFileName       = ".services.json"
lockFileName        = ".server.lock"
)

// ServiceState stores the port the embedded server is running on plus any
// active tunnel URLs. Written to ~/.logrun/.services.json.
type ServiceState struct {
Port      int    `json:"port"`
PID       int    `json:"pid"`
APITunnel string `json:"api_tunnel,omitempty"`
WebTunnel string `json:"web_tunnel,omitempty"`
TunnelPID int    `json:"tunnel_pid,omitempty"`
UpdatedAt string `json:"updated_at"`
}

// logrunDir returns ~/.logrun, creating it if needed.
func logrunDir() string {
home, err := os.UserHomeDir()
if err != nil {
home = os.TempDir()
}
dir := filepath.Join(home, ".logrun")
_ = os.MkdirAll(dir, 0755)
return dir
}

func stateFilePath() string { return filepath.Join(logrunDir(), stateFileName) }
func lockFilePath() string  { return filepath.Join(logrunDir(), lockFileName) }

func loadServiceState() ServiceState {
data, err := os.ReadFile(stateFilePath())
if err != nil {
return ServiceState{}
}
var s ServiceState
_ = json.Unmarshal(data, &s)
return s
}

func saveServiceState(s ServiceState) {
s.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
data, _ := json.MarshalIndent(s, "", "  ")
_ = os.WriteFile(stateFilePath(), data, 0644)
}

// isServerHealthy checks whether the embedded server at the given port responds to /health.
func isServerHealthy(port int) bool {
client := &http.Client{Timeout: 2 * time.Second}
resp, err := client.Get(fmt.Sprintf("http://localhost:%d/health", port))
if err != nil {
return false
}
resp.Body.Close()
return resp.StatusCode < 500
}

// ensureServerRunning guarantees exactly one embedded server is running and
// returns its port. Safe to call from multiple concurrent CLI instances.
//
// Algorithm:
//  1. Read ~/.logrun/.services.json — if healthy, reuse.
//  2. Acquire ~/.logrun/.server.lock (PID file). If already locked by a live
//     process, wait for it to write its port and return that.
//  3. Find free port (tries preferredPort first).
//  4. Start embedded server goroutine.
//  5. Wait until healthy, write state, release lock.
func ensureServerRunning() int {
fmt.Println("Checking LogRun services…")

// ── Step 1: reuse if already healthy ─────────────────────────────────────
state := loadServiceState()
if state.Port > 0 && isServerHealthy(state.Port) {
fmt.Printf("  ✓ LogRun already running on :%d\n\n", state.Port)
return state.Port
}

// ── Step 2: acquire lock ──────────────────────────────────────────────────
lockPath := lockFilePath()
if acquired := tryAcquireLock(lockPath); !acquired {
// Another instance is starting the server — wait for it.
fmt.Println("  ⏳ Another instance is starting the server, waiting…")
for i := 0; i < 30; i++ {
time.Sleep(500 * time.Millisecond)
state = loadServiceState()
if state.Port > 0 && isServerHealthy(state.Port) {
fmt.Printf("  ✓ LogRun running on :%d (started by peer)\n\n", state.Port)
return state.Port
}
}
// Peer may have failed — fall through to start ourselves.
}
defer releaseLock(lockPath)

// ── Step 3: find free port ────────────────────────────────────────────────
port := findFreePort(preferredPort)

// ── Step 4: start embedded server ────────────────────────────────────────
if err := StartEmbeddedServer(port); err != nil {
fmt.Fprintf(os.Stderr, "  ✗ Failed to start embedded server: %v\n", err)
return port
}

// ── Step 5: wait until healthy, persist ──────────────────────────────────
deadline := time.Now().Add(serviceStartTimeout)
for time.Now().Before(deadline) {
if isServerHealthy(port) {
saveServiceState(ServiceState{Port: port, PID: os.Getpid()})
fmt.Printf("  ✓ LogRun started on :%d\n\n", port)
return port
}
time.Sleep(200 * time.Millisecond)
}

fmt.Fprintf(os.Stderr, "  ⚠  Server did not become healthy within %s\n", serviceStartTimeout)
return port
}

// findFreePort tries preferred first, then any free port.
func findFreePort(preferred int) int {
if isPortFree(preferred) {
return preferred
}
ln, err := net.Listen("tcp", ":0")
if err != nil {
return preferred
}
p := ln.Addr().(*net.TCPAddr).Port
ln.Close()
return p
}

func isPortFree(port int) bool {
ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
if err != nil {
return false
}
ln.Close()
return true
}

// ── Lock helpers (PID file, no syscall.Flock for Windows compat) ─────────────

func tryAcquireLock(path string) bool {
// Read existing lock
if data, err := os.ReadFile(path); err == nil {
var existing struct {
PID int `json:"pid"`
}
if json.Unmarshal(data, &existing) == nil && existing.PID > 0 {
if isProcessAlive(existing.PID) {
return false // lock held by live process
}
}
}
// Write our own lock
data, _ := json.Marshal(map[string]int{"pid": os.Getpid()})
return os.WriteFile(path, data, 0644) == nil
}

func releaseLock(path string) { _ = os.Remove(path) }

func isProcessAlive(pid int) bool {
p, err := os.FindProcess(pid)
if err != nil {
return false
}
// On Unix, FindProcess always succeeds; send signal 0 to check.
return p.Signal(nil) == nil
}

// parsePortFromURL extracts the port from a URL string, returning defaultPort on failure.
func parsePortFromURL(rawURL string, defaultPort int) int {
u, err := url.Parse(rawURL)
if err != nil {
return defaultPort
}
portStr := u.Port()
if portStr == "" {
return defaultPort
}
p, err := strconv.Atoi(portStr)
if err != nil || p <= 0 {
return defaultPort
}
return p
}

// ── Legacy compatibility shims ────────────────────────────────────────────────
// The old code had separate API/Web ports. Keep minimal shims used by tunnel.go.

func isAPIHealthy(port int) bool  { return isServerHealthy(port) }
func isWebHealthy(port int) bool  { return isServerHealthy(port) }
func findProjectRoot() (string, error) { return logrunDir(), nil }
