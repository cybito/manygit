package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitCmd runs a git command in dir and fails the test on error.
func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// initRepo creates a repo with one commit on the default branch "master".
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitCmd(t, dir, "init", "-q", "-b", "master")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".")
	gitCmd(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func TestStatus_CleanNoUpstream(t *testing.T) {
	dir := initRepo(t)
	st := Status(dir)
	if st.Err != nil {
		t.Fatalf("unexpected err: %v", st.Err)
	}
	if st.Branch != "master" {
		t.Errorf("branch = %q, want master", st.Branch)
	}
	if st.HasUpstream {
		t.Errorf("HasUpstream = true, want false")
	}
	if st.DirtyCount != 0 {
		t.Errorf("DirtyCount = %d, want 0", st.DirtyCount)
	}
}

// A local-only repo (no remote at all) and a repo whose branch merely lacks an
// upstream are different states — the status column shows them differently.
func TestStatus_HasRemote(t *testing.T) {
	local := initRepo(t)
	if st := Status(local); st.HasRemote || st.HasUpstream {
		t.Errorf("local-only repo: HasRemote=%v HasUpstream=%v, want false/false", st.HasRemote, st.HasUpstream)
	}

	clone, _ := initRepoWithRemote(t)
	if st := Status(clone); !st.HasRemote || !st.HasUpstream {
		t.Errorf("cloned repo: HasRemote=%v HasUpstream=%v, want true/true", st.HasRemote, st.HasUpstream)
	}
	// A brand-new branch in that same repo: it HAS a remote, it just isn't pushed.
	gitCmd(t, clone, "checkout", "-q", "-b", "wip")
	if st := Status(clone); !st.HasRemote || st.HasUpstream {
		t.Errorf("unpushed branch: HasRemote=%v HasUpstream=%v, want true/false", st.HasRemote, st.HasUpstream)
	}
}

func TestStatus_Dirty(t *testing.T) {
	dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := Status(dir)
	if st.DirtyCount != 1 {
		t.Errorf("DirtyCount = %d, want 1", st.DirtyCount)
	}
}

// DiscardTracked reverts modified tracked files but keeps untracked ones.
func TestDiscardTracked(t *testing.T) {
	dir := initRepo(t)
	// modify a tracked file and create an untracked one
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := DiscardTracked(dir); err != nil {
		t.Fatalf("DiscardTracked: %v", err)
	}
	// tracked file restored
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "hello\n" {
		t.Errorf("a.txt = %q, want it reverted to %q", b, "hello\n")
	}
	// untracked file kept
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); err != nil {
		t.Errorf("untracked new.txt should survive DiscardTracked: %v", err)
	}
}

// DiscardAll reverts tracked files AND removes untracked ones.
func TestDiscardAll(t *testing.T) {
	dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := DiscardAll(dir); err != nil {
		t.Fatalf("DiscardAll: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "hello\n" {
		t.Errorf("a.txt = %q, want reverted", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); !os.IsNotExist(err) {
		t.Errorf("untracked new.txt should be deleted by DiscardAll")
	}
	if st := Status(dir); st.DirtyCount != 0 {
		t.Errorf("repo should be clean after DiscardAll, DirtyCount=%d", st.DirtyCount)
	}
}

// tagAt creates an annotated tag with a fixed tagger date so creatordate
// ordering is deterministic in tests.
func tagAt(t *testing.T, dir, date, name, msg string) {
	t.Helper()
	cmd := exec.Command("git", "tag", "-a", name, "-m", msg)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
		"GIT_COMMITTER_DATE="+date, "GIT_AUTHOR_DATE="+date,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git tag %s: %v\n%s", name, err, out)
	}
}

// Tags returns the repo's tags newest-first with their metadata.
func TestTags(t *testing.T) {
	dir := initRepo(t)
	if tags, err := Tags(dir, 10); err != nil || len(tags) != 0 {
		t.Fatalf("a fresh repo should have no tags, got %+v err %v", tags, err)
	}
	tagAt(t, dir, "2026-01-01T00:00:00", "v0.1.0", "first release")
	tagAt(t, dir, "2026-06-01T00:00:00", "v0.2.0", "second release")

	tags, err := Tags(dir, 10)
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if len(tags) != 2 {
		t.Fatalf("want 2 tags, got %d: %+v", len(tags), tags)
	}
	if tags[0].Name != "v0.2.0" || tags[1].Name != "v0.1.0" {
		t.Errorf("tags should be newest-first, got %q then %q", tags[0].Name, tags[1].Name)
	}
	if tags[0].Subject != "second release" {
		t.Errorf("subject = %q, want %q", tags[0].Subject, "second release")
	}
	if tags[0].Hash == "" || tags[0].Date != "2026-06-01" {
		t.Errorf("tag missing hash or wrong date: %+v", tags[0])
	}
}

// initRepoWithRemote creates a bare "origin", a clone with one pushed commit,
// and returns (clone, bare).
func initRepoWithRemote(t *testing.T) (clone, bare string) {
	t.Helper()
	bare = t.TempDir()
	gitCmd(t, bare, "init", "-q", "--bare", "-b", "master")

	clone = t.TempDir()
	gitCmd(t, clone, "init", "-q", "-b", "master")
	gitCmd(t, clone, "config", "user.name", "test")
	gitCmd(t, clone, "config", "user.email", "test@test")
	gitCmd(t, clone, "remote", "add", "origin", bare)
	if err := os.WriteFile(filepath.Join(clone, "a.txt"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, clone, "add", ".")
	gitCmd(t, clone, "commit", "-q", "-m", "c1")
	gitCmd(t, clone, "push", "-q", "-u", "origin", "master")
	return clone, bare
}

// advanceOrigin clones bare, adds a commit, and pushes it.
func advanceOrigin(t *testing.T, bare string) {
	advanceOriginFile(t, bare, "b.txt", "2\n", "c2")
}

func advanceOriginFile(t *testing.T, bare, name, content, message string) {
	t.Helper()
	other := t.TempDir()
	gitCmd(t, other, "clone", "-q", bare, ".")
	if err := os.WriteFile(filepath.Join(other, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, other, "add", ".")
	gitCmd(t, other, "commit", "-q", "-m", message)
	gitCmd(t, other, "push", "-q", "origin", "master")
}

func TestFetch_DetectsBehind(t *testing.T) {
	clone, bare := initRepoWithRemote(t)
	advanceOrigin(t, bare)
	if err := Fetch(clone); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if st := Status(clone); st.Behind != 1 {
		t.Errorf("Behind = %d, want 1", st.Behind)
	}
}

func TestSync_BehindFastForwardsAndPushes(t *testing.T) {
	clone, bare := initRepoWithRemote(t)
	advanceOrigin(t, bare)
	if err := Sync(clone); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if st := Status(clone); st.Ahead != 0 || st.Behind != 0 {
		t.Errorf("status after sync = ahead %d, behind %d; want 0/0", st.Ahead, st.Behind)
	}
	head, err := run(clone, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	remote, err := run(bare, "rev-parse", "refs/heads/master")
	if err != nil {
		t.Fatal(err)
	}
	if head != remote {
		t.Errorf("remote head = %s, local head = %s", remote, head)
	}
}

func TestSync_DivergedMergesAndPushes(t *testing.T) {
	clone, bare := initRepoWithRemote(t)
	if err := os.WriteFile(filepath.Join(clone, "local.txt"), []byte("local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, clone, "add", ".")
	gitCmd(t, clone, "commit", "-q", "-m", "local")
	advanceOrigin(t, bare)
	if err := Sync(clone); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	parents, err := run(clone, "rev-list", "--parents", "-n", "1", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Fields(parents)); got != 3 {
		t.Errorf("HEAD parent fields = %d, want 3 for a merge commit: %q", got, parents)
	}
	head, err := run(clone, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	remote, err := run(bare, "rev-parse", "refs/heads/master")
	if err != nil {
		t.Fatal(err)
	}
	if head != remote {
		t.Errorf("remote head = %s, local head = %s", remote, head)
	}
}

func TestSync_ConflictDoesNotPush(t *testing.T) {
	clone, bare := initRepoWithRemote(t)
	if err := os.WriteFile(filepath.Join(clone, "a.txt"), []byte("local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, clone, "add", "a.txt")
	gitCmd(t, clone, "commit", "-q", "-m", "local conflict")
	advanceOriginFile(t, bare, "a.txt", "remote\n", "remote conflict")
	before, err := run(bare, "rev-parse", "refs/heads/master")
	if err != nil {
		t.Fatal(err)
	}
	if err := Sync(clone); err == nil {
		t.Fatal("Sync should fail on a merge conflict")
	}
	after, err := run(bare, "rev-parse", "refs/heads/master")
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("remote moved after conflict: before %s, after %s", before, after)
	}
}

func TestPush_ClearsAhead(t *testing.T) {
	clone, _ := initRepoWithRemote(t)
	if err := os.WriteFile(filepath.Join(clone, "c.txt"), []byte("3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, clone, "add", ".")
	gitCmd(t, clone, "commit", "-q", "-m", "c3")

	if before := Status(clone); before.Ahead != 1 {
		t.Fatalf("Ahead before push = %d, want 1", before.Ahead)
	}
	if err := Push(clone); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if after := Status(clone); after.Ahead != 0 {
		t.Errorf("Ahead after push = %d, want 0", after.Ahead)
	}
}

func TestBranches_ListsLocalAndCurrent(t *testing.T) {
	dir := initRepo(t)
	gitCmd(t, dir, "branch", "feature")

	branches, err := Branches(dir)
	if err != nil {
		t.Fatalf("Branches: %v", err)
	}
	var sawMaster, sawFeature, masterCurrent bool
	for _, b := range branches {
		switch b.Name {
		case "master":
			sawMaster, masterCurrent = true, b.IsCurrent
		case "feature":
			sawFeature = true
		}
	}
	if !sawMaster || !sawFeature {
		t.Errorf("expected master and feature, got %+v", branches)
	}
	if !masterCurrent {
		t.Errorf("master should be current")
	}
}

// A remote-only branch whose name contains slashes (feat/…, fix/…) must be
// listed and checked out under its FULL name: dropping everything up to the last
// slash would try to check out a nonexistent "aisuite-onboarding".
func TestBranches_RemoteWithSlashesCheckedOutWhole(t *testing.T) {
	clone, bare := initRepoWithRemote(t)
	other := t.TempDir()
	gitCmd(t, other, "clone", "-q", bare, ".")
	gitCmd(t, other, "checkout", "-q", "-b", "feat/aisuite-onboarding")
	gitCmd(t, other, "push", "-q", "origin", "feat/aisuite-onboarding")
	if err := Fetch(clone); err != nil {
		t.Fatal(err)
	}

	branches, err := Branches(clone)
	if err != nil {
		t.Fatalf("Branches: %v", err)
	}
	var target Branch
	for _, b := range branches {
		if b.Name == "origin/feat/aisuite-onboarding" {
			target = b
		}
	}
	if !target.IsRemote {
		t.Fatalf("origin/feat/aisuite-onboarding not listed, got %+v", branches)
	}
	if got := target.LocalName(); got != "feat/aisuite-onboarding" {
		t.Fatalf("LocalName() = %q, want feat/aisuite-onboarding", got)
	}
	if err := Checkout(clone, target.LocalName()); err != nil {
		t.Fatalf("Checkout of a remote-only branch: %v", err)
	}
	if st := Status(clone); st.Branch != "feat/aisuite-onboarding" {
		t.Errorf("branch = %q, want feat/aisuite-onboarding", st.Branch)
	}
}

func TestCheckout_SwitchesBranch(t *testing.T) {
	dir := initRepo(t)
	gitCmd(t, dir, "branch", "feature")
	if err := Checkout(dir, "feature"); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if st := Status(dir); st.Branch != "feature" {
		t.Errorf("branch = %q, want feature", st.Branch)
	}
}

func TestStatusFiles(t *testing.T) {
	dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := StatusFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]string{}
	for _, f := range files {
		byPath[f.Path] = f.Status
	}
	if byPath["a.txt"] != "M" {
		t.Errorf("a.txt status = %q, want M", byPath["a.txt"])
	}
	if byPath["new.txt"] != "??" {
		t.Errorf("new.txt status = %q, want ??", byPath["new.txt"])
	}
}

func TestCommitFiles(t *testing.T) {
	dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".")
	gitCmd(t, dir, "commit", "-q", "-m", "add b")
	files, err := CommitFiles(dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "b.txt" || files[0].Status != "A" {
		t.Errorf("CommitFiles = %+v, want [{A b.txt}]", files)
	}
}

func TestGraphLogEntries(t *testing.T) {
	dir := initRepo(t)
	lines, commits, err := GraphLogEntries(dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) == 0 || len(commits) == 0 {
		t.Fatalf("expected lines and commits, got %d lines, %d commits", len(lines), len(commits))
	}
	for _, c := range commits {
		if c.Line < 0 || c.Line >= len(lines) {
			t.Errorf("commit line index %d out of range", c.Line)
		}
		if len(c.Hash) < 7 {
			t.Errorf("bad hash %q", c.Hash)
		}
	}
}

func TestFileDiffs(t *testing.T) {
	dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wd, err := WorkingFileDiff(dir, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(wd, "\n"), "world") {
		t.Errorf("working diff should show added 'world': %v", wd)
	}
	cd, err := CommitFileDiff(dir, "HEAD", "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(cd, "\n"), "hello") {
		t.Errorf("commit diff should show 'hello': %v", cd)
	}
}

func TestWorkingFileDiff_Untracked(t *testing.T) {
	dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("brand\nnew\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wd, err := WorkingFileDiff(dir, "new.txt")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(wd, "\n")
	if !strings.Contains(joined, "new file") || !strings.Contains(joined, "brand") {
		t.Errorf("untracked file diff should show its added content, got:\n%s", joined)
	}
}

func TestWorkingFileDiff_NoCommits(t *testing.T) {
	dir := t.TempDir()
	gitCmd(t, dir, "init", "-q", "-b", "main") // no commits -> no HEAD
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hello there\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wd, err := WorkingFileDiff(dir, "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(wd, "\n"), "hello there") {
		t.Errorf("a new file in a commitless repo should still show its content: %v", wd)
	}
}

func TestRecentCommits(t *testing.T) {
	dir := initRepo(t) // -b master, one commit "init", just made
	if ref := MainRef(dir); ref != "master" {
		t.Fatalf("MainRef = %q, want master", ref)
	}
	all, err := RecentCommits(dir, "master", 5, "")
	if err != nil || len(all) != 1 || all[0] != "init" {
		t.Fatalf("RecentCommits(no window) = %v, %v", all, err)
	}
	recent, err := RecentCommits(dir, "master", 5, "1 day ago") // window includes the fresh commit
	if err != nil || len(recent) != 1 {
		t.Errorf("RecentCommits(1 day) = %v, %v", recent, err)
	}
	if got, _ := RecentCommits(dir, "", 5, ""); len(got) != 0 {
		t.Errorf("an empty ref should return nothing, got %v", got)
	}

	// a commit dated long ago on main is excluded by a short window
	old := t.TempDir()
	gitCmd(t, old, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(old, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, old, "add", ".")
	c := exec.Command("git", "commit", "-q", "-m", "ancient")
	c.Dir = old
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_AUTHOR_DATE=2020-01-01T00:00:00", "GIT_COMMITTER_DATE=2020-01-01T00:00:00")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
	if ref := MainRef(old); ref != "main" {
		t.Errorf("MainRef(old) = %q, want main", ref)
	}
	if got, _ := RecentCommits(old, "main", 5, "3 days ago"); len(got) != 0 {
		t.Errorf("a 3-day window should exclude a 2020 commit, got %v", got)
	}
	if got, _ := RecentCommits(old, "main", 5, ""); len(got) != 1 {
		t.Errorf("no window should include the old commit, got %v", got)
	}

	// n <= 0 means "no count limit" — every commit in the window is returned,
	// while a positive n still caps the count.
	multi := initRepo(t) // "init"
	for _, msg := range []string{"c2", "c3", "c4"} {
		if err := os.WriteFile(filepath.Join(multi, msg+".txt"), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, multi, "add", ".")
		gitCmd(t, multi, "commit", "-q", "-m", msg)
	}
	if got, _ := RecentCommits(multi, "master", 0, ""); len(got) != 4 {
		t.Errorf("n=0 should return all 4 commits, got %d: %v", len(got), got)
	}
	if got, _ := RecentCommits(multi, "master", 2, ""); len(got) != 2 {
		t.Errorf("n=2 should still cap at 2 commits, got %d", len(got))
	}
}

func TestGraphLog_ReturnsCommits(t *testing.T) {
	dir := initRepo(t)
	lines, err := GraphLog(dir, 10)
	if err != nil {
		t.Fatalf("GraphLog: %v", err)
	}
	if len(lines) == 0 {
		t.Fatalf("expected at least one log line")
	}
	if !strings.Contains(strings.Join(lines, "\n"), "init") {
		t.Errorf("log should mention the 'init' commit, got:\n%s", strings.Join(lines, "\n"))
	}
}

// Fingerprint is the cheap "did this repo move?" probe the TUI polls while a
// script runs, so it can re-stat only the repos a script actually touched
// instead of all of them (a full Status() sweep is 6-8 git subprocesses per
// repo). It must move for every git-level change a script can make.
func TestFingerprint_DetectsGitOperations(t *testing.T) {
	clone, bare := initRepoWithRemote(t)
	gitCmd(t, clone, "branch", "feature")

	prev := Fingerprint(clone)
	if prev == 0 {
		t.Fatal("Fingerprint of a real repo should be non-zero")
	}

	steps := []struct {
		name string
		do   func()
	}{
		{"checkout a branch", func() { gitCmd(t, clone, "checkout", "-q", "feature") }},
		{"create a lightweight tag", func() { gitCmd(t, clone, "tag", "v1.2.3") }},
		{"commit", func() {
			if err := os.WriteFile(filepath.Join(clone, "n.txt"), []byte("n\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, clone, "add", ".")
			gitCmd(t, clone, "commit", "-q", "-m", "n")
		}},
		{"pack refs", func() { gitCmd(t, clone, "pack-refs", "--all") }},
		{"create a branch", func() { gitCmd(t, clone, "branch", "another") }},
		{"fetch an advanced origin", func() {
			advanceOrigin(t, bare)
			if err := Fetch(clone); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, s := range steps {
		s.do()
		got := Fingerprint(clone)
		if got == prev {
			t.Errorf("Fingerprint did not move after %s — the repo's row would stay stale", s.name)
		}
		prev = got
	}
}

// Nothing changed means no re-stat. If the fingerprint drifted on its own, every
// poll would spawn a full Status() for every repo and the probe would be worse
// than the sweep it replaces.
func TestFingerprint_StableWhenNothingChanges(t *testing.T) {
	dir := initRepo(t)
	first := Fingerprint(dir)
	for i := 0; i < 5; i++ {
		if got := Fingerprint(dir); got != first {
			t.Fatalf("call %d returned %d, want the stable %d", i+2, got, first)
		}
	}
}

// Deliberately git-level only: it stats a fixed handful of paths inside .git and
// never walks the working tree, which is what makes it ~350x cheaper than
// Status(). The cost is that a script editing files without running git (an
// `npm install`, say) changes the dirty count invisibly — which is exactly why
// the TUI also does one full re-stat when the script finishes. Widening this to
// catch working-tree edits would mean walking every repo's files on every poll.
func TestFingerprint_IgnoresWorkingTreeEdits(t *testing.T) {
	dir := initRepo(t)
	before := Fingerprint(dir)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Fingerprint(dir); got != before {
		t.Errorf("Fingerprint moved on a working-tree-only edit (%d -> %d); "+
			"the probe is meant to stay git-level and cheap", before, got)
	}
}

// In a linked worktree .git is a FILE holding "gitdir: <path>", not a directory.
// Stat'ing that file alone would produce a fingerprint that never moves, so the
// worktree's row would silently never refresh — worse than not probing at all,
// because nothing signals that it's stale.
func TestFingerprint_LinkedWorktree(t *testing.T) {
	main := initRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	gitCmd(t, main, "worktree", "add", "-q", "-b", "side", wt)

	fi, err := os.Stat(filepath.Join(wt, ".git"))
	if err != nil || fi.IsDir() {
		t.Fatalf("expected .git to be a file in a linked worktree, got dir=%v err=%v", fi != nil && fi.IsDir(), err)
	}
	before := Fingerprint(wt)
	if before == 0 {
		t.Fatal("Fingerprint of a linked worktree should be non-zero")
	}
	gitCmd(t, wt, "checkout", "-q", "-b", "side2")
	if got := Fingerprint(wt); got == before {
		t.Error("Fingerprint did not move after a checkout in a linked worktree")
	}
}

// A path that isn't a repo (deleted mid-run, or never one) returns 0 rather than
// erroring — the probe runs on every repo every tick and must not be a failure
// path.
func TestFingerprint_MissingRepoIsZero(t *testing.T) {
	if got := Fingerprint(filepath.Join(t.TempDir(), "nope")); got != 0 {
		t.Errorf("Fingerprint of a missing repo = %d, want 0", got)
	}
	if got := Fingerprint(t.TempDir()); got != 0 {
		t.Errorf("Fingerprint of a non-repo directory = %d, want 0", got)
	}
}

func TestParseSlug(t *testing.T) {
	cases := map[string]string{
		"https://github.com/rabeeh-ta/manygit.git": "rabeeh-ta/manygit",
		"https://github.com/rabeeh-ta/manygit":     "rabeeh-ta/manygit",
		"git@github.com:blend-ed/frontend-app.git": "blend-ed/frontend-app",
		"git@github.com:blend-ed/frontend-app":     "blend-ed/frontend-app",
		"ssh://git@github.com/o/r.git":             "o/r",
		"https://user:tok@github.com/o/r.git":      "o/r",
		"https://github.com/":                      "",
		"not a url":                                "",
		"":                                         "",
	}
	for in, want := range cases {
		if got := parseSlug(in); got != want {
			t.Errorf("parseSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

// RemoteSlug reads the origin URL; a repo with no origin yields ("", err).
func TestRemoteSlug(t *testing.T) {
	dir := initRepo(t)
	if slug, err := RemoteSlug(dir); err == nil {
		t.Errorf("no-origin repo should error, got slug %q", slug)
	}
	gitCmd(t, dir, "remote", "add", "origin", "git@github.com:acme/widgets.git")
	slug, err := RemoteSlug(dir)
	if err != nil {
		t.Fatalf("RemoteSlug: %v", err)
	}
	if slug != "acme/widgets" {
		t.Errorf("RemoteSlug = %q, want acme/widgets", slug)
	}
}
