package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/manifoldco/promptui"
	"github.com/spf13/cobra"
)

// kubectlFlags holds all flags for the kubectl subcommand.
var kubectlFlags struct {
	namespace     string
	pod           string
	container     string
	tail          int
	since         string
	follow        bool
	allNamespaces bool
}

// kubectl-specific flag vars that shadow the root vars only for this subcommand.
var (
	kubectlName   string
	kubectlTags   string
	kubectlAPIURL string
	kubectlShare  bool
)

// kubectlCmd is the Cobra subcommand for fetching Kubernetes pod logs.
var kubectlCmd = &cobra.Command{
	Use:   "kubectl",
	Short: "Fetch Kubernetes pod logs through LogRun",
	Long: `Fetch logs from Kubernetes pods and capture them in LogRun.

Provide --namespace and --pod for direct, non-interactive use, or omit
them to be guided through an interactive selection menu.

Pod name is treated as a substring/regex pattern — all matching pods are
fetched in parallel, each tracked as a separate command in LogRun.

Examples:
  logrun kubectl --namespace prod --pod my-app
  logrun kubectl --namespace staging --pod "worker-" --tail 200
  logrun kubectl --follow --namespace dev --pod api-server
  logrun kubectl   # fully interactive`,
	RunE: runKubectl,
}

func init() {
	f := kubectlCmd.Flags()
	f.StringVarP(&kubectlFlags.namespace, "namespace", "n", "", "Kubernetes namespace (interactive if omitted)")
	f.StringVarP(&kubectlFlags.pod, "pod", "p", "", "Pod name or pattern (interactive if omitted)")
	f.StringVarP(&kubectlFlags.container, "container", "c", "", "Container name (for multi-container pods)")
	f.IntVar(&kubectlFlags.tail, "tail", -1, "Number of lines from end of log (-1 = all; bare --tail defaults to 100)")
	f.StringVar(&kubectlFlags.since, "since", "", "Only logs newer than a duration e.g. 1h, 30m")
	f.BoolVarP(&kubectlFlags.follow, "follow", "f", true, "Stream live logs (default: on; use --follow=false to disable)")
	f.BoolVar(&kubectlFlags.allNamespaces, "all-namespaces", false, "Search pods across all namespaces")
	f.StringVar(&kubectlName, "name", "", "Friendly process name (default: kubectl/<namespace>/<pattern>)")
	f.StringVar(&kubectlTags, "tags", "", "Comma-separated tags")
	f.StringVar(&kubectlAPIURL, "api-url", fmt.Sprintf("http://localhost:%d", preferredPort), "LogRun server URL")
	f.BoolVar(&kubectlShare, "share", false, "Expose LogRun via zrok/ngrok for team sharing")

	// Allow --tail without a value (means "last 100 lines").
	kubectlCmd.Flags().Lookup("tail").NoOptDefVal = "100"
}

// runKubectl is the main entry point for the kubectl subcommand.
func runKubectl(cmd *cobra.Command, args []string) error {
	// Auto-start embedded server unless the user explicitly supplied --api-url.
	var apiPort int
	if !cmd.Flags().Changed("api-url") {
		apiPort = ensureServerRunning()
		runner.apiBaseURL = fmt.Sprintf("http://localhost:%d/api", apiPort)
	} else {
		runner.apiBaseURL = kubectlAPIURL
		apiPort = parsePortFromURL(kubectlAPIURL, preferredPort)
	}

	// Start tunnels synchronously if --share was requested so URLs are shown
	// before the interactive prompts begin.
	var tp *tunnelProcs
	if kubectlShare {
		var tunnelErr error
		tp, tunnelErr = startTunnelsAndPrint(apiPort, apiPort) // single port
		if tunnelErr != nil {
			fmt.Fprintf(os.Stderr, "tunnel error: %v\n", tunnelErr)
		}
	}

	// ── Step 1: Resolve namespace ────────────────────────────────────────────
	ns := kubectlFlags.namespace
	if ns == "" && !kubectlFlags.allNamespaces {
		namespaces, err := listNamespaces()
		if err != nil {
			return fmt.Errorf("failed to list namespaces: %w", err)
		}
		ns, err = promptSelectNamespace(namespaces)
		if err != nil {
			return err
		}
	}

	// ── Step 2: List all available pods (needed for both flows) ─────────────
	allPods, err := listPods(ns, kubectlFlags.allNamespaces)
	if err != nil {
		return fmt.Errorf("failed to list pods: %w", err)
	}

	// ── Step 3: Resolve initial pod pattern(s) ───────────────────────────────
	// --pod accepts comma-separated patterns; interactive mode prompts the user.
	var initialPatterns []string
	if kubectlFlags.pod != "" {
		for _, p := range strings.Split(kubectlFlags.pod, ",") {
			if t := strings.TrimSpace(p); t != "" {
				initialPatterns = append(initialPatterns, t)
			}
		}
	} else {
		p, promptErr := promptPodPattern(allPods)
		if promptErr != nil {
			return promptErr
		}
		initialPatterns = []string{p}
	}

	// ── Step 4: Find matches and (interactive) accumulation loop ─────────────
	// matched is a de-duplicated ordered set of pod names across all patterns.
	matched, err := matchPodsFromPatterns(allPods, initialPatterns)
	if err != nil {
		return err
	}
	if len(matched) == 0 {
		nsLabel := ns
		if kubectlFlags.allNamespaces {
			nsLabel = "all namespaces"
		}
		return fmt.Errorf("no pods matched %v in %s", initialPatterns, nsLabel)
	}

	// In interactive mode, let the user refine the selection before fetching.
	if kubectlFlags.pod == "" {
		for {
			action, addErr := promptConfirmPods(matched)
			if addErr != nil {
				return addErr
			}
			switch action {
			case confirmActionFetch:
				// proceed
			case confirmActionCancel:
				return fmt.Errorf("cancelled by user")
			case confirmActionAddMore:
				extraPattern, promptErr := promptPodPattern(allPods)
				if promptErr != nil {
					return promptErr
				}
				extra, matchErr := findMatchingPods(allPods, extraPattern)
				if matchErr != nil {
					fmt.Fprintf(os.Stderr, "warning: invalid pattern %q: %v\n", extraPattern, matchErr)
					continue
				}
				if len(extra) == 0 {
					fmt.Printf("No pods matched %q — try a different pattern.\n", extraPattern)
					continue
				}
				matched = unionPods(matched, extra)
				continue
			}
			break
		}
	}

	// ── Step 5: Set up LogRun process ────────────────────────────────────────
	processName := kubectlName
	if processName == "" {
		nsLabel := ns
		if kubectlFlags.allNamespaces {
			nsLabel = "all-namespaces"
		}
		processName = fmt.Sprintf("kubectl/%s/%s", nsLabel, strings.Join(initialPatterns, ","))
	}

	tagList := parseKubectlTags(kubectlTags)
	tagList = appendUnique(tagList, "kubectl")
	if ns != "" {
		tagList = appendUnique(tagList, ns)
	}

	processID := uuid.New().String()

	// Build CommandInfo entries — one per matched pod.
	commands := make([]CommandInfo, len(matched))
	commandIDs := make([]string, len(matched))
	for i, pod := range matched {
		cid := uuid.New().String()
		commandIDs[i] = cid
		commands[i] = CommandInfo{
			CommandID: cid,
			Command:   kubectlLogsCmdStr(ns, pod, kubectlFlags.container),
			Status:    "running",
			StartTime: time.Now(),
		}
	}

	metadata := &ProcessMetadata{
		ProcessID: processID,
		Name:      processName,
		Commands:  commands,
		Status:    "running",
		StartTime: time.Now(),
		Tags:      tagList,
	}

	if err := runner.createProcess(metadata); err != nil {
		// Non-fatal — continue even if API is unreachable.
		fmt.Fprintf(os.Stderr, "warning: could not register process with LogRun API: %v\n", err)
	}

	// ── Step 6: Start log batcher ────────────────────────────────────────────
	runner.batcher = NewLogBatcher(processID, runner.apiBaseURL, runner.httpClient, runner.batchConfig)
	runner.batcher.Start()

	// ── Step 7: Set up signal handling for graceful cancellation ─────────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		cancel() // propagates to all kubectl subprocesses via context

		fmt.Fprintf(os.Stderr, "\nInterrupted — marking process as cancelled\n")

		endTime := time.Now()
		for i, cid := range commandIDs {
			_ = runner.updateCommandStatus(processID, cid, "cancelled", -1, endTime)
			metadata.Commands[i].Status = "cancelled"
		}
		metadata.Status = "cancelled"
		metadata.EndTime = &endTime
		_ = runner.updateProcess(metadata)

		runner.batcher.Close()
		if tp != nil {
			tp.kill()
		}
		os.Exit(130)
	}()

	// ── Step 8: Fetch logs from each matched pod (parallel) ──────────────────
	fmt.Printf("Fetching logs from %d pod(s)\n", len(matched))

	var wg sync.WaitGroup
	errs := make(chan error, len(matched))

	for i, pod := range matched {
		wg.Add(1)
		go func(podName, commandID string) {
			defer wg.Done()
			if err := streamPodLogsToLogrun(ctx, processID, commandID, ns, podName, kubectlFlags.container); err != nil {
				errs <- fmt.Errorf("pod %s: %w", podName, err)
			}
		}(pod, commandIDs[i])
	}

	wg.Wait()
	signal.Stop(sigChan)
	close(errs)
	runner.batcher.Close()

	// ── Step 9: Finalize process status ──────────────────────────────────────
	var errMsgs []string
	for e := range errs {
		errMsgs = append(errMsgs, e.Error())
	}

	finalStatus := "completed"
	var finalErr error
	if len(errMsgs) > 0 {
		finalStatus = "failed"
		finalErr = fmt.Errorf("errors fetching pod logs:\n  %s", strings.Join(errMsgs, "\n  "))
	}

	endTime := time.Now()
	metadata.Status = finalStatus
	metadata.EndTime = &endTime
	if updateErr := runner.updateProcess(metadata); updateErr != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to update process status: %v\n", updateErr)
	}

	// If tunnels are running, keep them alive so teammates can view the dashboard.
	// Block until the user presses Ctrl+C, then shut down tunnels.
	if tp != nil {
		fmt.Printf("\nLog fetch complete. Tunnels still active — press Ctrl+C to stop sharing.\n")
		holdSig := make(chan os.Signal, 1)
		signal.Notify(holdSig, os.Interrupt, syscall.SIGTERM)
		<-holdSig
		tp.kill()
	}

	return finalErr
}

// ── kubectl helpers ──────────────────────────────────────────────────────────

// listNamespaces returns all namespace names visible to kubectl.
func listNamespaces() ([]string, error) {
	out, err := runKubectlCmd("get", "namespaces", "-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(out)
	if raw == "" {
		return nil, fmt.Errorf("no namespaces found — is kubectl configured correctly?")
	}
	return strings.Fields(raw), nil
}

// listPods returns pod names in the given namespace (or all namespaces).
// When allNamespaces is true, pod names are prefixed with "namespace/" so the
// caller can determine which namespace each pod belongs to.
func listPods(namespace string, allNamespaces bool) ([]string, error) {
	var kubectlArgs []string
	if allNamespaces {
		kubectlArgs = []string{
			"get", "pods", "--all-namespaces",
			"-o", `jsonpath={range .items[*]}{.metadata.namespace}{"/"}{.metadata.name}{"\n"}{end}`,
		}
	} else {
		kubectlArgs = []string{
			"get", "pods", "-n", namespace,
			"-o", `jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}`,
		}
	}

	out, err := runKubectlCmd(kubectlArgs...)
	if err != nil {
		return nil, err
	}

	var pods []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if p := strings.TrimSpace(line); p != "" {
			pods = append(pods, p)
		}
	}
	if len(pods) == 0 {
		ns := namespace
		if allNamespaces {
			ns = "all namespaces"
		}
		return nil, fmt.Errorf("no pods found in %s", ns)
	}
	return pods, nil
}

// findMatchingPods filters pods whose name contains pattern as a regex.
// Falls back to plain substring matching when pattern is not valid regex.
func findMatchingPods(pods []string, pattern string) ([]string, error) {
	re, reErr := regexp.Compile(pattern)

	var matched []string
	for _, p := range pods {
		if reErr != nil {
			// Plain substring fallback.
			if strings.Contains(p, pattern) {
				matched = append(matched, p)
			}
		} else {
			if re.MatchString(p) {
				matched = append(matched, p)
			}
		}
	}
	return matched, nil
}

// streamPodLogsToLogrun runs `kubectl logs` for one pod and feeds all output
// into the shared LogBatcher under the given processID / commandID.
// The context is used to cancel the subprocess when the user interrupts.
func streamPodLogsToLogrun(ctx context.Context, processID, commandID, namespace, podName, container string) error {
	cmdArgs := buildKubectlLogsArgs(namespace, podName, container)
	fmt.Printf("  → kubectl %s\n", strings.Join(cmdArgs, " "))

	c := exec.CommandContext(ctx, "kubectl", cmdArgs...)
	stdout, err := c.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stdoutErr, err := c.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := c.Start(); err != nil {
		return fmt.Errorf("start kubectl logs: %w", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	streamReader := func(r io.Reader, stream string) {
		defer wg.Done()
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if kubectlFlags.follow {
				fmt.Printf("[%s] %s\n", podName, line)
			}
			runner.batcher.Add(LogEntry{
				ProcessID: processID,
				CommandID: commandID,
				Timestamp: time.Now().UTC(),
				Stream:    stream,
				Message:   fmt.Sprintf("[%s] %s", podName, line),
			})
		}
	}

	go streamReader(stdout, "stdout")
	go streamReader(stdoutErr, "stderr")
	wg.Wait()

	exitErr := c.Wait()

	// Determine final status: cancelled takes priority over other errors.
	if ctx.Err() != nil {
		_ = runner.updateCommandStatus(processID, commandID, "cancelled", -1, time.Now())
		return nil
	}

	exitCode := 0
	if exitErr != nil {
		if ee, ok := exitErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			exitCode = 1
		}
	}

	status := "completed"
	if exitCode != 0 {
		status = "failed"
	}
	_ = runner.updateCommandStatus(processID, commandID, status, exitCode, time.Now())
	WaitForQueueDrain(5 * time.Second)
	return nil
}

// buildKubectlLogsArgs assembles the argument slice for `kubectl logs`.
func buildKubectlLogsArgs(namespace, podName, container string) []string {
	args := []string{"logs"}

	// When fetching across all namespaces, pod names are "namespace/pod".
	if strings.Contains(podName, "/") {
		parts := strings.SplitN(podName, "/", 2)
		args = append(args, parts[1], "-n", parts[0])
	} else {
		args = append(args, podName, "-n", namespace)
	}

	if container != "" {
		args = append(args, "-c", container)
	}
	if kubectlFlags.follow {
		args = append(args, "-f")
	}
	if kubectlFlags.tail >= 0 {
		args = append(args, "--tail", fmt.Sprintf("%d", kubectlFlags.tail))
	}
	if kubectlFlags.since != "" {
		args = append(args, "--since", kubectlFlags.since)
	}
	return args
}

// kubectlLogsCmdStr returns a human-readable command string for a given pod.
func kubectlLogsCmdStr(namespace, podName, container string) string {
	args := buildKubectlLogsArgs(namespace, podName, container)
	return "kubectl " + strings.Join(args, " ")
}

// runKubectlCmd runs kubectl with the given args and returns stdout output.
func runKubectlCmd(args ...string) (string, error) {
	c := exec.Command("kubectl", args...)
	out, err := c.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("kubectl %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("kubectl %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// ── Interactive prompts ──────────────────────────────────────────────────────

// promptSelectNamespace shows an interactive list of namespaces for the user to choose from.
func promptSelectNamespace(namespaces []string) (string, error) {
	prompt := promptui.Select{
		Label: "Select namespace",
		Items: namespaces,
		Size:  10,
		Searcher: func(input string, idx int) bool {
			return strings.Contains(strings.ToLower(namespaces[idx]), strings.ToLower(input))
		},
	}
	_, result, err := prompt.Run()
	if err != nil {
		return "", fmt.Errorf("namespace selection cancelled: %w", err)
	}
	return result, nil
}

// promptPodPattern lists available pods and prompts the user to enter a pattern.
func promptPodPattern(pods []string) (string, error) {
	fmt.Println("\nAvailable pods:")
	for i, p := range pods {
		fmt.Printf("  %3d. %s\n", i+1, p)
	}
	fmt.Println()

	prompt := promptui.Prompt{
		Label: "Enter pod name or pattern (substring / regex)",
		Validate: func(input string) error {
			if strings.TrimSpace(input) == "" {
				return fmt.Errorf("pattern cannot be empty")
			}
			return nil
		},
	}
	result, err := prompt.Run()
	if err != nil {
		return "", fmt.Errorf("pod pattern input cancelled: %w", err)
	}
	return strings.TrimSpace(result), nil
}

// confirmAction represents the user's choice at the confirmation prompt.
type confirmAction int

const (
	confirmActionFetch   confirmAction = iota // proceed with fetching
	confirmActionAddMore                      // add another pattern
	confirmActionCancel                       // abort
)

// promptConfirmPods shows the accumulated pod list and asks what to do next.
func promptConfirmPods(pods []string) (confirmAction, error) {
	fmt.Printf("\nSelected %d pod(s):\n", len(pods))
	for _, p := range pods {
		fmt.Printf("  • %s\n", p)
	}
	fmt.Println()

	prompt := promptui.Select{
		Label: "What would you like to do?",
		Items: []string{
			"Yes, fetch logs from these pods",
			"Add more pods (enter another pattern)",
			"No, cancel",
		},
	}
	idx, _, err := prompt.Run()
	if err != nil {
		return confirmActionCancel, fmt.Errorf("confirmation cancelled: %w", err)
	}
	switch idx {
	case 0:
		return confirmActionFetch, nil
	case 1:
		return confirmActionAddMore, nil
	default:
		return confirmActionCancel, nil
	}
}

// ── Utility helpers ──────────────────────────────────────────────────────────

// parseKubectlTags splits a comma-separated tag string into a slice.
func parseKubectlTags(raw string) []string {
	if raw == "" {
		return []string{}
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// appendUnique appends values to slice only if they are not already present.
func appendUnique(slice []string, values ...string) []string {
	set := make(map[string]struct{}, len(slice))
	for _, v := range slice {
		set[v] = struct{}{}
	}
	for _, v := range values {
		if v == "" {
			continue
		}
		if _, exists := set[v]; !exists {
			slice = append(slice, v)
			set[v] = struct{}{}
		}
	}
	return slice
}

// matchPodsFromPatterns applies each pattern in turn and returns the union of
// all matching pod names, preserving order and deduplicating.
func matchPodsFromPatterns(allPods, patterns []string) ([]string, error) {
	var result []string
	seen := make(map[string]struct{})
	for _, p := range patterns {
		matches, err := findMatchingPods(allPods, p)
		if err != nil {
			return nil, fmt.Errorf("invalid pattern %q: %w", p, err)
		}
		for _, m := range matches {
			if _, ok := seen[m]; !ok {
				seen[m] = struct{}{}
				result = append(result, m)
			}
		}
	}
	return result, nil
}

// unionPods merges extra into base, skipping duplicates, preserving order.
func unionPods(base, extra []string) []string {
	seen := make(map[string]struct{}, len(base))
	for _, p := range base {
		seen[p] = struct{}{}
	}
	for _, p := range extra {
		if _, ok := seen[p]; !ok {
			seen[p] = struct{}{}
			base = append(base, p)
		}
	}
	return base
}
