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

	// ── Step 0: Optional cluster context switch (interactive only) ──────────
	if kubectlFlags.namespace == "" && !kubectlFlags.allNamespaces {
		if current, err := getCurrentContext(); err == nil {
			if contexts, err := listContexts(); err == nil && len(contexts) > 1 {
				chosen, ctxErr := promptSwitchContext(current, contexts)
				if ctxErr != nil {
					return ctxErr
				}
				if chosen != current {
					fmt.Printf("Switching context to %s…\n", chosen)
					if err := switchContext(chosen); err != nil {
						return fmt.Errorf("failed to switch context: %w", err)
					}
					fmt.Printf("  ✓ Now using context: %s\n", chosen)
				}
			}
		}
	}

	// ── Step 1: Resolve namespace ────────────────────────────────────────────
	ns := kubectlFlags.namespace
	if ns == "" && !kubectlFlags.allNamespaces {
		fmt.Print("Listing namespaces…")
		namespaces, err := listNamespaces()
		if err != nil {
			fmt.Println()
			return fmt.Errorf("failed to list namespaces: %w", err)
		}
		fmt.Println()
		ns, err = promptSelectNamespace(namespaces)
		if err != nil {
			return err
		}
	}

	// ── Step 2: List all available pods (needed for both flows) ─────────────
	fmt.Printf("Listing pods in %s…", func() string {
		if kubectlFlags.allNamespaces {
			return "all namespaces"
		}
		return ns
	}())
	allPods, err := listPods(ns, kubectlFlags.allNamespaces)
	if err != nil {
		fmt.Println()
		return fmt.Errorf("failed to list pods: %w", err)
	}
	fmt.Println()

	// ── Step 3: Resolve initial pod selection ────────────────────────────────
	// --pod accepts comma-separated patterns; interactive mode shows a
	// searchable multi-select list (type to filter, enter to toggle).
	var initialPatterns []string
	if kubectlFlags.pod != "" {
		for _, p := range strings.Split(kubectlFlags.pod, ",") {
			if t := strings.TrimSpace(p); t != "" {
				initialPatterns = append(initialPatterns, t)
			}
		}
	} else {
		selected, promptErr := promptMultiSelectPods(allPods, nil)
		if promptErr != nil {
			return promptErr
		}
		if len(selected) == 0 {
			fmt.Println("No pods selected — exiting.")
			return nil
		}
		initialPatterns = selected
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
				// Re-open the multi-select picker with current selection pre-checked.
				// "Add from another namespace" is available inside the picker itself.
				extra, selErr := promptMultiSelectPods(allPods, matched)
				if selErr != nil {
					return selErr
				}
				if len(extra) == 0 {
					fmt.Println("No additional pods selected.")
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
// It applies a 20-second timeout so a slow/unreachable API server doesn't
// block indefinitely.
func runKubectlCmd(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "kubectl", args...)
	out, err := c.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("kubectl %s: timed out after 20s — is the cluster reachable?", strings.Join(args, " "))
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("kubectl %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("kubectl %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// ── Interactive prompts ──────────────────────────────────────────────────────

// listContexts returns all kubectl context names from the kubeconfig.
func listContexts() ([]string, error) {
	out, err := runKubectlCmd("config", "get-contexts", "-o", "name")
	if err != nil {
		return nil, err
	}
	var ctxs []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if c := strings.TrimSpace(line); c != "" {
			ctxs = append(ctxs, c)
		}
	}
	return ctxs, nil
}

// getCurrentContext returns the currently active kubectl context name.
func getCurrentContext() (string, error) {
	out, err := runKubectlCmd("config", "current-context")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// switchContext runs `kubectl config use-context <name>`.
func switchContext(name string) error {
	_, err := runKubectlCmd("config", "use-context", name)
	return err
}

// promptSwitchContext asks the user whether they want to switch kubectl context.
// It returns the chosen context name (may be the same as current).
func promptSwitchContext(current string, contexts []string) (string, error) {
	keepLabel := fmt.Sprintf("Continue with current context (%s)", current)
	items := []string{keepLabel}
	items = append(items, contexts...)

	prompt := promptui.Select{
		Label: "Kubectl context (cluster)",
		Items: items,
		Size:  12,
		Searcher: func(input string, idx int) bool {
			return strings.Contains(strings.ToLower(items[idx]), strings.ToLower(input))
		},
	}
	idx, _, err := prompt.Run()
	if err != nil {
		if err == promptui.ErrInterrupt {
			os.Exit(130) // Ctrl+C — exit like a normal signal interrupt
		}
		if err == promptui.ErrEOF {
			return current, nil // Escape — stay on current context
		}
		return "", fmt.Errorf("context selection cancelled: %w", err)
	}
	if idx == 0 {
		return current, nil // user chose to keep current
	}
	return contexts[idx-1], nil
}

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
		if err == promptui.ErrInterrupt {
			os.Exit(130)
		}
		return "", fmt.Errorf("namespace selection cancelled")
	}
	return result, nil
}

// promptMultiSelectPods shows an interactive, toggleable pod list with
// persistent filter, "Select all matching", and inline "add from another
// namespace" — all within a single promptui.Select loop.
//
// Sentinel items at the top of the list:
//   ✓  Done  (N selected)          — confirm and return
//   ✗  Cancel                      — abort (returns nil, nil)
//   ⊕  Select all (M pods)         — when no filter: select all visible pods
//   ⊕  Select all matching "x" (M) — when filter active: select all filtered pods
//   ✕  Clear filter: "x"           — clear the active filter
//   📁  Add pods from another namespace
//
// Filter persistence: the Searcher closure captures the last typed input; after
// each toggle the filter is preserved so the user does not have to retype.
// The user can clear the filter explicitly via the "✕ Clear filter" sentinel or
// by backspacing the search input until it is empty.
func promptMultiSelectPods(allPods []string, alreadySelected []string) ([]string, error) {
	// selected covers both allPods and any externalPods added inline.
	selected := make(map[string]bool, len(alreadySelected))
	for _, p := range alreadySelected {
		selected[p] = true
	}

	// externalPods accumulates pods from other namespaces added inline.
	var externalPods []string

	// currentFilter is the persistent filter string (survives across toggles).
	var currentFilter string

	const (
		donePrefix   = "✓  Done"
		cancelLabel  = "✗  Cancel"
		selectAllPfx = "⊕  Select all"
		clearFiltPfx = "✕  Clear filter:"
		addOtherNS   = "📁  Add pods from another namespace"
	)

	for {
		// Build the full ordered pod list for this iteration.
		displayPods := make([]string, 0, len(allPods)+len(externalPods))
		displayPods = append(displayPods, allPods...)
		displayPods = append(displayPods, externalPods...)

		// Apply persistent filter to compute the visible pod subset.
		visiblePods := displayPods
		if currentFilter != "" {
			visiblePods = nil
			for _, p := range displayPods {
				if strings.Contains(strings.ToLower(p), strings.ToLower(currentFilter)) {
					visiblePods = append(visiblePods, p)
				}
			}
		}

		nSel := countSelected(selected)
		nVis := len(visiblePods)

		// Build item list: sentinels first, then pods.
		var items []string
		items = append(items, fmt.Sprintf("%s  (%d selected)", donePrefix, nSel))
		items = append(items, cancelLabel)
		if currentFilter != "" {
			items = append(items, fmt.Sprintf("%s \"%s\"  (%d pods)", selectAllPfx+" matching", currentFilter, nVis))
			items = append(items, fmt.Sprintf("%s \"%s\"", clearFiltPfx, currentFilter))
		} else {
			items = append(items, fmt.Sprintf("%s  (%d pods)", selectAllPfx, nVis))
		}
		items = append(items, addOtherNS)
		nSentinels := len(items)

		for _, p := range visiblePods {
			if selected[p] {
				items = append(items, "[x] "+p)
			} else {
				items = append(items, "[ ] "+p)
			}
		}

		// searcherCalled / lastSearch track filter persistence.
		searcherCalled := false
		lastSearch := ""

		label := fmt.Sprintf("Select pods  (%d selected)", nSel)
		if currentFilter != "" {
			label = fmt.Sprintf("Select pods  [filter: %q]  (%d/%d shown, %d selected)",
				currentFilter, nVis, len(displayPods), nSel)
		}

		prompt := promptui.Select{
			Label: label,
			Items: items,
			Size:  15,
			Searcher: func(input string, idx int) bool {
				searcherCalled = true
				lastSearch = input
				if idx < nSentinels {
					return true // always show sentinel controls
				}
				pod := visiblePods[idx-nSentinels]
				return strings.Contains(strings.ToLower(pod), strings.ToLower(input))
			},
		}

		idx, choice, err := prompt.Run()
		if err != nil {
			if err == promptui.ErrInterrupt {
				os.Exit(130) // Ctrl+C — kill the process
			}
			if err == promptui.ErrEOF {
				// Escape while typing — exit search mode, re-render.
				continue
			}
			return nil, fmt.Errorf("pod selection error: %w", err)
		}

		// Persist (or clear) filter based on what the user typed this round.
		if searcherCalled {
			currentFilter = lastSearch
		}

		switch {
		case idx == 0 || strings.HasPrefix(choice, donePrefix):
			// Collect selected pods in display order.
			var result []string
			for _, p := range displayPods {
				if selected[p] {
					result = append(result, p)
				}
			}
			// Preserve any alreadySelected pods not in displayPods.
			for _, p := range alreadySelected {
				inResult := false
				for _, r := range result {
					if r == p {
						inResult = true
						break
					}
				}
				if !inResult {
					result = append(result, p)
				}
			}
			return result, nil

		case idx == 1 || choice == cancelLabel:
			return nil, nil // deliberate cancel, not an error

		case strings.HasPrefix(choice, selectAllPfx):
			// Determine which pods to actually select.
			// If the user was actively typing a search this iteration, honour
			// that search term — the items list was built before they typed so
			// the label count may lag, but the selection must match what they see.
			effectiveFilter := currentFilter
			if searcherCalled && lastSearch != "" {
				effectiveFilter = lastSearch
			}
			if effectiveFilter != "" {
				for _, p := range displayPods {
					if strings.Contains(strings.ToLower(p), strings.ToLower(effectiveFilter)) {
						selected[p] = true
					}
				}
			} else {
				for _, p := range visiblePods {
					selected[p] = true
				}
			}
			currentFilter = "" // clear filter after select-all so full list is shown

		case strings.HasPrefix(choice, clearFiltPfx):
			currentFilter = ""

		case choice == addOtherNS:
			extra, addErr := interactiveAddFromOtherNamespace(nil)
			if addErr != nil {
				fmt.Fprintf(os.Stderr, "warning: %v\n", addErr)
				continue
			}
			for _, p := range extra {
				if !containsString(externalPods, p) {
					externalPods = append(externalPods, p)
				}
				selected[p] = true // auto-select newly added pods
			}

		default:
			if idx >= nSentinels {
				pod := visiblePods[idx-nSentinels]
				selected[pod] = !selected[pod]
			}
		}
	}
}

// containsString reports whether s appears in slice.
func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// countSelected returns the number of true values in a bool map.
func countSelected(m map[string]bool) int {
	n := 0
	for _, v := range m {
		if v {
			n++
		}
	}
	return n
}

// confirmAction represents the user's choice at the confirmation prompt.
type confirmAction int

const (
	confirmActionFetch  confirmAction = iota // proceed with fetching
	confirmActionAddMore                     // add more pods (same namespace)
	confirmActionCancel                      // abort
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
			"Fetch logs from these pods",
			"Add / change pods",
			"Cancel",
		},
	}
	idx, _, err := prompt.Run()
	if err != nil {
		if err == promptui.ErrInterrupt {
			os.Exit(130)
		}
		return confirmActionCancel, nil
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

// interactiveAddFromOtherNamespace guides the user through an optional context
// switch → namespace selection → pod multi-select and returns the chosen pods,
// each prefixed with "namespace/" so buildKubectlLogsArgs resolves them correctly.
func interactiveAddFromOtherNamespace(alreadyMatched []string) ([]string, error) {
	// Ask whether the user wants to switch context first (opt-in).
	current, _ := getCurrentContext()
	currentLabel := current
	if currentLabel == "" {
		currentLabel = "current"
	}

	ctxPrompt := promptui.Select{
		Label: "Switch cluster/context first?",
		Items: []string{
			fmt.Sprintf("No, use current context (%s)", currentLabel),
			"Yes, switch context",
		},
	}
	ctxIdx, _, ctxErr := ctxPrompt.Run()
	if ctxErr != nil {
		if ctxErr == promptui.ErrInterrupt {
			os.Exit(130)
		}
		return nil, nil // Escape — bail out of this sub-flow gracefully
	}

	if ctxIdx == 1 {
		contexts, err := listContexts()
		if err != nil || len(contexts) == 0 {
			fmt.Println("Could not list contexts — continuing with current context.")
		} else {
			chosen, switchErr := promptSwitchContext(currentLabel, contexts)
			if switchErr != nil {
				return nil, switchErr
			}
			if chosen != current {
				fmt.Printf("Switching context to %s…\n", chosen)
				if err := switchContext(chosen); err != nil {
					return nil, fmt.Errorf("failed to switch context: %w", err)
				}
				fmt.Printf("  ✓ Now using context: %s\n", chosen)
			}
		}
	}

	// Namespace selection.
	fmt.Print("Listing namespaces…")
	namespaces, err := listNamespaces()
	if err != nil {
		fmt.Println()
		return nil, fmt.Errorf("failed to list namespaces: %w", err)
	}
	fmt.Println()
	newNS, err := promptSelectNamespace(namespaces)
	if err != nil {
		return nil, err
	}

	// List pods in the chosen namespace.
	fmt.Printf("Listing pods in %s…", newNS)
	nsPods, err := listPods(newNS, false)
	if err != nil {
		fmt.Println()
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}
	fmt.Println()

	// Build a prefixed pod list so the caller can tell which namespace they belong to.
	prefixed := make([]string, len(nsPods))
	for i, p := range nsPods {
		prefixed[i] = newNS + "/" + p
	}

	// Multi-select on the prefixed list, with already-matched pods pre-checked.
	selected, err := promptMultiSelectPods(prefixed, alreadyMatched)
	if err != nil {
		return nil, err
	}
	return selected, nil
}

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
