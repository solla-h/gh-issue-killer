package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// CodingAgentInfo describes a supported coding-agent CLI.
type CodingAgentInfo struct {
	ID          string
	Name        string
	Binary      string
	InstallHint string
}

var codingAgentRegistry = []CodingAgentInfo{
	{ID: "opencode", Name: "opencode", Binary: "opencode", InstallHint: "https://opencode.ai"},
	{ID: "claude", Name: "Claude Code", Binary: "claude", InstallHint: "https://docs.anthropic.com/en/docs/claude-code"},
	{ID: "codex", Name: "Codex CLI", Binary: "codex", InstallHint: "https://github.com/openai/codex"},
	{ID: "pi", Name: "Pi", Binary: "pi", InstallHint: "https://pi.dev"},
	{ID: "omp", Name: "Oh My Pi", Binary: "omp", InstallHint: "https://omp.sh"},
}

func codingAgentInfoByID(id string) (CodingAgentInfo, bool) {
	for _, info := range codingAgentRegistry {
		if strings.EqualFold(info.ID, id) {
			return info, true
		}
	}
	return CodingAgentInfo{}, false
}

type DetectedCodingAgent struct {
	Info    CodingAgentInfo
	Path    string
	Version string
}

// detectCodingAgents reports the supported CLIs that are currently on PATH.
func detectCodingAgents() []DetectedCodingAgent {
	detected := make([]DetectedCodingAgent, 0, len(codingAgentRegistry))
	for _, info := range codingAgentRegistry {
		path, err := exec.LookPath(info.Binary)
		if err != nil {
			continue
		}
		version := ""
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		result, runErr := runCmd(ctx, path, []string{"--version"}, CmdOptions{
			Timeout:      4 * time.Second,
			AllowFailure: true,
		})
		cancel()
		if runErr == nil && result.Code == 0 {
			version = strings.SplitN(strings.TrimSpace(result.Stdout), "\n", 2)[0]
		}
		detected = append(detected, DetectedCodingAgent{Info: info, Path: path, Version: strings.TrimSpace(version)})
	}
	return detected
}

type RunnerOptions struct {
	Cwd       string
	Prompt    string
	SessionID string
	Model     string
	RunDir    string
	Env       []string
	Timeout   time.Duration
	LogFile   string
	Log       *Logger
}

type RunnerResult struct {
	SessionID string
	Text      string
	Code      int
	TimedOut  bool
	Stderr    string
	ErrText   string
}

type CodingRunner interface {
	ID() string
	Run(ctx context.Context, opts RunnerOptions) (RunnerResult, error)
}

func newCodingRunner(id string) (CodingRunner, error) {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "", "opencode":
		return &opencodeRunner{}, nil
	case "claude":
		return &claudeRunner{}, nil
	case "codex":
		return &codexRunner{}, nil
	case "pi":
		return &piRunner{}, nil
	case "omp":
		return &ompRunner{}, nil
	default:
		return nil, fmt.Errorf("unsupported codingAgent %q (supported: %s)", id, supportedCodingAgentList())
	}
}

type processResult struct {
	Stdout   string
	Stderr   string
	Code     int
	TimedOut bool
}

// runProcess runs one coding-agent CLI. stdout is scanned line by line so a
// runner can parse its event stream, and both streams are appended to LogFile.
func runProcess(ctx context.Context, binary string, args []string, opts RunnerOptions, onStdout func(string)) (processResult, error) {
	started := time.Now()
	if opts.Log != nil {
		opts.Log.Debug("runner start: %s cwd=%s timeout=%s", binary, opts.Cwd, opts.Timeout)
	}
	runCtx := ctx
	var cancel context.CancelFunc
	if opts.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(runCtx, binary, args...)
	cmd.Dir = opts.Cwd
	cmd.Env = opts.Env
	cmd.Stdin = nil // /dev/null: non-interactive CLIs must not wait for input
	cmd.SysProcAttr = childSysProcAttr()

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return processResult{}, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return processResult{}, err
	}
	if err := cmd.Start(); err != nil {
		return processResult{}, err
	}

	var logFile *os.File
	if opts.LogFile != "" {
		_ = os.MkdirAll(filepath.Dir(opts.LogFile), 0o755)
		logFile, _ = os.OpenFile(opts.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if logFile != nil {
			defer logFile.Close()
		}
	}
	writeLog := func(line string) {
		if logFile != nil {
			_, _ = io.WriteString(logFile, line+"\n")
		}
	}

	var stdoutBuf, stderrBuf limitedBuffer
	stdoutBuf.limit = 8 * 1024 * 1024
	stderrBuf.limit = 2 * 1024 * 1024

	done := make(chan error, 2)
	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			stdoutBuf.Write([]byte(line + "\n"))
			writeLog(line)
			if onStdout != nil {
				onStdout(line)
			}
		}
		done <- scanner.Err()
	}()
	go func() {
		scanner := bufio.NewScanner(stderrPipe)
		scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			stderrBuf.Write([]byte(line + "\n"))
			writeLog("[stderr] " + line)
		}
		done <- scanner.Err()
	}()

	waitErr := cmd.Wait()
	<-done
	<-done

	result := processResult{Stdout: stdoutBuf.String(), Stderr: stderrBuf.String()}
	if cmd.ProcessState != nil {
		result.Code = cmd.ProcessState.ExitCode()
	}
	if opts.Log != nil {
		opts.Log.Debug("runner exit: code=%d timedOut=%v duration=%s", result.Code, result.TimedOut, time.Since(started).Round(time.Millisecond))
	}
	if runCtx.Err() == context.DeadlineExceeded {
		result.TimedOut = true
		if cmd.Process != nil {
			killChildTree(cmd.Process.Pid)
		}
		return result, fmt.Errorf("coding agent timed out after %s", opts.Timeout)
	}
	if waitErr != nil {
		return result, fmt.Errorf("coding agent exited %d: %s", result.Code, tailString(result.Stderr, 20))
	}
	return result, nil
}

func runnerBinary(info CodingAgentInfo) (string, error) {
	path, err := exec.LookPath(info.Binary)
	if err != nil {
		return "", fmt.Errorf("%s is not installed or not on PATH; install it from %s", info.Binary, info.InstallHint)
	}
	return path, nil
}

func newUUID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("gwik-%d", time.Now().UnixNano())
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16])
}

// ---------------------------------------------------------------------------
// opencode
// ---------------------------------------------------------------------------

type opencodeRunner struct{}

func (r *opencodeRunner) ID() string { return "opencode" }

func opencodeArgs(opts RunnerOptions) []string {
	args := []string{"run"}
	if opts.SessionID != "" {
		args = append(args, "--session", opts.SessionID)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	args = append(args, "--format", "json", "--auto", opts.Prompt)
	return args
}

func (r *opencodeRunner) Run(ctx context.Context, opts RunnerOptions) (RunnerResult, error) {
	info, _ := codingAgentInfoByID("opencode")
	binary, err := runnerBinary(info)
	if err != nil {
		return RunnerResult{}, err
	}

	result := RunnerResult{SessionID: opts.SessionID}
	var texts []string
	var eventErr string
	if opts.Log != nil {
		opts.Log.Debug("opencode run: session=%s model=%s cwd=%s", firstNonEmpty(opts.SessionID, "(new)"), firstNonEmpty(opts.Model, "(default)"), opts.Cwd)
	}

	proc, runErr := runProcess(ctx, binary, opencodeArgs(opts), opts, func(line string) {
		var event opencodeEvent
		if json.Unmarshal([]byte(line), &event) != nil {
			return
		}
		if event.SessionID != "" {
			result.SessionID = event.SessionID
		}
		if event.Type == "text" && event.Part.Text != "" {
			texts = append(texts, event.Part.Text)
		}
		if event.Type == "error" && event.Error.Message != "" {
			eventErr = event.Error.Message
		}
	})
	// Join with a blank line: text parts are separated messages, not one stream,
	// so "I'll explore..." followed by "## 问题分析" must not become one line.
	result.Text = strings.TrimSpace(strings.Join(texts, "\n\n"))
	result.Stderr = tailString(proc.Stderr, 50)
	result.Code = proc.Code
	result.TimedOut = proc.TimedOut
	result.ErrText = eventErr
	if runErr != nil {
		return result, runErr
	}
	if result.Text == "" && eventErr != "" {
		return result, fmt.Errorf("opencode failed: %s", eventErr)
	}
	return result, nil
}

type opencodeEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionID"`
	Part      struct {
		Text string `json:"text"`
	} `json:"part"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// ---------------------------------------------------------------------------
// Claude Code
// ---------------------------------------------------------------------------

type claudeRunner struct{}

func (r *claudeRunner) ID() string { return "claude" }

func claudeArgs(opts RunnerOptions, sessionID string) []string {
	args := []string{
		"-p", opts.Prompt,
		"--output-format", "json",
		"--permission-mode", "bypassPermissions",
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.RunDir != "" {
		args = append(args, "--add-dir", opts.RunDir)
	}
	if opts.SessionID != "" {
		args = append(args, "--resume", opts.SessionID)
	} else if sessionID != "" {
		args = append(args, "--session-id", sessionID)
	}
	return args
}

type claudeResult struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	IsError   bool   `json:"is_error"`
	Result    string `json:"result"`
	SessionID string `json:"session_id"`
}

func (r *claudeRunner) Run(ctx context.Context, opts RunnerOptions) (RunnerResult, error) {
	info, _ := codingAgentInfoByID("claude")
	binary, err := runnerBinary(info)
	if err != nil {
		return RunnerResult{}, err
	}
	sessionID := ""
	if opts.SessionID == "" {
		sessionID = newUUID()
	}
	if opts.Log != nil {
		opts.Log.Debug("claude run: session=%s model=%s cwd=%s runDir=%s", firstNonEmpty(opts.SessionID, sessionID), firstNonEmpty(opts.Model, "(default)"), opts.Cwd, opts.RunDir)
	}
	proc, runErr := runProcess(ctx, binary, claudeArgs(opts, sessionID), opts, nil)

	result := RunnerResult{
		SessionID: firstNonEmpty(opts.SessionID, sessionID),
		Code:      proc.Code,
		TimedOut:  proc.TimedOut,
		Stderr:    tailString(proc.Stderr, 50),
	}
	var parsed claudeResult
	if json.Unmarshal([]byte(proc.Stdout), &parsed) == nil {
		if parsed.SessionID != "" {
			result.SessionID = parsed.SessionID
		}
		result.Text = strings.TrimSpace(parsed.Result)
		if parsed.IsError {
			result.ErrText = parsed.Result
		}
	} else {
		result.Text = strings.TrimSpace(proc.Stdout)
	}
	if runErr != nil {
		return result, runErr
	}
	if result.Text == "" && result.ErrText != "" {
		return result, fmt.Errorf("claude failed: %s", result.ErrText)
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Codex
// ---------------------------------------------------------------------------

type codexRunner struct{}

func (r *codexRunner) ID() string { return "codex" }

func codexArgs(opts RunnerOptions, lastMessageFile string) []string {
	args := []string{
		"exec", opts.Prompt,
		"--cd", opts.Cwd,
		"--full-auto",
		"--color", "never",
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.RunDir != "" {
		args = append(args, "--add-dir", opts.RunDir)
	}
	if lastMessageFile != "" {
		args = append(args, "--output-last-message", lastMessageFile)
	}
	return args
}

func (r *codexRunner) Run(ctx context.Context, opts RunnerOptions) (RunnerResult, error) {
	info, _ := codingAgentInfoByID("codex")
	binary, err := runnerBinary(info)
	if err != nil {
		return RunnerResult{}, err
	}
	lastFile := ""
	if opts.RunDir != "" {
		lastFile = filepath.Join(opts.RunDir, fmt.Sprintf("codex-last-%d.txt", time.Now().UnixNano()))
	}
	if opts.Log != nil {
		opts.Log.Debug("codex run: model=%s cwd=%s runDir=%s", firstNonEmpty(opts.Model, "(default)"), opts.Cwd, opts.RunDir)
	}
	proc, runErr := runProcess(ctx, binary, codexArgs(opts, lastFile), opts, nil)

	result := RunnerResult{
		Code:     proc.Code,
		TimedOut: proc.TimedOut,
		Stderr:   tailString(proc.Stderr, 50),
	}
	if lastFile != "" {
		if data, readErr := os.ReadFile(lastFile); readErr == nil {
			result.Text = strings.TrimSpace(string(data))
		}
	}
	if result.Text == "" {
		result.Text = strings.TrimSpace(proc.Stdout)
	}
	if runErr != nil {
		return result, runErr
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Pi and Oh My Pi (omp)
//
// Both CLIs share the same --mode json event stream: pi documents it at
// pi.dev (docs/json.md) and omp, a Pi fork, emits the same records. The
// stream is JSONL: a session header first, then agent/turn/message events,
// ending with agent_end (and agent_settled for pi; omp stops after
// agent_end). One parser serves both runners.
//
// Two CLI quirks matter here, both verified against pi 1.5.2 / omp 18.20.3:
//   - Pi waits on stdin in print mode when stdin is an open pipe, so the
//     daemon must keep stdin at /dev/null (runProcess already does).
//   - Pi's --resume opens the interactive session picker even with -p
//     --mode json, emitting terminal escapes instead of JSON. Resume is
//     therefore done with --session-id (same flag as a fresh run): pi
//     continues the session whose id is passed. omp has no --session-id
//     flag, so a fresh run takes its id from the session header and a
//     later stage continues it with --resume, which does work in
//     print mode.
// ---------------------------------------------------------------------------

// piJSONEvent is one record of the pi/omp --mode json stream. Only the
// fields the daemon needs are decoded; anything else is ignored.
type piJSONEvent struct {
	Type       string `json:"type"`
	ID         string `json:"id"` // session header
	FinalError string `json:"finalError"`
	Success    *bool  `json:"success"` // auto_retry_end
	Message    struct {
		Role         string `json:"role"`
		StopReason   string `json:"stopReason"`
		ErrorMessage string `json:"errorMessage"`
		Content      []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

// piJSONAccumulator folds a pi/omp JSONL stream into a session id, the
// assistant's text and the terminal error, if any.
type piJSONAccumulator struct {
	sessionID string
	texts     []string
	errText   string
}

// text joins the collected assistant texts the way the runners report
// them: text parts are separate messages, not one stream.
func (a *piJSONAccumulator) text() string {
	return strings.TrimSpace(strings.Join(a.texts, "\n\n"))
}

func (a *piJSONAccumulator) feed(line string) {
	var event piJSONEvent
	if json.Unmarshal([]byte(line), &event) != nil {
		return // non-JSON lines (e.g. terminal escapes) are skipped
	}
	switch event.Type {
	case "session":
		if event.ID != "" {
			a.sessionID = event.ID
		}
	case "message_end":
		if event.Message.Role != "assistant" {
			return
		}
		added := false
		for _, block := range event.Message.Content {
			if block.Type == "text" && block.Text != "" {
				a.texts = append(a.texts, block.Text)
				added = true
			}
		}
		switch event.Message.StopReason {
		case "stop":
			if added {
				// A later good answer supersedes transient retry errors.
				a.errText = ""
			}
		case "error":
			if event.Message.ErrorMessage != "" {
				a.errText = event.Message.ErrorMessage
			}
		}
	case "auto_retry_end":
		if event.Success != nil && !*event.Success && event.FinalError != "" {
			a.errText = event.FinalError
		}
	}
}

type piRunner struct{}

func (r *piRunner) ID() string { return "pi" }

func piArgs(opts RunnerOptions, sessionID string) []string {
	args := []string{"-p", "--mode", "json"}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	args = append(args, "--session-id", sessionID, opts.Prompt)
	return args
}

func (r *piRunner) Run(ctx context.Context, opts RunnerOptions) (RunnerResult, error) {
	info, _ := codingAgentInfoByID("pi")
	binary, err := runnerBinary(info)
	if err != nil {
		return RunnerResult{}, err
	}
	sessionID := opts.SessionID
	if sessionID == "" {
		sessionID = newUUID()
	}
	if opts.Log != nil {
		opts.Log.Debug("pi run: session=%s model=%s cwd=%s", sessionID, firstNonEmpty(opts.Model, "(default)"), opts.Cwd)
	}
	acc := &piJSONAccumulator{sessionID: sessionID}
	proc, runErr := runProcess(ctx, binary, piArgs(opts, sessionID), opts, acc.feed)
	result := RunnerResult{
		SessionID: acc.sessionID,
		Text:      acc.text(),
		Code:      proc.Code,
		TimedOut:  proc.TimedOut,
		Stderr:    tailString(proc.Stderr, 50),
		ErrText:   acc.errText,
	}
	if runErr != nil {
		return result, runErr
	}
	if result.Text == "" && result.ErrText != "" {
		return result, fmt.Errorf("pi failed: %s", result.ErrText)
	}
	return result, nil
}

type ompRunner struct{}

func (r *ompRunner) ID() string { return "omp" }

func ompArgs(opts RunnerOptions) []string {
	args := []string{"-p", "--mode", "json"}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.SessionID != "" {
		args = append(args, "--resume", opts.SessionID)
	}
	args = append(args, opts.Prompt)
	return args
}

func (r *ompRunner) Run(ctx context.Context, opts RunnerOptions) (RunnerResult, error) {
	info, _ := codingAgentInfoByID("omp")
	binary, err := runnerBinary(info)
	if err != nil {
		return RunnerResult{}, err
	}
	if opts.Log != nil {
		opts.Log.Debug("omp run: session=%s model=%s cwd=%s", firstNonEmpty(opts.SessionID, "(new)"), firstNonEmpty(opts.Model, "(default)"), opts.Cwd)
	}
	acc := &piJSONAccumulator{sessionID: opts.SessionID}
	proc, runErr := runProcess(ctx, binary, ompArgs(opts), opts, acc.feed)
	result := RunnerResult{
		SessionID: acc.sessionID,
		Text:      acc.text(),
		Code:      proc.Code,
		TimedOut:  proc.TimedOut,
		Stderr:    tailString(proc.Stderr, 50),
		ErrText:   acc.errText,
	}
	if runErr != nil {
		return result, runErr
	}
	if result.Text == "" && result.ErrText != "" {
		return result, fmt.Errorf("omp failed: %s", result.ErrText)
	}
	return result, nil
}
