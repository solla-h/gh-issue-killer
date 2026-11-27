package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// The projects page edits config.repos from one screen: list every watched
// repository with the bot's role on it, add one, remove one. The settings
// dialog still owns per-repo options (agent, model, release); this endpoint
// only touches membership, so the two editors never disagree about the shape
// of the document.

// webProject is one row of the projects page. It carries only what the daemon
// already knows, so GET /api/projects answers without touching GitHub and the
// page renders immediately; the role check is a second, slower request
// (GET /api/projects/access) that fills the badges in place.
type webProject struct {
	Name      string   `json:"name"`
	Base      string   `json:"base"`
	Agent     string   `json:"agent"`
	Model     string   `json:"model"`
	Mode      string   `json:"mode"`
	Approvers []string `json:"approvers"`
	HookID    int64    `json:"hookId"`
	Smee      string   `json:"smee"`
	LastPoll  string   `json:"lastPoll"`
	AccessURL string   `json:"accessUrl"`
}

type projectsPayload struct {
	// Bot is the account the daemon acts as; the page asks the main account to
	// invite this login.
	Bot     string       `json:"bot"`
	Config  string       `json:"config"`
	Webhook bool         `json:"webhook"`
	Repos   []webProject `json:"repos"`
}

// projectAccess is the role check for one repository.
type projectAccess struct {
	Name         string `json:"name"`
	Permission   string `json:"permission"`
	CanPush      bool   `json:"canPush"`
	CanAdmin     bool   `json:"canAdmin"`
	WebhookReady bool   `json:"webhookReady"`
	Error        string `json:"error,omitempty"`
}

type projectsAccessPayload struct {
	Repos []projectAccess `json:"repos"`
	Error string          `json:"error,omitempty"`
}

func (w *webServer) handleProjects(rw http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodGet:
		w.writeJSON(rw, http.StatusOK, w.projectsPayload())
	case http.MethodPost:
		w.changeProject(rw, req)
	default:
		rw.Header().Set("Allow", "GET, POST")
		w.writeJSON(rw, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "GET or POST required"})
	}
}

// handleProjectsAccess checks the bot's role per repository. It is separate
// from the list because it costs two GitHub requests per repo (eight seconds
// through a proxy for nine repos), and the page must not wait for it.
func (w *webServer) handleProjectsAccess(rw http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		rw.Header().Set("Allow", "GET")
		w.writeJSON(rw, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "GET required"})
		return
	}
	payload := projectsAccessPayload{Repos: make([]projectAccess, 0, len(w.cfg.Repos))}
	names := make([]string, 0, len(w.cfg.Repos))
	for _, repo := range w.cfg.Repos {
		names = append(names, repo.FullName)
	}
	results, err := w.checkRepoAccess(names)
	if err != nil {
		payload.Error = err.Error()
	}
	access := map[string]repoAccessInfo{}
	for _, result := range results {
		access[result.Repo] = result
	}
	for _, name := range names {
		info := access[name]
		payload.Repos = append(payload.Repos, projectAccess{
			Name:         name,
			Permission:   firstNonEmpty(info.Permission, "unknown"),
			CanPush:      info.Permission == "admin" || info.Permission == "write",
			CanAdmin:     info.Permission == "admin",
			WebhookReady: info.WebhookReady,
			Error:        info.Err,
		})
	}
	w.writeJSON(rw, http.StatusOK, payload)
}

func (w *webServer) projectsPayload() projectsPayload {
	payload := projectsPayload{
		Bot:     w.identity,
		Config:  w.cfg.File,
		Webhook: w.cfg.Webhook,
		Repos:   make([]webProject, 0, len(w.cfg.Repos)),
	}
	for _, repo := range w.cfg.Repos {
		meta := w.store.Meta(repo.FullName)
		base := ""
		if w.router != nil {
			if app := w.router.contexts[repo.FullName]; app != nil && app.Local != nil {
				base = app.Local.Base
			}
		}
		payload.Repos = append(payload.Repos, webProject{
			Name:      repo.FullName,
			Base:      base,
			Agent:     repo.CodingAgent,
			Model:     repo.Model,
			Mode:      repo.Mode,
			Approvers: repo.Approvers,
			HookID:    meta.HookID,
			Smee:      meta.SmeeChannel,
			LastPoll:  meta.LastPollAt,
			AccessURL: "https://github.com/" + repo.FullName + "/settings/access",
		})
	}
	return payload
}

type projectChange struct {
	Action string `json:"action"`
	Repo   string `json:"repo"`
}

func (w *webServer) changeProject(rw http.ResponseWriter, req *http.Request) {
	if !w.requireJSONPost(rw, req, 64<<10) {
		return
	}
	var change projectChange
	if err := json.NewDecoder(req.Body).Decode(&change); err != nil {
		w.writeJSON(rw, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON: " + err.Error()})
		return
	}
	name, err := normalizeRepoName(change.Repo)
	if err != nil {
		w.writeJSON(rw, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	path := w.cfg.File
	if path == "" {
		path = defaultConfigPath()
	}
	doc, err := readConfigDocument(path)
	if err != nil {
		w.writeJSON(rw, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	permission := ""
	switch strings.ToLower(strings.TrimSpace(change.Action)) {
	case "add":
		// Verify before writing: a typo in the name would otherwise sit in the
		// config as a repo the daemon retries forever.
		results, accessErr := w.checkRepoAccess([]string{name})
		if accessErr != nil {
			w.writeJSON(rw, http.StatusBadRequest, map[string]any{"ok": false, "error": accessErr.Error()})
			return
		}
		if len(results) == 1 {
			permission = results[0].Permission
			if results[0].Err != "" {
				w.writeJSON(rw, http.StatusBadRequest, map[string]any{"ok": false, "error": results[0].Err})
				return
			}
			if permission == "none" {
				w.writeJSON(rw, http.StatusBadRequest, map[string]any{
					"ok": false, "error": fmt.Sprintf("%s: the bot token cannot see this repository", name),
				})
				return
			}
		}
		if err := addRepoToDocument(doc, name); err != nil {
			w.writeJSON(rw, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
	case "remove":
		if err := removeRepoFromDocument(doc, name); err != nil {
			w.writeJSON(rw, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
	default:
		w.writeJSON(rw, http.StatusBadRequest, map[string]any{"ok": false, "error": `action must be "add" or "remove"`})
		return
	}
	status, writeErr := w.writeConfigDocument(doc)
	if writeErr != nil {
		w.writeJSON(rw, status, map[string]any{"ok": false, "error": writeErr.Error()})
		return
	}
	w.log.Info("web: %s project %s", strings.ToLower(change.Action), name)
	w.writeJSON(rw, http.StatusOK, map[string]any{
		"ok": true, "repo": name, "permission": permission, "restarting": w.reload != nil,
	})
	w.requestRestart()
}

// checkRepoAccess reports the bot's role per repository. Tests replace
// repoAccess so the payload never depends on GitHub.
func (w *webServer) checkRepoAccess(repos []string) ([]repoAccessInfo, error) {
	if len(repos) == 0 {
		return nil, nil
	}
	check := w.repoAccess
	if check == nil {
		check = wizardRepoCheck
	}
	proxy, err := parseProxy(w.cfg.Proxy)
	if err != nil {
		return nil, err
	}
	return check(proxy, repos)
}

// readConfigDocument returns the raw config object so unknown keys survive the
// round trip; a missing file is an empty document, which the loader then
// validates against the daemon defaults.
func readConfigDocument(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	doc := map[string]any{}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("config is not valid JSON: %v", err)
	}
	return doc, nil
}

// writeConfigDocument validates and saves a config object through the same
// path as the settings dialog: the daemon must accept what the page wrote.
func (w *webServer) writeConfigDocument(doc map[string]any) (int, error) {
	path := w.cfg.File
	if path == "" {
		path = defaultConfigPath()
	}
	if err := normalizeConfigDocument(doc, path); err != nil {
		return http.StatusBadRequest, err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return http.StatusInternalServerError, err
	}
	data = append(data, '\n')
	if err := atomicWriteFile(path, data); err != nil {
		return http.StatusInternalServerError, err
	}
	return http.StatusOK, nil
}

// repoNameOf reports the repository a repos[] element names, whichever shape
// that element has.
func repoNameOf(item any) string {
	switch typed := item.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]any:
		name, _ := typed["repo"].(string)
		return strings.TrimSpace(name)
	}
	return ""
}

// addRepoToDocument appends a repository and keeps the list's existing shape:
// a name list stays a name list, an object list gains one {repo} object, and a
// keyed object gains one empty entry.
func addRepoToDocument(doc map[string]any, name string) error {
	switch typed := doc["repos"].(type) {
	case nil:
		doc["repos"] = []any{name}
	case []any:
		objects := false
		for _, item := range typed {
			if repoNameOf(item) == name {
				return fmt.Errorf("%s is already watched", name)
			}
			if _, ok := item.(map[string]any); ok {
				objects = true
			}
		}
		if objects {
			doc["repos"] = append(typed, map[string]any{"repo": name})
		} else {
			doc["repos"] = append(typed, name)
		}
	case map[string]any:
		if _, exists := typed[name]; exists {
			return fmt.Errorf("%s is already watched", name)
		}
		typed[name] = map[string]any{}
		doc["repos"] = typed
	default:
		return errors.New("config.repos must be an array or object")
	}
	return nil
}

// removeRepoFromDocument deletes one repository and refuses to empty the list:
// the loader requires at least one repo.
func removeRepoFromDocument(doc map[string]any, name string) error {
	switch typed := doc["repos"].(type) {
	case []any:
		kept := make([]any, 0, len(typed))
		removed := false
		for _, item := range typed {
			if repoNameOf(item) == name {
				removed = true
				continue
			}
			kept = append(kept, item)
		}
		if !removed {
			return fmt.Errorf("%s is not watched", name)
		}
		if len(kept) == 0 {
			return errors.New("keep at least one project")
		}
		doc["repos"] = kept
	case map[string]any:
		if _, ok := typed[name]; !ok {
			return fmt.Errorf("%s is not watched", name)
		}
		delete(typed, name)
		if len(typed) == 0 {
			return errors.New("keep at least one project")
		}
		doc["repos"] = typed
	default:
		return errors.New("config.repos must be an array or object")
	}
	return nil
}
