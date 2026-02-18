package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// CommandInfo represents a single command within a process
type CommandInfo struct {
	CommandID string     `json:"command_id"`
	Command   string     `json:"command"`
	PID       int        `json:"pid"`
	Status    string     `json:"status"`
	ExitCode  *int       `json:"exit_code,omitempty"`
	StartTime time.Time  `json:"start_time"`
	EndTime   *time.Time `json:"end_time,omitempty"`
}

// ProcessMetadata represents process information with multiple commands
type ProcessMetadata struct {
	ProcessID string        `json:"process_id"`
	Name      string        `json:"name"`
	Commands  []CommandInfo `json:"commands"`
	Status    string        `json:"status"`
	StartTime time.Time     `json:"start_time"`
	EndTime   *time.Time    `json:"end_time,omitempty"`
	Tags      []string      `json:"tags"`
	CWD       string        `json:"cwd"`
}

// LogEntry represents a single log line
type LogEntry struct {
	ProcessID string    `json:"process_id"`
	CommandID string    `json:"command_id"`
	Timestamp time.Time `json:"timestamp"`
	Stream    string    `json:"stream"`
	Message   string    `json:"message"`
}

// LogBatch represents a batch of log entries
type LogBatch struct {
	ProcessID string     `json:"process_id"`
	Logs      []LogEntry `json:"logs"`
}

// LogBatcher handles batched log sending
type LogBatcher struct {
	buffer       chan LogEntry
	batchSize    int
	flushTimer   *time.Timer
	processID    string
	httpClient   *http.Client
	apiBaseURL   string
	shutdown     chan struct{}
	wg           sync.WaitGroup
	flushOnClose bool
}

// BatchConfig holds batching configuration
type BatchConfig struct {
	MaxBatchSize  int
	FlushInterval time.Duration
	BufferSize    int
}

// LogRunner handles command execution and logging
type LogRunner struct {
	apiBaseURL string
	httpClient *http.Client
	logsDir    string
	logMutex   sync.Mutex
	batcher    *LogBatcher
	batchConfig BatchConfig
}

var (
	name       string
	tags       string
	cwd        string
	env        []string
	detach     bool
	follow     bool
	apiURL     string
	runner     *LogRunner
)

func main() {
	var rootCmd = &cobra.Command{
		Use:   "logrun [command] [args...]",
		Short: "Wrap any shell command with comprehensive logging",
		Long: `LogRun wraps arbitrary shell commands and captures stdout, stderr, 
exit codes, and timestamps. Provides a web interface for log viewing and search.`,
		RunE: runCommand,
	}

	rootCmd.Flags().StringVar(&name, "name", "", "Friendly process name")
	rootCmd.Flags().StringVar(&tags, "tags", "", "Comma-separated tags")
	rootCmd.Flags().StringVar(&cwd, "cwd", "", "Run command in directory")
	rootCmd.Flags().StringArrayVar(&env, "env", []string{}, "Inject environment variables (KEY=VALUE)")
	rootCmd.Flags().BoolVar(&detach, "detach", false, "Run in background")
	rootCmd.Flags().BoolVar(&follow, "follow", true, "Stream logs live to stdout")
	rootCmd.Flags().StringVar(&apiURL, "api-url", "http://localhost:3001", "LogRun API server URL")

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func init() {
	var err error
	runner, err = NewLogRunner("http://localhost:3001") // Default API URL
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize LogRunner: %v\n", err)
		os.Exit(1)
	}
}

func NewLogRunner(baseURL string) (*LogRunner, error) {
	// Get current working directory
	currentDir, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to get working directory: %w", err)
	}

	// Set up logs directory (for local backup)
	logsDir := filepath.Join(currentDir, "logs")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create logs directory: %w", err)
	}

	// Create HTTP client with timeouts suitable for log streaming
	httpClient := &http.Client{
		Timeout: 30 * time.Second, // Increased timeout for log streaming
		Transport: &http.Transport{
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
		},
	}

	return &LogRunner{
		apiBaseURL: baseURL,
		httpClient: httpClient,
		logsDir:    logsDir,
		batchConfig: BatchConfig{
			MaxBatchSize:  50,
			FlushInterval: 1 * time.Second,
			BufferSize:    1000,
		},
	}, nil
}

func runCommand(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("no command specified")
	}

	// Update API URL from flag if provided
	if apiURL != "" {
		runner.apiBaseURL = apiURL
	}

	processID := uuid.New().String()
	
	// Parse tags
	var tagList []string
	if tags != "" {
		tagList = strings.Split(tags, ",")
		for i, tag := range tagList {
			tagList[i] = strings.TrimSpace(tag)
		}
	}

	// Determine working directory
	workDir := cwd
	if workDir == "" {
		var err error
		workDir, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get working directory: %w", err)
		}
	}

	// Create command string
	cmdStr := strings.Join(args, " ")
	processName := name
	if processName == "" {
		processName = args[0]
	}

	// Check if process with same name exists
	existingProcess, err := runner.getProcessByName(processName)

	var commandID string
	var metadata *ProcessMetadata

	if err == nil && existingProcess != nil && existingProcess.Status == "running" {
		// Process exists and is running, add to existing
		processID = existingProcess.ProcessID
		commandID = uuid.New().String()
		metadata = existingProcess

		// Add new command to existing process
		newCommand := CommandInfo{
			CommandID: commandID,
			Command:   cmdStr,
			PID:       0, // Will be set after command starts
			Status:    "running",
			StartTime: time.Now(),
		}
		metadata.Commands = append(metadata.Commands, newCommand)

		fmt.Printf("Adding command to existing process %s (name: %s)\n", processID, processName)
	} else {
		// Create new process
		processID = uuid.New().String()
		commandID = uuid.New().String()
		metadata = &ProcessMetadata{
			ProcessID: processID,
			Name:      processName,
			Commands: []CommandInfo{
				{
					CommandID: commandID,
					Command:   cmdStr,
					Status:    "running",
					StartTime: time.Now(),
				},
			},
			Status:    "running",
			StartTime: time.Now(),
			Tags:      tagList,
			CWD:       workDir,
		}

		fmt.Printf("Creating new process %s (name: %s)\n", processID, processName)
	}

	// Send initial metadata to API
	if err := runner.createProcess(metadata); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Failed to register process with API: %v\n", err)
		// Continue anyway - we can still run the command and log locally
	}

	// Create log file (local backup)
	logFile, err := os.Create(filepath.Join(runner.logsDir, processID+".log"))
	if err != nil {
		return fmt.Errorf("failed to create log file: %w", err)
	}
	defer logFile.Close()

	// Prepare command
	execCmd := exec.Command(args[0], args[1:]...)
	execCmd.Dir = workDir

	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Flag to track if process was cancelled
	var cancelled bool
	var cancelMutex sync.Mutex

	// Handle signals in a separate goroutine
	go func() {
		<-sigChan
		cancelMutex.Lock()
		cancelled = true
		cancelMutex.Unlock()
		
		// Use atomic command update for cancellation
		if err := runner.updateCommandStatus(processID, commandID, "cancelled", -1, time.Now()); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: Failed to update command cancellation: %v\n", err)
		}
		
		// Terminate the child process
		if execCmd.Process != nil {
			execCmd.Process.Kill()
		}
		
		fmt.Printf("\nCommand %s cancelled by user\n", commandID)
		os.Exit(130) // Standard exit code for Ctrl+C
	}()

	// Set up environment
	execCmd.Env = os.Environ()
	for _, envVar := range env {
		execCmd.Env = append(execCmd.Env, envVar)
	}

	// Create pipes for stdout and stderr
	stdout, err := execCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := execCmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	// Start the command
	if err := execCmd.Start(); err != nil {
		// Update the specific command status
		for i := range metadata.Commands {
			if metadata.Commands[i].CommandID == commandID {
				metadata.Commands[i].Status = "failed"
				metadata.Commands[i].ExitCode = new(int)
				*metadata.Commands[i].ExitCode = 1
				endTime := time.Now()
				metadata.Commands[i].EndTime = &endTime
				break
			}
		}
		// Update overall process status if all commands failed
		allFailed := true
		for _, cmd := range metadata.Commands {
			if cmd.Status == "running" {
				allFailed = false
				break
			}
		}
		if allFailed {
			metadata.Status = "failed"
			endTime := time.Now()
			metadata.EndTime = &endTime
		}
		runner.updateProcess(metadata)
		return fmt.Errorf("failed to start command: %w", err)
	}

	// Update the specific command with PID
	for i := range metadata.Commands {
		if metadata.Commands[i].CommandID == commandID {
			metadata.Commands[i].PID = execCmd.Process.Pid
			break
		}
	}
	if err := runner.updateProcess(metadata); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Failed to update process with API: %v\n", err)
	}

	fmt.Printf("Started command %s in process %s (PID: %d)\n", commandID, processID, execCmd.Process.Pid)

	// Initialize log batcher for efficient batch processing
	runner.batcher = NewLogBatcher(processID, runner.apiBaseURL, runner.httpClient, runner.batchConfig)
	runner.batcher.Start()
	defer runner.batcher.Close()

	// Handle log streaming
	var wg sync.WaitGroup
	wg.Add(2)

	// Stream stdout
	go func() {
		defer wg.Done()
		runner.streamLogs(stdout, "stdout", processID, commandID, logFile)
	}()

	// Stream stderr
	go func() {
		defer wg.Done()
		runner.streamLogs(stderr, "stderr", processID, commandID, logFile)
	}()

	// Wait for command to complete first
	err = execCmd.Wait()
	
	// Then wait for streaming to complete (pipes will be closed after command ends)
	wg.Wait()
	
	// Check if process was cancelled via signal
	cancelMutex.Lock()
	wasCancelled := cancelled
	cancelMutex.Unlock()
	
	if wasCancelled {
		// Process was already handled by signal handler
		return nil
	}
	
	endTime := time.Now()

	// Update the specific command status atomically 
	exitCode := 0
	status := "completed"
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			if exitStatus, ok := exitError.Sys().(syscall.WaitStatus); ok {
				exitCode = exitStatus.ExitStatus()
			}
		}
		status = "failed"
	}

	// Use atomic command update instead of full process update
	if updateErr := runner.updateCommandStatus(processID, commandID, status, exitCode, endTime); updateErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: Failed to update command status: %v\n", updateErr)
	}

	fmt.Printf("Command %s in process %s completed with exit code %d\n", commandID, processID, exitCode)
	return nil
}

func (lr *LogRunner) streamLogs(reader io.Reader, stream, processID, commandID string, logFile *os.File) {
	scanner := bufio.NewScanner(reader)
	var currentEntry *LogEntry
	var entryBuilder strings.Builder
	var lastLineTime time.Time
	
	flushEntry := func() {
		if currentEntry != nil {
			currentEntry.Message = entryBuilder.String()
			
			// Write to local log file (backup) - thread-safe
			entryJSON, err := json.Marshal(*currentEntry)
			if err == nil {
				entryJSON = append(entryJSON, '\n')
				lr.logMutex.Lock()
				logFile.Write(entryJSON)
				lr.logMutex.Unlock()
			}

			// Add to batcher instead of individual send
			if lr.batcher != nil {
				lr.batcher.Add(*currentEntry)
			} else {
				// Fallback to individual send if batcher not initialized
				go lr.sendLogEntry(currentEntry)
			}

			// Stream to stdout if following
			if follow && !detach {
				prefix := "[stdout]"
				if stream == "stderr" {
					prefix = "[stderr]"
				}
				fmt.Printf("%s %s\n", prefix, currentEntry.Message)
			}
		}
	}
	
	// Set up a ticker to flush incomplete entries after 2 seconds
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	
	// Channel to signal when scanning is done
	done := make(chan bool)
	
	// Goroutine to handle timeout-based flushing
	go func() {
		for {
			select {
			case <-ticker.C:
				// If we have a current entry and it's been more than 2 seconds since the last line
				if currentEntry != nil && time.Since(lastLineTime) > 2*time.Second {
					flushEntry()
					currentEntry = nil
					entryBuilder.Reset()
				}
			case <-done:
				return
			}
		}
	}()
	
	for scanner.Scan() {
		line := scanner.Text()
		lastLineTime = time.Now()
		
		// Check if this line starts a new log entry or continues the current one
		isNewEntry := lr.isNewLogEntry(line)
		
		if isNewEntry && currentEntry != nil {
			// Flush the previous entry
			flushEntry()
			entryBuilder.Reset()
		}
		
		if isNewEntry {
			// Start a new entry
			currentEntry = &LogEntry{
				ProcessID: processID,
				CommandID: commandID,
				Timestamp: time.Now(),
				Stream:    stream,
				Message:   "", // Will be set when flushing
			}
			entryBuilder.WriteString(line)
		} else if currentEntry != nil {
			// Continue the current entry
			entryBuilder.WriteString("\n")
			entryBuilder.WriteString(line)
		} else {
			// No current entry, treat this as a new entry anyway
			currentEntry = &LogEntry{
				ProcessID: processID,
				CommandID: commandID,
				Timestamp: time.Now(),
				Stream:    stream,
				Message:   "",
			}
			entryBuilder.WriteString(line)
		}
	}
	
	// Signal the timeout goroutine to stop
	close(done)
	
	// Flush any remaining entry
	flushEntry()
	
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "Error reading %s: %v\n", stream, err)
	}
}

// isNewLogEntry determines if a line starts a new log entry
func (lr *LogRunner) isNewLogEntry(line string) bool {
	// Trim leading whitespace to check the actual content
	trimmed := strings.TrimSpace(line)
	
	// Empty lines continue the current entry
	if trimmed == "" {
		return false
	}
	
	// Lines starting with whitespace/indentation are likely continuations
	if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
		return false
	}
	
	// Common continuation patterns
	continuationPatterns := []string{
		"at ",           // Stack traces
		"Caused by:",    // Java stack traces
		"... ",          // Truncated stack traces
		"}",             // Closing braces
		"]",             // Closing brackets
		"errno:",        // Error details
		"code:",         // Error codes
		"syscall:",      // System call info
		"hostname:",     // Network error details
	}
	
	for _, pattern := range continuationPatterns {
		if strings.HasPrefix(trimmed, pattern) {
			return false
		}
	}
	
	// Lines that contain only punctuation/brackets (likely continuations)
	if len(trimmed) < 3 && (strings.Contains(trimmed, "{") || strings.Contains(trimmed, "}") || strings.Contains(trimmed, ",")) {
		return false
	}
	
	// Check for ISO 8601 timestamp at the start (like your example)
	if len(trimmed) >= 10 {
		// Check for YYYY-MM-DD pattern or YYYY format
		if trimmed[4] == '-' && (trimmed[7] == '-' || trimmed[10] == 'T') {
			return true
		}
	}
	
	// Check for bracketed log prefixes like [HOOTHOOT] or [ERROR]
	if strings.HasPrefix(trimmed, "[") {
		closeBracket := strings.Index(trimmed, "]")
		if closeBracket > 1 && closeBracket < 20 { // Reasonable bracket length
			return true
		}
	}
	
	// Lines starting with common log levels
	logLevels := []string{"ERROR", "WARN", "INFO", "DEBUG", "TRACE", "FATAL"}
	words := strings.Fields(trimmed)
	if len(words) > 0 {
		firstWord := strings.ToUpper(words[0])
		for _, level := range logLevels {
			if firstWord == level || strings.HasSuffix(firstWord, level) {
				return true
			}
		}
		
		// Check if any word in the first few words contains a log level
		for i := 0; i < len(words) && i < 3; i++ {
			word := strings.ToUpper(words[i])
			for _, level := range logLevels {
				if strings.Contains(word, level) {
					return true
				}
			}
		}
	}
	
	// Check for common application prefixes
	appPrefixes := []string{
		"preprod-",
		"prod-", 
		"dev-",
		"staging-",
	}
	
	for _, prefix := range appPrefixes {
		if strings.Contains(strings.ToLower(line), prefix) {
			return true
		}
	}
	
	// Default: if no clear indication, treat as new entry
	// This handles cases where logs don't have clear patterns
	return true
}

// HTTP API methods
func (lr *LogRunner) getProcessByName(name string) (*ProcessMetadata, error) {
	resp, err := lr.httpClient.Get(
		fmt.Sprintf("%s/processes/by-name/%s", lr.apiBaseURL, name),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, nil // Process not found
	}

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var process ProcessMetadata
	if err := json.NewDecoder(resp.Body).Decode(&process); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &process, nil
}

func (lr *LogRunner) createProcess(metadata *ProcessMetadata) error {
	jsonData, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("failed to marshal process metadata: %w", err)
	}

	resp, err := lr.httpClient.Post(
		fmt.Sprintf("%s/processes", lr.apiBaseURL),
		"application/json",
		bytes.NewBuffer(jsonData),
	)
	if err != nil {
		return fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

func (lr *LogRunner) updateProcess(metadata *ProcessMetadata) error {
	jsonData, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("failed to marshal process metadata: %w", err)
	}

	req, err := http.NewRequest(
		"PUT",
		fmt.Sprintf("%s/processes/%s", lr.apiBaseURL, metadata.ProcessID),
		bytes.NewBuffer(jsonData),
	)
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := lr.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// Atomic command status update to prevent race conditions
func (lr *LogRunner) updateCommandStatus(processID, commandID, status string, exitCode int, endTime time.Time) error {
	updateData := map[string]interface{}{
		"status":    status,
		"exit_code": exitCode,
		"end_time":  endTime.Format(time.RFC3339),
	}

	jsonData, err := json.Marshal(updateData)
	if err != nil {
		return fmt.Errorf("failed to marshal command update: %w", err)
	}

	req, err := http.NewRequest(
		"PUT",
		fmt.Sprintf("%s/processes/%s/commands/%s", lr.apiBaseURL, processID, commandID),
		bytes.NewBuffer(jsonData),
	)
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := lr.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

func (lr *LogRunner) sendLogEntry(entry *LogEntry) {
	jsonData, err := json.Marshal(entry)
	if err != nil {
		// Log locally but don't fail the process
		fmt.Fprintf(os.Stderr, "Warning: Failed to marshal log entry: %v\n", err)
		return
	}

	// Retry logic for failed requests
	maxRetries := 3
	for attempt := 1; attempt <= maxRetries; attempt++ {
		resp, err := lr.httpClient.Post(
			fmt.Sprintf("%s/processes/%s/logs", lr.apiBaseURL, entry.ProcessID),
			"application/json",
			bytes.NewBuffer(jsonData),
		)
		if err != nil {
			if attempt == maxRetries {
				// Only log error on final attempt to reduce noise
				fmt.Fprintf(os.Stderr, "Warning: Failed to send log entry after %d attempts: %v\n", maxRetries, err)
			}
			// Exponential backoff for retries
			time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(resp.Body)
			if attempt == maxRetries {
				fmt.Fprintf(os.Stderr, "Warning: API error %d after %d attempts: %s\n", resp.StatusCode, maxRetries, string(body))
			}
			// Retry on 5xx errors, but not 4xx errors
			if resp.StatusCode >= 500 && attempt < maxRetries {
				time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
				continue
			}
		}
		// Success - exit retry loop
		break
	}
}

// LogBatcher implementation
func NewLogBatcher(processID, apiBaseURL string, httpClient *http.Client, config BatchConfig) *LogBatcher {
	return &LogBatcher{
		buffer:     make(chan LogEntry, config.BufferSize),
		batchSize:  config.MaxBatchSize,
		processID:  processID,
		httpClient: httpClient,
		apiBaseURL: apiBaseURL,
		shutdown:   make(chan struct{}),
		flushTimer: time.NewTimer(config.FlushInterval),
	}
}

func (lb *LogBatcher) Start() {
	lb.wg.Add(1)
	go lb.run()
}

func (lb *LogBatcher) Add(entry LogEntry) {
	select {
	case lb.buffer <- entry:
		// Successfully added to buffer
	case <-lb.shutdown:
		// Batcher is shutting down
		return
	default:
		// Buffer full, drop the entry (backpressure)
		fmt.Fprintf(os.Stderr, "Warning: Log buffer full, dropping entry\n")
	}
}

func (lb *LogBatcher) run() {
	defer lb.wg.Done()
	
	batch := make([]LogEntry, 0, lb.batchSize)
	
	for {
		select {
		case entry := <-lb.buffer:
			batch = append(batch, entry)
			
			// Flush if batch is full
			if len(batch) >= lb.batchSize {
				lb.flushBatch(batch)
				batch = batch[:0] // Reset slice
				lb.flushTimer.Reset(1 * time.Second)
			}
			
		case <-lb.flushTimer.C:
			// Timer expired, flush whatever we have
			if len(batch) > 0 {
				lb.flushBatch(batch)
				batch = batch[:0]
			}
			lb.flushTimer.Reset(1 * time.Second)
			
		case <-lb.shutdown:
			// Shutdown requested, flush remaining entries
			if len(batch) > 0 {
				lb.flushBatch(batch)
			}
			return
		}
	}
}

func (lb *LogBatcher) flushBatch(batch []LogEntry) {
	if len(batch) == 0 {
		return
	}
	
	logBatch := LogBatch{
		ProcessID: lb.processID,
		Logs:      batch,
	}
	
	jsonData, err := json.Marshal(logBatch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Failed to marshal log batch: %v\n", err)
		return
	}
	
	// Try to send batch with retry logic
	maxRetries := 3
	for attempt := 1; attempt <= maxRetries; attempt++ {
		resp, err := lb.httpClient.Post(
			fmt.Sprintf("%s/processes/%s/logs/batch", lb.apiBaseURL, lb.processID),
			"application/json",
			bytes.NewBuffer(jsonData),
		)
		if err != nil {
			if attempt == maxRetries {
				fmt.Fprintf(os.Stderr, "Warning: Failed to send log batch after %d attempts: %v\n", maxRetries, err)
			} else {
				time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
				continue
			}
		} else {
			defer resp.Body.Close()
			if resp.StatusCode >= 400 {
				body, _ := io.ReadAll(resp.Body)
				if attempt == maxRetries {
					fmt.Fprintf(os.Stderr, "Warning: API error %d sending batch: %s\n", resp.StatusCode, string(body))
				}
			}
		}
		break
	}
}

func (lb *LogBatcher) Close() {
	close(lb.shutdown)
	lb.wg.Wait()
}