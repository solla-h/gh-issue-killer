package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newProjectsTestWeb(t *testing.T, raw string) (*webServer, string) {
	t.Helper()
	old := stateDirOverride
	stateDirOverride = t.TempDir()
	t.Cleanup(func() { stateDirOverride = old })
	store, err := LoadStore()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if raw != "" {
		if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := loadConfig(path)
	if raw == "" {
		cfg, err = &Config{File: path, Webhook: true, PlanParallel: 5, ImplementParallel: 1}, nil
	}
	if err != nil {
		t.Fatalf("test config does not load: %v", err)
	}
	cfg.WebPort = 8787
	web := &webServer{
		cfg: cfg, store: store, log: NewSilentLogger(), identity: "bot",
		repoAccess: func(_ *Proxy, repos []string) ([]repoAccessInfo, error) {
			results := make([]repoAccessInfo, 0, len(repos))
			for _, repo := range repos {
				results = append(results, repoAccessInfo{Repo: repo, Permission: "write", WebhookReady: true})
			}
			return results, nil
		},
	}
	return web, path
}

func postProject(t *testing.T, web *webServer, body, origin string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/projects", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	recorder := httptest.NewRecorder()
	web.handleProjects(recorder, request)
	return recorder
}

func readDoc(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("config is not JSON: %v", err)
	}
	return doc
}

func TestAddRepoToDocumentKeepsListShape(t *testing.T) {
	names := map[string]any{"repos": []any{"a/b"}}
	if err := addRepoToDocument(names, "c/d"); err != nil {
		t.Fatal(err)
	}
	list, _ := names["repos"].([]any)
	if len(list) != 2 || list[1] != "c/d" {
		t.Fatalf("name list = %#v", names["repos"])
	}

	objects := map[string]any{"repos": []any{map[string]any{"repo": "a/b", "model": "m"}}}
	if err := addRepoToDocument(objects, "c/d"); err != nil {
		t.Fatal(err)
	}
	list, _ = objects["repos"].([]any)
	entry, ok := list[1].(map[string]any)
	if !ok || entry["repo"] != "c/d" {
		t.Fatalf("object list = %#v", objects["repos"])
	}

	keyed := map[string]any{"repos": map[string]any{"a/b": map[string]any{}}}
	if err := addRepoToDocument(keyed, "c/d"); err != nil {
		t.Fatal(err)
	}
	table, _ := keyed["repos"].(map[string]any)
	if _, ok := table["c/d"]; !ok {
		t.Fatalf("keyed form = %#v", keyed["repos"])
	}

	if err := addRepoToDocument(names, "a/b"); err == nil {
		t.Fatal("adding a duplicate must fail")
	}
}

func TestRemoveRepoFromDocumentKeepsOverrides(t *testing.T) {
	doc := map[string]any{
		"codingAgent": "claude",
		"repos": []any{
			map[string]any{"repo": "a/b", "model": "m"},
			map[string]any{"repo": "c/d"},
		},
	}
	if err := removeRepoFromDocument(doc, "a/b"); err != nil {
		t.Fatal(err)
	}
	list, _ := doc["repos"].([]any)
	if len(list) != 1 || repoNameOf(list[0]) != "c/d" {
		t.Fatalf("repos = %#v", doc["repos"])
	}
	if doc["codingAgent"] != "claude" {
		t.Fatalf("global key dropped: %#v", doc)
	}
	if err := removeRepoFromDocument(doc, "c/d"); err == nil {
		t.Fatal("removing the last project must fail")
	}
}

func TestWebProjectsAddsAfterAccessCheck(t *testing.T) {
	web, path := newProjectsTestWeb(t, `{"repos":["a/b"],"codingAgent":"opencode"}`)
	recorder := postProject(t, web, `{"action":"add","repo":"https://github.com/c/d.git"}`, "http://127.0.0.1:8787")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	doc := readDoc(t, path)
	list, _ := doc["repos"].([]any)
	if len(list) != 2 || list[1] != "c/d" {
		t.Fatalf("repos = %#v", doc["repos"])
	}
	cfg, err := loadConfig(path)
	if err != nil || len(cfg.Repos) != 2 {
		t.Fatalf("saved config = %+v, %v", cfg, err)
	}
}

func TestWebProjectsRejectsRepoTheTokenCannotSee(t *testing.T) {
	web, path := newProjectsTestWeb(t, `{"repos":["a/b"],"codingAgent":"opencode"}`)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	web.repoAccess = func(_ *Proxy, repos []string) ([]repoAccessInfo, error) {
		return []repoAccessInfo{{Repo: repos[0], Permission: "none", Err: "GET /repos/o/r -> 404"}}, nil
	}
	recorder := postProject(t, web, `{"action":"add","repo":"o/typo"}`, "http://127.0.0.1:8787")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("a rejected add must not rewrite the config")
	}
}

func TestWebProjectsRemovesOnlyThatRepo(t *testing.T) {
	web, path := newProjectsTestWeb(t, `{"repos":{"a/b":{"model":"m"},"c/d":{}},"codingAgent":"opencode"}`)
	recorder := postProject(t, web, `{"action":"remove","repo":"a/b"}`, "http://127.0.0.1:8787")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	doc := readDoc(t, path)
	table, _ := doc["repos"].(map[string]any)
	if _, ok := table["a/b"]; ok {
		t.Fatalf("a/b still in %#v", doc["repos"])
	}
	if _, ok := table["c/d"]; !ok {
		t.Fatalf("c/d dropped from %#v", doc["repos"])
	}
}

func TestWebProjectsRejectsCrossOrigin(t *testing.T) {
	web, _ := newProjectsTestWeb(t, `{"repos":["a/b"],"codingAgent":"opencode"}`)
	recorder := postProject(t, web, `{"action":"remove","repo":"a/b"}`, "https://evil.example")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
}

// The list must answer from the config alone: the page renders it before the
// role check, and nine repos with a slow proxy made the combined request take
// eight seconds.
func TestWebProjectsListDoesNotCheckGitHub(t *testing.T) {
	web, _ := newProjectsTestWeb(t, `{"repos":["a/b"],"codingAgent":"opencode"}`)
	web.repoAccess = func(_ *Proxy, _ []string) ([]repoAccessInfo, error) {
		t.Fatal("GET /api/projects must not call GitHub")
		return nil, nil
	}
	recorder := httptest.NewRecorder()
	web.handleProjects(recorder, httptest.NewRequest(http.MethodGet, "/api/projects", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var payload projectsPayload
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Bot != "bot" || len(payload.Repos) != 1 {
		t.Fatalf("payload = %+v", payload)
	}
	if repo := payload.Repos[0]; !strings.HasSuffix(repo.AccessURL, "/a/b/settings/access") {
		t.Fatalf("access URL = %q", repo.AccessURL)
	}
}

func TestWebProjectsAccessReportsMissingPush(t *testing.T) {
	web, _ := newProjectsTestWeb(t, `{"repos":["a/b"],"codingAgent":"opencode"}`)
	web.repoAccess = func(_ *Proxy, repos []string) ([]repoAccessInfo, error) {
		return []repoAccessInfo{{Repo: repos[0], Permission: "read", WebhookReady: false}}, nil
	}
	recorder := httptest.NewRecorder()
	web.handleProjectsAccess(recorder, httptest.NewRequest(http.MethodGet, "/api/projects/access", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var payload projectsAccessPayload
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Repos) != 1 {
		t.Fatalf("payload = %+v", payload)
	}
	repo := payload.Repos[0]
	if repo.Name != "a/b" || repo.Permission != "read" || repo.CanPush || repo.CanAdmin {
		t.Fatalf("access = %+v", repo)
	}
}

func TestWebProjectsAccessKeepsPartialResults(t *testing.T) {
	web, _ := newProjectsTestWeb(t, `{"repos":["a/b","c/d"],"codingAgent":"opencode"}`)
	web.repoAccess = func(_ *Proxy, repos []string) ([]repoAccessInfo, error) {
		return []repoAccessInfo{
			{Repo: repos[0], Permission: "none", Err: "GET /repos/a/b -> 404"},
			{Repo: repos[1], Permission: "admin", WebhookReady: true},
		}, nil
	}
	recorder := httptest.NewRecorder()
	web.handleProjectsAccess(recorder, httptest.NewRequest(http.MethodGet, "/api/projects/access", nil))
	var payload projectsAccessPayload
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Repos) != 2 {
		t.Fatalf("a failed repo must not hide the others: %+v", payload)
	}
	if payload.Repos[0].Error == "" || !payload.Repos[1].CanAdmin {
		t.Fatalf("repos = %+v", payload.Repos)
	}
}
