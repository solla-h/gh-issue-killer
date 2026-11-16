package main

import (
	"slices"
	"testing"
)

func TestOpencodeArgs(t *testing.T) {
	args := opencodeArgs(RunnerOptions{
		Prompt:    "do the thing",
		Model:     "magpie/group/deepseek",
		SessionID: "ses_123",
	})
	for _, want := range []string{"run", "--session", "ses_123", "--model", "magpie/group/deepseek", "--format", "json", "--auto", "do the thing"} {
		if !slices.Contains(args, want) {
			t.Fatalf("missing %q in %v", want, args)
		}
	}
	if slices.Contains(args, "--agent") {
		t.Fatalf("agent mode must not be passed: %v", args)
	}
}

func TestClaudeArgs(t *testing.T) {
	opts := RunnerOptions{
		Prompt: "plan it",
		Model:  "sonnet",
		RunDir: "/tmp/run",
	}
	args := claudeArgs(opts, "11111111-1111-4111-8111-111111111111")
	for _, want := range []string{"-p", "plan it", "--output-format", "json", "--permission-mode", "bypassPermissions", "--model", "sonnet", "--add-dir", "/tmp/run", "--session-id", "11111111-1111-4111-8111-111111111111"} {
		if !slices.Contains(args, want) {
			t.Fatalf("missing %q in %v", want, args)
		}
	}
	if slices.Contains(args, "--resume") {
		t.Fatalf("fresh run must not resume: %v", args)
	}

	resumed := claudeArgs(RunnerOptions{Prompt: "fix it", SessionID: "22222222-2222-4222-8222-222222222222"}, "")
	if !slices.Contains(resumed, "--resume") || !slices.Contains(resumed, "22222222-2222-4222-8222-222222222222") {
		t.Fatalf("resume args missing: %v", resumed)
	}
	if slices.Contains(resumed, "--session-id") {
		t.Fatalf("resume must not set a new session id: %v", resumed)
	}
}

func TestCodexArgs(t *testing.T) {
	args := codexArgs(RunnerOptions{
		Cwd:    "/tmp/worktree",
		Prompt: "implement it",
		Model:  "gpt-5.1-codex",
		RunDir: "/tmp/run",
	}, "/tmp/run/last.txt")
	for _, want := range []string{"exec", "implement it", "--cd", "/tmp/worktree", "--full-auto", "--model", "gpt-5.1-codex", "--add-dir", "/tmp/run", "--output-last-message", "/tmp/run/last.txt"} {
		if !slices.Contains(args, want) {
			t.Fatalf("missing %q in %v", want, args)
		}
	}
	if slices.Contains(args, "--agent") || slices.Contains(args, "--profile") {
		t.Fatalf("agent/profile must not be passed: %v", args)
	}
}

func TestPiArgs(t *testing.T) {
	fresh := piArgs(RunnerOptions{
		Prompt: "do the thing",
		Model:  "howtofix/deepseek-v4.1-flash-1017",
	}, "11111111-1111-4111-8111-111111111111")
	for _, want := range []string{"-p", "--mode", "json", "--model", "howtofix/deepseek-v4.1-flash-1017", "--session-id", "11111111-1111-4111-8111-111111111111", "do the thing"} {
		if !slices.Contains(fresh, want) {
			t.Fatalf("missing %q in %v", want, fresh)
		}
	}
	if slices.Contains(fresh, "--resume") {
		t.Fatalf("pi must not use --resume: %v", fresh)
	}
	if fresh[len(fresh)-1] != "do the thing" {
		t.Fatalf("prompt must be the last argv entry: %v", fresh)
	}

	// Resume reuses the same --session-id flag with the stored id.
	resumed := piArgs(RunnerOptions{Prompt: "fix it", SessionID: "22222222-2222-4222-8222-222222222222"}, "22222222-2222-4222-8222-222222222222")
	if !slices.Contains(resumed, "--session-id") || !slices.Contains(resumed, "22222222-2222-4222-8222-222222222222") {
		t.Fatalf("resume args missing: %v", resumed)
	}
	if slices.Contains(resumed, "--resume") {
		t.Fatalf("resume must not use --resume: %v", resumed)
	}
}

func TestOmpArgs(t *testing.T) {
	fresh := ompArgs(RunnerOptions{
		Prompt: "do the thing",
		Model:  "howtofix/deepseek-v4.1-flash-1017",
	})
	for _, want := range []string{"-p", "--mode", "json", "--model", "howtofix/deepseek-v4.1-flash-1017", "do the thing"} {
		if !slices.Contains(fresh, want) {
			t.Fatalf("missing %q in %v", want, fresh)
		}
	}
	if slices.Contains(fresh, "--resume") || slices.Contains(fresh, "--session-id") {
		t.Fatalf("fresh run must not resume: %v", fresh)
	}
	if fresh[len(fresh)-1] != "do the thing" {
		t.Fatalf("prompt must be the last argv entry: %v", fresh)
	}

	resumed := ompArgs(RunnerOptions{Prompt: "fix it", SessionID: "22222222-2222-4222-8222-222222222222"})
	if !slices.Contains(resumed, "--resume") || !slices.Contains(resumed, "22222222-2222-4222-8222-222222222222") {
		t.Fatalf("resume args missing: %v", resumed)
	}
	if slices.Contains(resumed, "--session-id") {
		t.Fatalf("omp has no --session-id flag: %v", resumed)
	}
}

// Recorded --mode json excerpts (pi 1.5.2 / omp 18.20.3, trimmed to the
// fields the parser reads).
const (
	piSessionLine   = `{"type":"session","version":3,"id":"01a1e38b-65c1-76a3-9905-b87991f8b2b9","timestamp":"2026-11-16T08:48:46.018Z","cwd":"/tmp/pi-probe"}`
	piUserLine      = `{"type":"message_end","message":{"role":"user","content":[{"type":"text","text":"Reply with exactly the word: pong"}]}}`
	piToolUseLine   = `{"type":"message_end","message":{"role":"assistant","content":[{"type":"toolCall","id":"call_8192a3e46aed40d0a5be1d2f","name":"write","arguments":{"path":"/tmp/pi-live/hello.txt","content":"hi\n"}}],"stopReason":"toolUse"}}`
	piStopLine      = `{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Created hello.txt with \"hi\"."}],"stopReason":"stop"}}`
	piErrorLine     = `{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"503: {\"message\":\"Server error\",\"type\":\"server_error\",\"code\":\"system_cpu_overloaded\"}"}}`
	piRetryFailLine = `{"type":"auto_retry_end","success":false,"attempt":3,"finalError":"Assistant returned empty stop after retry cap; try switching models or ` + "`/shake images`" + ` to remove archived frames"}`
	piEscapeLine    = "\x1b[?25h\x1b[?2004l\x1b[<u" // pi --resume prints terminal escapes, not JSON
)

func TestPiJSONAccumulator(t *testing.T) {
	acc := &piJSONAccumulator{}
	acc.feed(piSessionLine)
	acc.feed(piUserLine)
	acc.feed(piToolUseLine)
	if acc.sessionID != "01a1e38b-65c1-76a3-9905-b87991f8b2b9" {
		t.Fatalf("sessionID = %q", acc.sessionID)
	}
	if len(acc.texts) != 0 {
		t.Fatalf("user/toolUse messages must add no text: %v", acc.texts)
	}

	// A transient error is superseded by a later good answer.
	acc.feed(piErrorLine)
	if acc.errText == "" {
		t.Fatal("error message must set errText")
	}
	acc.feed(piStopLine)
	if acc.errText != "" {
		t.Fatalf("good answer must clear errText, got %q", acc.errText)
	}
	if len(acc.texts) != 1 || acc.texts[0] != `Created hello.txt with "hi".` {
		t.Fatalf("texts = %v", acc.texts)
	}

	// A failed retry cap is the terminal error.
	acc.feed(piRetryFailLine)
	if acc.errText == "" {
		t.Fatal("failed retry cap must set errText")
	}
}

func TestPiJSONAccumulatorEmptyFailure(t *testing.T) {
	// omp returning empty content: stop with no text, then the retry cap.
	acc := &piJSONAccumulator{}
	acc.feed(piSessionLine)
	acc.feed(`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"stop"}}`)
	acc.feed(piRetryFailLine)
	if acc.text() != "" {
		t.Fatalf("text must be empty, got %q", acc.text())
	}
	if acc.errText == "" {
		t.Fatal("empty failure must set errText from finalError")
	}
}

func TestPiJSONAccumulatorSkipsNonJSON(t *testing.T) {
	acc := &piJSONAccumulator{}
	acc.feed(piEscapeLine)
	acc.feed(piSessionLine)
	if acc.sessionID != "01a1e38b-65c1-76a3-9905-b87991f8b2b9" {
		t.Fatalf("sessionID = %q after escape line", acc.sessionID)
	}
}

func TestNewCodingRunner(t *testing.T) {
	for _, id := range []string{"opencode", "claude", "codex", "pi", "omp"} {
		runner, err := newCodingRunner(id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if runner.ID() != id {
			t.Fatalf("runner id = %q, want %q", runner.ID(), id)
		}
	}
	if _, err := newCodingRunner("vim"); err == nil {
		t.Fatal("expected unsupported runner error")
	}
}
