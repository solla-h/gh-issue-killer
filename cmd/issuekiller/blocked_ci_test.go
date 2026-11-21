package main

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChecksAllGreen(t *testing.T) {
	ok := conclusion("success")
	skip := conclusion("skipped")
	fail := conclusion("failure")
	now := time.Now()

	cases := []struct {
		name string
		runs CheckRuns
		want bool
	}{
		{"empty", CheckRuns{}, false},
		{"all green", CheckRuns{CheckRuns: []CheckRun{
			testCheckRun("a", "completed", ok, now, &now),
			testCheckRun("b", "completed", skip, now, &now),
		}}, true},
		{"one failure", CheckRuns{CheckRuns: []CheckRun{
			testCheckRun("a", "completed", ok, now, &now),
			testCheckRun("b", "completed", fail, now, &now),
		}}, false},
		{"still running", CheckRuns{CheckRuns: []CheckRun{
			testCheckRun("a", "in_progress", nil, now, &now),
		}}, false},
	}
	for _, tc := range cases {
		if got := checksAllGreen(tc.runs); got != tc.want {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A task parked by a failed CI check remains eligible until its merge job
// actually runs, so a dropped job can be recovered by a later scan.
func TestRetryBlockedCIRetriesAfterDroppedJob(t *testing.T) {
	store := &Store{file: filepath.Join(t.TempDir(), "state.json"), state: State{Version: 1, Repos: map[string]*RepoState{}}}
	store.UpdateIssue("acme/app", 5, func(st *IssueState) {
		st.Phase = "blocked"
		st.BlockedReason = "test: failure"
		st.BlockedByCI = true
		st.PRNumber = 50
	})
	gh, _ := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/app/issues":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"number": 5, "state": "open"}})
		case r.URL.Path == "/repos/acme/app/pulls/50":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 50, "state": "open", "mergeable": true, "mergeable_state": "clean",
				"head": map[string]any{"sha": "abc123"},
				"base": map[string]any{"ref": "master"},
			})
		case strings.HasSuffix(r.URL.Path, "/commits/abc123/check-runs"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_count": 1,
				"check_runs": []map[string]any{
					{"name": "test", "status": "completed", "conclusion": "success"},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	app := &AppContext{
		Repo:  RepoConfig{FullName: "acme/app", Approvers: []string{"owner"}},
		GH:    gh,
		Store: store,
		Log:   NewSilentLogger(),
	}
	// A missing pool drops enqueue, as can a process exit before its queued job
	// starts. The persisted flag must survive for a newly constructed router.
	first := &Router{
		store: store, log: NewSilentLogger(), pools: map[string]*repoPool{},
		active: map[string]bool{}, queued: map[string]bool{}, cancels: map[string]context.CancelFunc{},
	}
	first.retryBlockedCI(context.Background(), app)
	if got := store.Issue("acme/app", 5).BlockedByCI; !got {
		t.Fatal("BlockedByCI was cleared before the merge job ran")
	}

	pool := &repoPool{plan: make(chan routerJob, 1), work: make(chan routerJob, 1), ci: make(chan routerJob, 1)}
	second := &Router{
		store: store, log: NewSilentLogger(),
		pools:  map[string]*repoPool{"acme/app": pool},
		active: map[string]bool{}, queued: map[string]bool{}, cancels: map[string]context.CancelFunc{},
	}
	second.retryBlockedCI(context.Background(), app)
	if len(pool.work) != 1 {
		t.Fatalf("next router scan queued %d jobs, want 1", len(pool.work))
	}
	if got := store.Issue("acme/app", 5).BlockedByCI; !got {
		t.Fatal("BlockedByCI was cleared when retry was only queued")
	}
}

// A blocked task whose reason is not CI (for example local checks) is left for
// the approver even when the PR is green.
func TestRetryBlockedCISkipsOtherReasons(t *testing.T) {
	store := &Store{file: filepath.Join(t.TempDir(), "state.json"), state: State{Version: 1, Repos: map[string]*RepoState{}}}
	store.UpdateIssue("acme/app", 6, func(st *IssueState) {
		st.Phase = "blocked"
		st.BlockedReason = "checks failed"
		st.PRNumber = 60
	})
	gh, calls := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"number": 6, "state": "open"}})
	})
	app := &AppContext{
		Repo:  RepoConfig{FullName: "acme/app"},
		GH:    gh,
		Store: store,
		Log:   NewSilentLogger(),
	}
	router := NewRouter(&Config{PlanParallel: 1, ImplementParallel: 1},
		map[string]*AppContext{"acme/app": app}, "bot", store, NewSilentLogger())

	router.retryBlockedCI(context.Background(), app)
	for _, call := range calls.snapshot() {
		if strings.Contains(call.path, "/pulls/") || strings.Contains(call.path, "/check-runs") {
			t.Fatalf("unexpected call for a non-CI block: %+v", call)
		}
	}
}
