package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A pull request head may live in a fork under a different branch name, so the
// push has to take the local and remote refs separately.
func TestPushBranchToDifferentNames(t *testing.T) {
	ctx := context.Background()
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	work := filepath.Join(root, "work")
	mustGit := func(dir string, args ...string) {
		t.Helper()
		if _, err := gitWorktree(ctx, dir, env, 30*time.Second, false, args...); err != nil {
			t.Fatalf("git %v in %s: %v", args, dir, err)
		}
	}
	mustGit(root, "init", "-q", "--bare", remote)
	mustGit(root, "init", "-q", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(work, "add", "a.txt")
	mustGit(work, "commit", "-q", "-m", "one")

	if err := pushBranchTo(ctx, work, remote, env, "main", "feature-x"); err != nil {
		t.Fatalf("push local main to remote feature-x: %v", err)
	}
	if got := gitOutput(ctx, remote, env, time.Minute, "rev-parse", "--verify", "refs/heads/feature-x"); got == "" {
		t.Fatal("remote branch feature-x was not created")
	}
	if got := gitOutput(ctx, remote, env, time.Minute, "rev-parse", "--verify", "refs/heads/main"); got != "" {
		t.Fatal("push must not create the local branch name on the remote")
	}
}

// The review flow can push to a PR branch between two issue revisions; the
// revision must build on that head instead of failing its own push.
func TestSyncWorktreeBranchFastForwardsOrReportsDivergence(t *testing.T) {
	ctx := context.Background()
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	work := filepath.Join(root, "work")
	mustGit := func(dir string, args ...string) {
		t.Helper()
		if _, err := gitWorktree(ctx, dir, env, 30*time.Second, false, args...); err != nil {
			t.Fatalf("git %v in %s: %v", args, dir, err)
		}
	}
	writeFile := func(dir, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustGit(root, "init", "-q", "--bare", remote)
	mustGit(root, "init", "-q", "-b", "main", work)
	writeFile(work, "a.txt", "one\n")
	mustGit(work, "add", "a.txt")
	mustGit(work, "commit", "-q", "-m", "one")
	mustGit(work, "checkout", "-q", "-b", "feature")
	if err := pushBranchTo(ctx, work, remote, env, "feature", "feature"); err != nil {
		t.Fatalf("seed remote branch: %v", err)
	}

	// Another writer (the review fix) advances the branch.
	other := filepath.Join(root, "other")
	mustGit(root, "clone", "-q", remote, other)
	mustGit(other, "checkout", "-q", "feature")
	writeFile(other, "b.txt", "two\n")
	mustGit(other, "add", "b.txt")
	mustGit(other, "commit", "-q", "-m", "two")
	mustGit(other, "push", "-q", "origin", "feature")

	if err := syncWorktreeBranch(ctx, work, remote, env, "feature"); err != nil {
		t.Fatalf("fast-forward: %v", err)
	}
	want := gitOutput(ctx, other, env, time.Minute, "rev-parse", "HEAD")
	if got := gitOutput(ctx, work, env, time.Minute, "rev-parse", "HEAD"); got != want {
		t.Fatalf("worktree head = %s, want %s", got, want)
	}
	if _, err := os.Stat(filepath.Join(work, "b.txt")); err != nil {
		t.Fatal("fast-forward did not bring the pushed file into the worktree")
	}

	// A local commit plus a different remote commit cannot fast-forward.
	writeFile(work, "c.txt", "local\n")
	mustGit(work, "add", "c.txt")
	mustGit(work, "commit", "-q", "-m", "local")
	writeFile(other, "d.txt", "remote\n")
	mustGit(other, "add", "d.txt")
	mustGit(other, "commit", "-q", "-m", "remote")
	mustGit(other, "push", "-q", "origin", "feature")
	if err := syncWorktreeBranch(ctx, work, remote, env, "feature"); err == nil {
		t.Fatal("a diverged branch must be reported instead of merged or force-pushed")
	}
}
