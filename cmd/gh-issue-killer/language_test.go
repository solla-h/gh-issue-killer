package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeReposReplyLanguage(t *testing.T) {
	inherited, err := normalizeRepos(json.RawMessage(`["acme/one"]`), repoOptions{Language: "zh-CN"})
	if err != nil {
		t.Fatal(err)
	}
	if got := inherited[0].Language; got != "zh-CN" {
		t.Fatalf("inherited language = %q, want zh-CN", got)
	}

	overridden, err := normalizeRepos(json.RawMessage(`{"acme/two": {"replyLanguage": "en"}}`), repoOptions{Language: "zh-CN"})
	if err != nil {
		t.Fatal(err)
	}
	if got := overridden[0].Language; got != "en" {
		t.Fatalf("per-repo language = %q, want en", got)
	}

	plain, err := normalizeRepos(json.RawMessage(`["acme/one"]`), repoOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := plain[0].Language; got != "auto" {
		t.Fatalf("default language = %q, want auto", got)
	}
}

func TestLanguageConfigAndLegacyAlias(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	cfg, err := loadConfig(write("new.json", `{"repos": ["acme/one"], "language": "en"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Language != "en" || cfg.Repos[0].Language != "en" {
		t.Fatalf("language = %q, repo = %q", cfg.Language, cfg.Repos[0].Language)
	}
	if cfg.DeprecatedReplyLanguage {
		t.Fatal("new key flagged as deprecated")
	}

	legacy, err := loadConfig(write("legacy.json", `{"repos": ["acme/one"], "replyLanguage": "zh-CN"}`))
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Language != "zh-CN" || legacy.Repos[0].Language != "zh-CN" {
		t.Fatalf("legacy language = %q, repo = %q", legacy.Language, legacy.Repos[0].Language)
	}
	if !legacy.DeprecatedReplyLanguage {
		t.Fatal("replyLanguage alias was not flagged as deprecated")
	}

	precedence, err := loadConfig(write("both.json", `{"repos": ["acme/one"], "language": "en", "replyLanguage": "zh-CN"}`))
	if err != nil {
		t.Fatal(err)
	}
	if precedence.Language != "en" {
		t.Fatalf("language = %q, want the new key to win", precedence.Language)
	}
}

func TestReplyLanguage(t *testing.T) {
	auto := &AppContext{
		Cfg:  &Config{Language: "auto"},
		Repo: RepoConfig{Language: "auto"},
	}
	if got := promptLanguage(auto); !strings.Contains(got, "same language") {
		t.Fatalf("auto = %q, want the follow-the-issue instruction", got)
	}

	global := &AppContext{
		Cfg:  &Config{Language: "zh-CN"},
		Repo: RepoConfig{Language: ""},
	}
	if got := promptLanguage(global); got != "zh-CN" {
		t.Fatalf("global = %q, want zh-CN", got)
	}

	perRepo := &AppContext{
		Cfg:  &Config{Language: "zh-CN"},
		Repo: RepoConfig{Language: "en"},
	}
	if got := promptLanguage(perRepo); got != "en" {
		t.Fatalf("per-repo = %q, want en", got)
	}

	noConfig := &AppContext{Repo: RepoConfig{}}
	if got := promptLanguage(noConfig); !strings.Contains(got, "same language") {
		t.Fatalf("missing config = %q, want the follow-the-issue instruction", got)
	}
}

func TestPromptLanguagePlaceholder(t *testing.T) {
	text, err := renderPrompt("plan", map[string]string{"LANGUAGE": "zh-CN"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "zh-CN") || strings.Contains(text, "{{LANGUAGE}}") {
		t.Fatal("plan prompt did not receive the language")
	}
}
