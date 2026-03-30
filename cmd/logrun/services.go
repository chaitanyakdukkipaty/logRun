package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

const (
	preferredAPIPort    = 3001
	preferredWebPort    = 3000
	serviceStartTimeout = 30 * time.Second
	stateFileName       = ".logrun-services.json"
)

// ServiceState records the ports the API and web services are running on,
// plus any active public tunnel URLs.
// It is written to <project_root>/.logrun-services.json so every CLI instance
// can discover the correct ports even when they differ from the defaults.
type ServiceState struct {
	APIPort   int    `json:"api_port"`
	WebPort   int    `json:"web_port"`
	APITunnel string `json:"api_tunnel,omitempty"`
	WebTunnel string `json:"web_tunnel,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

func loadServiceState(baseDir string) ServiceState {
	data, err := os.ReadFile(filepath.Join(baseDir, stateFileName))
	if err != nil {
		return ServiceState{}
	}
	var s ServiceState
	_ = json.Unmarshal(data, &s)
	return s
}

func saveServiceState(baseDir string, s ServiceState) {
	s.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(baseDir, stateFileName), data, 0644)
}

// ensureServicesRunning starts the LogRun API and Web services if they are not
// already running. It returns the ports they are (or will be) listening on.
// Both results are always valid — fallback to defaults on any error.
func ensureServicesRunning() (apiPort, webPort int) {
	baseDir, err := findProjectRoot()
	if err != nil {
		// Can't locate the project — assume externally managed services.
		return preferredAPIPort, preferredWebPort
	}

	state := loadServiceState(baseDir)

	fmt.Println("Checking LogRun services…")

	apiPort, err = ensureAPIRunning(baseDir, state.APIPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ⚠  Could not start LogRun API: %v\n", err)
		apiPort = preferredAPIPort
	}

	webPort, err = ensureWebRunning(baseDir, apiPort, state.WebPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ⚠  Could not start LogRun Web: %v\n", err)
		webPort = preferredWebPort
	}

	// Persist resolved ports so the next invocation finds them immediately.
	saveServiceState(baseDir, ServiceState{APIPort: apiPort, WebPort: webPort})

	fmt.Println()
	return apiPort, webPort
}

// ── API service ───────────────────────────────────────────────────────────────

// ensureAPIRunning checks for a running API in this order:
//  1. The port stored in the state file (survives across invocations)
//  2. The hardcoded default port (3001)
//  3. A freshly-found free port (starts a new process)
func ensureAPIRunning(baseDir string, storedPort int) (int, error) {
	// 1. Try the stored port first.
	if storedPort > 0 && storedPort != preferredAPIPort && isAPIHealthy(storedPort) {
		fmt.Printf("  ✓ LogRun API already running (:%d)\n", storedPort)
		return storedPort, nil
	}

	// 2. Try the preferred default port.
	if isAPIHealthy(preferredAPIPort) {
		fmt.Printf("  ✓ LogRun API already running (:%d)\n", preferredAPIPort)
		return preferredAPIPort, nil
	}

	// 3. Nothing is running — pick a port and start the service.
	port := preferredAPIPort
	if !isPortFree(port) {
		port = findFreePort()
		if port == 0 {
			return 0, fmt.Errorf("could not find a free port for LogRun API")
		}
	}

	fmt.Printf("  → Starting LogRun API on :%d\n", port)
	if err := startAPIService(port, baseDir); err != nil {
		return 0, err
	}

	if err := waitForService(fmt.Sprintf("http://localhost:%d/health", port), serviceStartTimeout); err != nil {
		return 0, fmt.Errorf("API did not become ready: %w", err)
	}

	fmt.Printf("  ✓ LogRun API ready (:%d)\n", port)
	return port, nil
}

func startAPIService(port int, baseDir string) error {
	if _, err := exec.LookPath("node"); err != nil {
		return fmt.Errorf("node not found in PATH")
	}

	apiDir := filepath.Join(baseDir, "api")
	if _, err := os.Stat(filepath.Join(apiDir, "server.js")); err != nil {
		return fmt.Errorf("api/server.js not found in %s", baseDir)
	}

	logOut := openLogFile(filepath.Join(baseDir, "logs", "api.log"))

	cmd := exec.Command("node", "server.js")
	cmd.Dir = apiDir
	cmd.Env = append(os.Environ(), fmt.Sprintf("PORT=%d", port))
	cmd.Stdout = logOut
	cmd.Stderr = logOut
	cmd.SysProcAttr = detachedSysProcAttr()
	return cmd.Start()
}

// ── Web service ───────────────────────────────────────────────────────────────

// ensureWebRunning checks for a running web server in this order:
//  1. The port stored in the state file
//  2. The hardcoded default port and common Vite alternates (3000, 5173, 5174)
//  3. A freshly-found free port (starts a new Vite dev process)
func ensureWebRunning(baseDir string, apiPort, storedPort int) (int, error) {
	// 1. Try the stored port first (skip if it's a common default we'll check anyway).
	if storedPort > 0 && !isCommonWebPort(storedPort) && isWebHealthy(storedPort) {
		fmt.Printf("  ✓ LogRun Web already running (:%d)\n", storedPort)
		return storedPort, nil
	}

	// 2. Try known common web ports.
	for _, p := range []int{preferredWebPort, 5173, 5174} {
		if isWebHealthy(p) {
			fmt.Printf("  ✓ LogRun Web already running (:%d)\n", p)
			return p, nil
		}
	}

	// 3. Check prerequisites before launching.
	webDir := filepath.Join(baseDir, "web")
	if _, err := os.Stat(filepath.Join(webDir, "node_modules")); err != nil {
		return 0, fmt.Errorf("web/node_modules not found — run `npm install` in %s/web first", baseDir)
	}
	if _, err := exec.LookPath("npm"); err != nil {
		return 0, fmt.Errorf("npm not found in PATH")
	}

	port := preferredWebPort
	if !isPortFree(port) {
		port = findFreePort()
		if port == 0 {
			return 0, fmt.Errorf("could not find a free port for LogRun Web")
		}
	}

	fmt.Printf("  → Starting LogRun Web on :%d\n", port)
	if err := startWebService(port, apiPort, baseDir); err != nil {
		return 0, err
	}

	if err := waitForService(fmt.Sprintf("http://localhost:%d/", port), serviceStartTimeout); err != nil {
		return 0, fmt.Errorf("web did not become ready: %w", err)
	}

	fmt.Printf("  ✓ LogRun Web ready → http://localhost:%d\n", port)
	return port, nil
}

func isCommonWebPort(p int) bool {
	for _, cp := range []int{preferredWebPort, 5173, 5174} {
		if p == cp {
			return true
		}
	}
	return false
}

func startWebService(webPort, apiPort int, baseDir string) error {
	webDir := filepath.Join(baseDir, "web")
	logOut := openLogFile(filepath.Join(baseDir, "logs", "web.log"))

	cmd := exec.Command("npm", "run", "dev")
	cmd.Dir = webDir
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("VITE_PORT=%d", webPort),
		fmt.Sprintf("VITE_API_URL=http://localhost:%d", apiPort),
	)
	cmd.Stdout = logOut
	cmd.Stderr = logOut
	cmd.SysProcAttr = detachedSysProcAttr()
	return cmd.Start()
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// findProjectRoot walks up from the binary location (and the working directory)
// looking for a directory that contains both api/server.js and web/package.json.
func findProjectRoot() (string, error) {
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Dir(dir), // binary is typically at <root>/bin/logrun
			dir,
		)
	}
	if cwd, err := os.Getwd(); err == nil {
		dir := cwd
		for i := 0; i < 5; i++ {
			candidates = append(candidates, dir)
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}

	for _, c := range candidates {
		if isProjectRoot(c) {
			return c, nil
		}
	}
	return "", fmt.Errorf("could not find LogRun project root (api/server.js + web/package.json)")
}

func isProjectRoot(dir string) bool {
	_, apiErr := os.Stat(filepath.Join(dir, "api", "server.js"))
	_, webErr := os.Stat(filepath.Join(dir, "web", "package.json"))
	return apiErr == nil && webErr == nil
}

func isPortFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

func findFreePort() int {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func isAPIHealthy(port int) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://localhost:%d/health", port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

func isWebHealthy(port int) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://localhost:%d/", port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 500
}

func waitForService(url string, timeout time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("service at %s did not become ready within %s", url, timeout)
}

func openLogFile(path string) *os.File {
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		f, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	}
	return f
}

// parsePortFromURL extracts the port number from a URL string.
// Falls back to defaultPort if parsing fails or no port is present.
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
