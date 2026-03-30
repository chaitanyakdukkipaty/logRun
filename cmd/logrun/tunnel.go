package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// tunnelTool identifies which tunneling binary to use.
type tunnelTool int

const (
	tunnelNone  tunnelTool = iota
	tunnelZrok             // zrok share public --headless <url>
	tunnelNgrok            // ngrok http <port>
)

// TunnelURLs holds the resolved public URLs for the API and web services.
type TunnelURLs struct {
	API string
	Web string
}

// tunnelProcs holds running tunnel processes for cleanup.
type tunnelProcs struct {
	apiProc *exec.Cmd
	webProc *exec.Cmd
	baseDir string
}

// kill terminates tunnel processes and clears their URLs from the state file.
func (tp *tunnelProcs) kill() {
	if tp == nil {
		return
	}
	fmt.Println("\nStopping tunnel…")
	if tp.apiProc != nil {
		tp.apiProc.Process.Kill()
	}
	// Clear tunnel URLs from state file.
	state := loadServiceState()
	state.APITunnel = ""
	state.WebTunnel = ""
	saveServiceState(state)
	fmt.Println("Tunnel stopped.")
}

// startTunnelsAndPrint establishes a single tunnel (API + web on same port),
// prints the public URL, persists it to the state file, and returns the process
// for later cleanup. Blocks only until the tunnel is ready (or returns an error).
func startTunnelsAndPrint(apiPort, _ int) (*tunnelProcs, error) {
	// Always use the authoritative port from the state file if healthy.
	state := loadServiceState()
	if state.Port > 0 && isServerHealthy(state.Port) {
		apiPort = state.Port
	}

	tool, err := detectTunnelTool()
	if err != nil {
		return nil, err
	}

	fmt.Println("\nStarting tunnel…")

	tunnelURL, proc, err := startTunnel(tool, apiPort)
	if err != nil {
		return nil, fmt.Errorf("could not start tunnel: %w", err)
	}
	fmt.Printf("  ✓ Tunnel: %s\n", tunnelURL)

	// Persist tunnel URL to state file.
	state = loadServiceState()
	state.APITunnel = tunnelURL
	state.WebTunnel = tunnelURL // same URL serves web UI too
	saveServiceState(state)

	fmt.Printf("\nShare this with your team:\n  🌐 Dashboard + API: %s\n\n", tunnelURL)

	baseDir := logrunDir()
	return &tunnelProcs{apiProc: proc, webProc: proc, baseDir: baseDir}, nil
}

// runShare is the entry point for `logrun share` standalone subcommand.
func runShare(apiPort, _ int) error {
	tp, err := startTunnelsAndPrint(apiPort, apiPort)
	if err != nil {
		return err
	}
	fmt.Println("Press Ctrl+C to stop sharing.")
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan
	tp.kill()
	return nil
}

// detectTunnelTool returns the best available tunneling tool.
func detectTunnelTool() (tunnelTool, error) {
	// Prefer zrok — check binary + enabled environment.
	if path, err := exec.LookPath("zrok"); err == nil && path != "" {
		if isZrokEnabled() {
			return tunnelZrok, nil
		}
		return tunnelNone, fmt.Errorf("zrok is installed but not enabled — run `zrok enable <token>` first")
	}

	if path, err := exec.LookPath("ngrok"); err == nil && path != "" {
		return tunnelNgrok, nil
	}

	return tunnelNone, fmt.Errorf("no tunnel tool found — install zrok (https://zrok.io) or ngrok (https://ngrok.com)")
}

// isZrokEnabled checks that zrok has a configured Ziti identity (i.e. `zrok enable` was run).
func isZrokEnabled() bool {
	out, err := exec.Command("zrok", "status").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "Ziti Identity")
}

// startTunnel launches a tunnel for the given local port and returns the
// public URL plus the running *exec.Cmd so the caller can kill it later.
func startTunnel(tool tunnelTool, port int) (string, *exec.Cmd, error) {
	switch tool {
	case tunnelZrok:
		return startZrokTunnel(port)
	case tunnelNgrok:
		return startNgrokTunnel(port)
	default:
		return "", nil, fmt.Errorf("unsupported tunnel tool")
	}
}

// ── zrok ─────────────────────────────────────────────────────────────────────

func startZrokTunnel(port int) (string, *exec.Cmd, error) {
	target := fmt.Sprintf("http://localhost:%d", port)
	cmd := exec.Command("zrok", "share", "public", "--headless", target)
	cmd.SysProcAttr = detachedSysProcAttr()

	// zrok writes its share URL in a JSON log line to stderr.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", nil, err
	}

	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("zrok start: %w", err)
	}

	urlCh := make(chan string, 1)

	scanForURL := func(r io.Reader) {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			line := scanner.Text()
			if url := extractZrokURL(line); url != "" {
				select {
				case urlCh <- url:
				default:
				}
				return
			}
		}
	}

	go scanForURL(stdout)
	go scanForURL(stderr)

	select {
	case url, ok := <-urlCh:
		if !ok || url == "" {
			cmd.Process.Kill()
			return "", nil, fmt.Errorf("zrok did not emit a public URL")
		}
		return url, cmd, nil
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		return "", nil, fmt.Errorf("timed out waiting for zrok URL")
	}
}

// extractZrokURL looks for the share URL in a line of zrok output.
// zrok v1+ emits JSON log lines where the URL appears in the "msg" field:
//
//	{"msg":"access your zrok share at the following endpoints:\n https://abc.share.zrok.io",...}
//
// Older versions / plain text mode may emit a bare URL line.
func extractZrokURL(line string) string {
	line = strings.TrimSpace(line)

	// JSON log format: extract the msg value and search within it.
	if strings.HasPrefix(line, "{") {
		const key = `"msg":"`
		idx := strings.Index(line, key)
		if idx != -1 {
			rest := line[idx+len(key):]
			end := strings.Index(rest, `"`)
			if end != -1 {
				msg := rest[:end]
				// msg may contain \n as a literal escape sequence.
				msg = strings.ReplaceAll(msg, `\n`, "\n")
				for _, part := range strings.Fields(msg) {
					if (strings.HasPrefix(part, "https://") || strings.HasPrefix(part, "http://")) &&
						strings.Contains(part, ".zrok.io") {
						return part
					}
				}
			}
		}
		return ""
	}

	// Plain-text: bare URL on its own line.
	for _, prefix := range []string{"https://", "http://"} {
		if strings.HasPrefix(line, prefix) && strings.Contains(line, ".zrok.io") {
			return line
		}
	}
	fields := strings.Fields(line)
	for _, f := range fields {
		if (strings.HasPrefix(f, "https://") || strings.HasPrefix(f, "http://")) &&
			strings.Contains(f, ".zrok.io") {
			return f
		}
	}
	return ""
}

// ── ngrok ─────────────────────────────────────────────────────────────────────

func startNgrokTunnel(port int) (string, *exec.Cmd, error) {
	cmd := exec.Command("ngrok", "http", fmt.Sprintf("%d", port),
		"--log=stdout", "--log-format=json")
	cmd.SysProcAttr = detachedSysProcAttr()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", nil, err
	}

	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("ngrok start: %w", err)
	}

	urlCh := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if url := extractNgrokURL(line); url != "" {
				urlCh <- url
				return
			}
		}
		close(urlCh)
	}()

	select {
	case url, ok := <-urlCh:
		if !ok || url == "" {
			cmd.Process.Kill()
			return "", nil, fmt.Errorf("ngrok did not emit a public URL")
		}
		return url, cmd, nil
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		return "", nil, fmt.Errorf("timed out waiting for ngrok URL")
	}
}

// extractNgrokURL parses ngrok JSON log lines for the public URL.
// ngrok emits: {"lvl":"info","msg":"started tunnel","url":"https://..."}
func extractNgrokURL(line string) string {
	if !strings.Contains(line, `"started tunnel"`) {
		return ""
	}
	const key = `"url":"`
	idx := strings.Index(line, key)
	if idx == -1 {
		return ""
	}
	rest := line[idx+len(key):]
	end := strings.Index(rest, `"`)
	if end == -1 {
		return ""
	}
	return rest[:end]
}
