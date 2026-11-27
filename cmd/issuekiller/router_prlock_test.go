package main

import (
	"context"
	"testing"
	"time"
)

// prLockRouter wires a router whose state already knows the PR of issue 30, so
// the issue job and the review-fix job target the same branch.
func prLockRouter(t *testing.T) *Router {
	t.Helper()
	store := newTestStore(t)
	store.UpdateIssue("acme/app", 30, func(state *IssueState) {
		state.PRNumber = 33
	})
	app := &AppContext{Repo: RepoConfig{FullName: "acme/app"}, Store: store, Log: NewSilentLogger()}
	return NewRouter(&Config{PlanParallel: 1, ImplementParallel: 2},
		map[string]*AppContext{"acme/app": app}, "bot", store, NewSilentLogger())
}

func TestWorkJobsOnOnePRRunOneAtATime(t *testing.T) {
	router := prLockRouter(t)
	started := make(chan string, 4)
	release := make(chan struct{})

	router.enqueue("acme/app", "acme/app#30", stageWork, func(context.Context) {
		started <- "issue"
		<-release
	})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the issue job never started")
	}

	router.enqueue("acme/app", "acme/app#33#reviewfix", stageWork, func(context.Context) {
		started <- "reviewfix"
	})
	select {
	case job := <-started:
		t.Fatalf("%s started while the issue revision still held the PR", job)
	case <-time.After(150 * time.Millisecond):
		/* serialized: correct */
	}

	close(release)
	select {
	case job := <-started:
		if job != "reviewfix" {
			t.Fatalf("unexpected job %q", job)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the review fix did not start after the revision finished")
	}
}

// A merge job waits for the issue's CI job, so the CI queue must not take the
// PR lock: with both taking it the merge would block on a job that can never
// start.
func TestCIJobsDoNotTakeThePRLock(t *testing.T) {
	router := prLockRouter(t)
	started := make(chan string, 4)
	release := make(chan struct{})

	router.enqueue("acme/app", "acme/app#30", stageWork, func(context.Context) {
		started <- "issue"
		<-release
	})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the issue job never started")
	}

	router.enqueue("acme/app", "acme/app#30#ci", stageCI, func(context.Context) {
		started <- "ci"
	})
	select {
	case job := <-started:
		if job != "ci" {
			t.Fatalf("unexpected job %q", job)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a CI settle job must not wait for the PR lock")
	}
	close(release)
}

func TestJobNumberReadsTheKey(t *testing.T) {
	cases := map[string]int{
		"acme/app#30":           30,
		"acme/app#33#reviewfix": 33,
		"acme/app#7#ci":         7,
		"acme/app":              0,
		"acme/app#x":            0,
	}
	for key, want := range cases {
		if got := jobNumber(key); got != want {
			t.Fatalf("jobNumber(%q) = %d, want %d", key, got, want)
		}
	}
}
