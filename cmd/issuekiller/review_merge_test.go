package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// prLabelsFake serves the label endpoints a SetAgentState round trip needs and
// records every call; extra handles the endpoints of the test's scenario.
func prLabelsFake(t *testing.T, number int, labels []string, extra http.HandlerFunc) (*GH, *callLog) {
	t.Helper()
	return newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/labels"):
			payload := make([]map[string]string, 0, len(labels))
			for _, name := range labels {
				payload = append(payload, map[string]string{"name": name})
			}
			_ = json.NewEncoder(w).Encode(payload)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 4242})
		case extra != nil:
			extra(w, r)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
}

func reviewFixApp(t *testing.T, gh *GH) (*AppContext, *Store) {
	store := newTestStore(t)
	return &AppContext{
		Repo:       RepoConfig{FullName: "acme/app", Name: "app", Language: "en"},
		GH:         gh,
		Store:      store,
		Log:        NewSilentLogger(),
		AgentLogin: "bot",
	}, store
}

func TestFinishPRModifyAsksTheMergeAfterAReview(t *testing.T) {
	gh, calls := prLabelsFake(t, 5, []string{awaitingLabel}, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/acme/app/pulls/5" {
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 5, "state": "closed"})
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	app, _ := reviewFixApp(t, gh)

	finishPRModify(context.Background(), app, Trigger{Type: "reaction", IssueNumber: 5},
		PullRequest{Number: 5}, t.TempDir(), nil, true)

	var comment, released bool
	for _, call := range calls.snapshot() {
		if call.method == http.MethodPost && strings.HasSuffix(call.path, "/comments") {
			comment = true
			if !strings.Contains(call.body, app.text(5, "modify.done.merge_hint")) {
				t.Fatalf("comment does not promise the merge: %s", call.body)
			}
		}
		if call.method == http.MethodDelete && strings.Contains(call.path, "awaiting") {
			released = true
		}
	}
	if !comment {
		t.Fatal("no update comment posted")
	}
	// mergeReviewedPR sees a closed PR and clears the waiting label.
	if !released {
		t.Fatal("a merged/closed PR must not keep the awaiting label")
	}
}

func TestFinishPRModifyWithoutApprovalWaitsForTheMaintainer(t *testing.T) {
	gh, calls := prLabelsFake(t, 5, nil, nil)
	app, _ := reviewFixApp(t, gh)

	finishPRModify(context.Background(), app, Trigger{Type: "comment", IssueNumber: 5},
		PullRequest{Number: 5}, t.TempDir(), nil, false)

	var waits bool
	for _, call := range calls.snapshot() {
		if call.method == http.MethodPost && strings.HasSuffix(call.path, "/labels") {
			if strings.Contains(call.body, awaitingLabel) {
				waits = true
			}
		}
		if call.method == http.MethodGet && strings.HasSuffix(call.path, "/pulls/5") {
			t.Fatal("an instruction-only modify must not try to merge")
		}
	}
	if !waits {
		t.Fatal("an instruction-only modify should park the PR on the maintainer")
	}
}

func TestDoModifyPRRefusesAForkWithoutEdits(t *testing.T) {
	gh, calls := prLabelsFake(t, 5, nil, nil)
	app, _ := reviewFixApp(t, gh)
	pr := PullRequest{Number: 5}
	pr.Head.Repo.FullName = "someone/fork"

	if ok := doModifyPR(context.Background(), app, Trigger{IssueNumber: 5}, pr, "fix it", true); ok {
		t.Fatal("a fork without maintainer edits cannot be modified")
	}
	var commented, waiting bool
	for _, call := range calls.snapshot() {
		if call.method == http.MethodPost && strings.HasSuffix(call.path, "/comments") {
			commented = true
		}
		if call.method == http.MethodPost && strings.HasSuffix(call.path, "/labels") &&
			strings.Contains(call.body, awaitingLabel) {
			waiting = true
		}
	}
	if !commented || !waiting {
		t.Fatalf("refusal should comment and park the PR (comment=%v waiting=%v)", commented, waiting)
	}
}

func TestFinishMergedPRClosesTheTrackedIssue(t *testing.T) {
	gh, calls := prLabelsFake(t, 33, []string{awaitingLabel}, nil)
	app, store := reviewFixApp(t, gh)
	store.UpdateIssue("acme/app", 30, func(state *IssueState) {
		state.Phase = "blocked"
		state.PRNumber = 33
		state.BlockedReason = "push rejected"
	})
	store.UpdateIssue("acme/app", 33, func(state *IssueState) {
		state.Phase = ""
	})

	finishMergedPR(context.Background(), app, 33)

	st := store.Issue("acme/app", 30)
	if st.Phase != "done" || st.BlockedReason != "" {
		t.Fatalf("issue #30 after merge = %+v", st)
	}
	var clearedIssues int
	for _, call := range calls.snapshot() {
		if call.method == http.MethodDelete && strings.Contains(call.path, "/issues/30/labels/") &&
			strings.Contains(call.path, "awaiting") {
			clearedIssues++
		}
	}
	if clearedIssues == 0 {
		t.Fatal("the tracked issue label was not cleared")
	}
}
