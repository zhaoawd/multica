package repocache

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.Default()
}

func TestGitEnv(t *testing.T) {
	t.Parallel()
	env := gitEnv()

	// Must contain GIT_TERMINAL_PROMPT=0.
	found := false
	for _, entry := range env {
		if entry == "GIT_TERMINAL_PROMPT=0" {
			found = true
			break
		}
	}
	if !found {
		t.Error("gitEnv() must include GIT_TERMINAL_PROMPT=0")
	}

	// Must contain HOME from the current environment.
	home := os.Getenv("HOME")
	if home == "" {
		t.Skip("HOME not set in test environment")
	}
	foundHome := false
	for _, entry := range env {
		if entry == "HOME="+home {
			foundHome = true
			break
		}
	}
	if !foundHome {
		t.Error("gitEnv() must include HOME from os.Environ()")
	}

	// Must set safe.directory=* via GIT_CONFIG env vars.
	envHas := func(env []string, want string) bool {
		for _, e := range env {
			if e == want {
				return true
			}
		}
		return false
	}
	if !envHas(env, "GIT_CONFIG_KEY_0=safe.directory") {
		t.Error("gitEnv() must include GIT_CONFIG_KEY_0=safe.directory (no pre-existing config)")
	}
	if !envHas(env, "GIT_CONFIG_VALUE_0=*") {
		t.Error("gitEnv() must include GIT_CONFIG_VALUE_0=*")
	}
}

func TestGitEnvPreservesExistingConfig(t *testing.T) {
	// GIT_CONFIG_COUNT env vars are process-wide; cannot use t.Setenv in
	// parallel tests, so run sequentially.
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "url.https://github.com/.insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "gh:")
	t.Setenv("GIT_CONFIG_KEY_1", "http.extraHeader")
	t.Setenv("GIT_CONFIG_VALUE_1", "Authorization: Bearer tok")

	env := gitEnv()

	envHas := func(want string) bool {
		for _, e := range env {
			if e == want {
				return true
			}
		}
		return false
	}

	// safe.directory must be appended at index 2 (next available).
	if !envHas("GIT_CONFIG_COUNT=3") {
		t.Error("expected GIT_CONFIG_COUNT=3")
	}
	if !envHas("GIT_CONFIG_KEY_2=safe.directory") {
		t.Error("expected GIT_CONFIG_KEY_2=safe.directory")
	}
	if !envHas("GIT_CONFIG_VALUE_2=*") {
		t.Error("expected GIT_CONFIG_VALUE_2=*")
	}

	// Original entries must still be present.
	if !envHas("GIT_CONFIG_KEY_0=url.https://github.com/.insteadOf") {
		t.Error("existing GIT_CONFIG_KEY_0 was lost")
	}
	if !envHas("GIT_CONFIG_VALUE_0=gh:") {
		t.Error("existing GIT_CONFIG_VALUE_0 was lost")
	}
	if !envHas("GIT_CONFIG_KEY_1=http.extraHeader") {
		t.Error("existing GIT_CONFIG_KEY_1 was lost")
	}
}

func TestBareDirName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input, want string
	}{
		{"https://github.com/org/my-repo.git", "github.com+org+my-repo.git"},
		{"https://github.com/org/my-repo", "github.com+org+my-repo.git"},
		{"git@github.com:org/my-repo.git", "github.com+org+my-repo.git"},
		{"git@github.com:org/my-repo", "github.com+org+my-repo.git"},
		{"https://github.com/org/repo/", "github.com+org+repo.git"},
		{"ssh://git@gitlab.example.com:22/group/sub/repo.git", "gitlab.example.com%3A22+group+sub+repo.git"},
		// Basename collision: two repos sharing the basename must produce
		// distinct dirs (the original bug).
		{"ssh://git@gitlab.example.com:22/relisty/app.git", "gitlab.example.com%3A22+relisty+app.git"},
		{"ssh://git@gitlab.example.com:22/listbridge/app.git", "gitlab.example.com%3A22+listbridge+app.git"},
		{"my-repo", "my-repo.git"},
		{"", "repo.git"},
	}
	for _, tt := range tests {
		if got := bareDirName(tt.input); got != tt.want {
			t.Errorf("bareDirName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// TestBareDirNameDistinctsSegmentBoundaryColliders covers the collision class
// that a naive path-flattening-with-dashes scheme would miss: two repos whose
// path segments differ only at a segment boundary flatten to the same string
// once slashes become dashes. The '+' separator can't appear inside a
// GitHub/GitLab path segment, so the boundary stays visible in the output.
func TestBareDirNameDistinctsSegmentBoundaryColliders(t *testing.T) {
	t.Parallel()
	pairs := [][2]string{
		{"git@github.com:foo/bar-baz.git", "git@github.com:foo-bar/baz.git"},
		{"https://github.com/foo/bar-baz.git", "https://github.com/foo-bar/baz.git"},
	}
	for _, p := range pairs {
		a, b := bareDirName(p[0]), bareDirName(p[1])
		if a == b {
			t.Errorf("bareDirName collision: %q and %q both → %q", p[0], p[1], a)
		}
	}
}

// TestBareDirNameDistinctsSameRepoNameAcrossHosts covers the cross-host
// collision class: the same path-with-namespace on different hosts must
// produce distinct cache dirs so an agent configured for host A can't be
// served the clone from host B.
func TestBareDirNameDistinctsSameRepoNameAcrossHosts(t *testing.T) {
	t.Parallel()
	pairs := [][2]string{
		{"git@github.com:org/repo.git", "git@gitlab.example.com:org/repo.git"},
		{"https://github.com/org/repo.git", "https://gitlab.example.com/org/repo.git"},
		{"ssh://git@github.com/org/repo.git", "ssh://git@gitlab.example.com/org/repo.git"},
	}
	for _, p := range pairs {
		a, b := bareDirName(p[0]), bareDirName(p[1])
		if a == b {
			t.Errorf("bareDirName collision across hosts: %q and %q both → %q", p[0], p[1], a)
		}
	}
}

// TestBareDirNameDistinctsHostPortFromDashedHostname covers the lossy-port
// encoding regression: a naive ':' -> '-' rewrite would collapse
// `host:port` onto a hostname that literally contains the same dash pattern,
// silently reintroducing the wrong-remote bug. We URL-encode ':' to '%3A'
// so host+port is lossless — and '%' is forbidden in valid hostnames so the
// marker can never come from a legal literal hostname.
func TestBareDirNameDistinctsHostPortFromDashedHostname(t *testing.T) {
	t.Parallel()
	pairs := [][2]string{
		// Host-with-port vs a literal hostname that looks like `host-port`.
		{"ssh://git@gitlab.example.com:22/org/repo.git", "git@gitlab.example.com-22:org/repo.git"},
		// Same again but across the URL and scp-style forms, explicit ports
		// swapped to ensure we don't rely on order.
		{"ssh://git@host.example.com:443/a/b.git", "git@host.example.com-443:a/b.git"},
	}
	for _, p := range pairs {
		a, b := bareDirName(p[0]), bareDirName(p[1])
		if a == b {
			t.Errorf("bareDirName collision between host:port and host-port: %q and %q both → %q", p[0], p[1], a)
		}
	}
}

func TestIsBareRepo(t *testing.T) {
	t.Parallel()

	// A directory with a HEAD file should be detected as bare.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	if !isBareRepo(dir) {
		t.Error("expected bare repo to be detected")
	}

	// An empty directory should not.
	emptyDir := t.TempDir()
	if isBareRepo(emptyDir) {
		t.Error("expected empty dir to not be detected as bare repo")
	}
}

// createTestRepo creates a local git repo with an initial commit and returns its path.
func createTestRepo(t *testing.T) string {
	t.Helper()
	return createTestRepoAt(t, t.TempDir())
}

// createTestRepoAt initializes a git repo at the given directory (which
// must already exist). Used to craft repo URLs at paths chosen by the test
// — e.g. to reproduce collision classes in name derivation.
func createTestRepoAt(t *testing.T, dir string) string {
	t.Helper()
	for _, args := range [][]string{
		{"init", dir},
		{"-C", dir, "commit", "--allow-empty", "-m", "initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git setup failed: %s: %v", out, err)
		}
	}
	return dir
}

func TestSyncAndLookup(t *testing.T) {
	t.Parallel()
	sourceRepo := createTestRepo(t)
	cacheRoot := t.TempDir()

	cache := New(cacheRoot, testLogger())

	// Sync should clone the repo.
	err := cache.Sync("ws-123", []RepoInfo{
		{URL: sourceRepo},
	})
	if err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	// Lookup should find the cached repo.
	path := cache.Lookup("ws-123", sourceRepo)
	if path == "" {
		t.Fatal("expected to find cached repo")
	}
	if !isBareRepo(path) {
		t.Fatalf("expected bare repo at %s", path)
	}

	// Lookup for unknown URL should return empty.
	if got := cache.Lookup("ws-123", "https://github.com/org/unknown"); got != "" {
		t.Fatalf("expected empty for unknown URL, got %q", got)
	}

	// Lookup for unknown workspace should return empty.
	if got := cache.Lookup("ws-999", sourceRepo); got != "" {
		t.Fatalf("expected empty for unknown workspace, got %q", got)
	}
}

// TestSyncAndCheckoutForFileURL covers the local-repo path: a `file:///abs`
// URL must skip the bare-clone step entirely and produce a worktree whose
// new branch lives in the user's own repository, not in any intermediate
// cache. This is the "Claude Code-like" workflow — the agent's branch is
// immediately visible to the user via `git branch` in their checkout.
func TestSyncAndCheckoutForFileURL(t *testing.T) {
	t.Parallel()
	sourcePath := createTestRepo(t)
	fileURL := "file://" + sourcePath
	cacheRoot := t.TempDir()
	cache := New(cacheRoot, testLogger())

	if err := cache.Sync("ws-local", []RepoInfo{{URL: fileURL}}); err != nil {
		t.Fatalf("Sync(file://) failed: %v", err)
	}

	// Sync MUST NOT create a bare clone for local repos — the whole point
	// of the file:// path is to avoid the round-trip and have task branches
	// land directly in the user's repo. A leftover bare clone here would
	// regress us back to the old behavior.
	wsCacheDir := filepath.Join(cacheRoot, "ws-local")
	if entries, err := os.ReadDir(wsCacheDir); err == nil && len(entries) > 0 {
		t.Fatalf("expected no cache dirs for file:// URL, got %d entries under %s", len(entries), wsCacheDir)
	}

	// Lookup must return the user's repo path itself, not a bare-clone path.
	gitRoot := cache.Lookup("ws-local", fileURL)
	if gitRoot != sourcePath {
		t.Fatalf("Lookup(file://) = %q, want %q", gitRoot, sourcePath)
	}
	if isBareRepo(gitRoot) {
		t.Fatalf("expected user repo (non-bare), got bare at %s", gitRoot)
	}

	workDir := t.TempDir()
	result, err := cache.CreateWorktree(WorktreeParams{
		WorkspaceID: "ws-local",
		RepoURL:     fileURL,
		WorkDir:     workDir,
		AgentName:   "local-agent",
		TaskID:      "task-abcdef12",
	})
	if err != nil {
		t.Fatalf("CreateWorktree(file://) failed: %v", err)
	}
	if !isGitWorktree(result.Path) {
		t.Errorf("expected git worktree at %s", result.Path)
	}

	// The new branch must be visible inside the user's source repo — this
	// is the artifact handoff: user reviews `agent/...` branch with their
	// own tools, no fetch/push hops required.
	branches := runGitOutput(t, sourcePath, "branch", "--list", result.BranchName)
	if !strings.Contains(branches, result.BranchName) {
		t.Fatalf("branch %q missing from user repo %s; `git branch` returned: %s",
			result.BranchName, sourcePath, strings.TrimSpace(branches))
	}

	// The worktree's git common-dir must point back at the user's repo,
	// not at any cache. Anything else would mean we accidentally cloned.
	// Resolve symlinks before comparing — macOS routes /var/folders/... to
	// /private/var/folders/..., and `git rev-parse` emits the realpath while
	// t.TempDir() returns the symlinked form. A naive string compare would
	// flag a passing run as a regression.
	commonDir := strings.TrimSpace(runGitOutput(t, result.Path, "rev-parse", "--git-common-dir"))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(result.Path, commonDir)
	}
	wantCommon := filepath.Join(sourcePath, ".git")
	gotResolved := mustEvalSymlinks(t, commonDir)
	wantResolved := mustEvalSymlinks(t, wantCommon)
	if gotResolved != wantResolved {
		t.Errorf("worktree git-common-dir = %q, want %q", gotResolved, wantResolved)
	}
}

func mustEvalSymlinks(t *testing.T, p string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", p, err)
	}
	return resolved
}

// runGitOutput executes git -C dir args... and returns combined stdout,
// failing the test on error. Used by tests that need to read git state
// the daemon mutated indirectly.
func runGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s failed: %s: %v", args, dir, strings.TrimSpace(string(out)), err)
	}
	return string(out)
}

// TestCreateWorktreeForFileURLRespectsCurrentHEAD verifies the local-repo
// base-ref resolver: when the user has a non-default branch checked out,
// the agent's new branch must be created off that branch (the user's
// working state), not silently rerouted to refs/heads/main behind their
// back. This is the property that makes the local mode feel like
// "Claude Code on my current branch".
func TestCreateWorktreeForFileURLRespectsCurrentHEAD(t *testing.T) {
	t.Parallel()
	sourcePath := createTestRepo(t)
	// Switch the source repo onto a feature branch with a commit unique to it.
	runGitAuthored(t, sourcePath, "checkout", "-b", "feat/local-agent")
	if err := os.WriteFile(filepath.Join(sourcePath, "feature.txt"), []byte("on feat\n"), 0o644); err != nil {
		t.Fatalf("write feature.txt: %v", err)
	}
	runGitAuthored(t, sourcePath, "add", ".")
	runGitAuthored(t, sourcePath, "commit", "-m", "feature commit")

	fileURL := "file://" + sourcePath
	cache := New(t.TempDir(), testLogger())
	if err := cache.Sync("ws-local", []RepoInfo{{URL: fileURL}}); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	result, err := cache.CreateWorktree(WorktreeParams{
		WorkspaceID: "ws-local",
		RepoURL:     fileURL,
		WorkDir:     t.TempDir(),
		AgentName:   "local-agent",
		TaskID:      "feat-resolve-test",
	})
	if err != nil {
		t.Fatalf("CreateWorktree failed: %v", err)
	}

	// The new branch should contain the feature commit — proving it was
	// based on feat/local-agent, not on master/main.
	if _, err := os.Stat(filepath.Join(result.Path, "feature.txt")); err != nil {
		t.Fatalf("worktree should contain feature.txt from user's checked-out branch: %v", err)
	}
}

// TestCreateWorktreeForFileURLLeavesUserHooksAlone verifies that creating a
// worktree off the user's local repo does NOT install daemon-owned hooks
// in the user's own .git/hooks dir. The Co-authored-by hook (or any other
// daemon-installed hook) is fine in an isolated bare cache the daemon owns
// outright, but the user's repo is not the daemon's to mutate.
func TestCreateWorktreeForFileURLLeavesUserHooksAlone(t *testing.T) {
	t.Parallel()
	sourcePath := createTestRepo(t)
	fileURL := "file://" + sourcePath
	cache := New(t.TempDir(), testLogger())
	if err := cache.Sync("ws-local", []RepoInfo{{URL: fileURL}}); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	_, err := cache.CreateWorktree(WorktreeParams{
		WorkspaceID:         "ws-local",
		RepoURL:             fileURL,
		WorkDir:             t.TempDir(),
		AgentName:           "local-agent",
		TaskID:              "hook-isolation-test",
		CoAuthoredByEnabled: true,
	})
	if err != nil {
		t.Fatalf("CreateWorktree failed: %v", err)
	}

	hookPath := filepath.Join(sourcePath, ".git", "hooks", "prepare-commit-msg")
	if _, err := os.Stat(hookPath); err == nil {
		t.Fatalf("daemon installed a hook in user's repo at %s — must skip for file:// URLs", hookPath)
	}
}

// TestSyncRejectsNonGitFileURL verifies that pointing a file:// URL at a
// directory that isn't a git repository fails Sync explicitly, instead of
// silently succeeding and then failing later inside `git worktree add`
// with a confusing message.
func TestSyncRejectsNonGitFileURL(t *testing.T) {
	t.Parallel()
	notARepo := t.TempDir()
	cache := New(t.TempDir(), testLogger())
	err := cache.Sync("ws-local", []RepoInfo{{URL: "file://" + notARepo}})
	if err == nil {
		t.Fatal("expected Sync to fail for non-git directory")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("expected 'not a git repository' in error, got: %v", err)
	}
}

// TestSyncKeepsDistinctCachesForSegmentBoundaryColliders proves that two
// URLs differing only at a path-segment boundary don't share a bare cache
// and don't silently reuse each other's origin. Both conditions would have
// failed under a plain slashes-to-dashes flattening scheme: the two URLs
// in this test produce the same dash-joined key even though they point at
// different source repositories.
func TestSyncKeepsDistinctCachesForSegmentBoundaryColliders(t *testing.T) {
	t.Parallel()

	// Build two real source repos under a shared parent. Their filesystem
	// paths are used directly as URLs (git accepts local paths as remote
	// URLs). The path pair ".../foo/bar-baz" and ".../foo-bar/baz" would
	// flatten to the same string under slashes-to-dashes — that's the
	// class of collision we want to rule out.
	parent := t.TempDir()
	srcA := filepath.Join(parent, "foo", "bar-baz")
	srcB := filepath.Join(parent, "foo-bar", "baz")
	if err := os.MkdirAll(srcA, 0o755); err != nil {
		t.Fatalf("mkdir srcA: %v", err)
	}
	if err := os.MkdirAll(srcB, 0o755); err != nil {
		t.Fatalf("mkdir srcB: %v", err)
	}
	createTestRepoAt(t, srcA)
	createTestRepoAt(t, srcB)
	// Distinct content so a silent-reuse bug would produce the wrong file
	// in the wrong cache.
	if err := os.WriteFile(filepath.Join(srcA, "A.txt"), []byte("A\n"), 0o644); err != nil {
		t.Fatalf("write A: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcB, "B.txt"), []byte("B\n"), 0o644); err != nil {
		t.Fatalf("write B: %v", err)
	}
	runGitAuthored(t, srcA, "add", ".")
	runGitAuthored(t, srcA, "commit", "-m", "A-content")
	runGitAuthored(t, srcB, "add", ".")
	runGitAuthored(t, srcB, "commit", "-m", "B-content")

	cache := New(t.TempDir(), testLogger())
	if err := cache.Sync("ws-1", []RepoInfo{{URL: srcA}, {URL: srcB}}); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	pathA := cache.Lookup("ws-1", srcA)
	pathB := cache.Lookup("ws-1", srcB)
	if pathA == "" || pathB == "" {
		t.Fatalf("missing cache entry: A=%q B=%q", pathA, pathB)
	}
	if pathA == pathB {
		t.Fatalf("collider URLs share a bare cache path: %s", pathA)
	}

	// Each bare cache must carry the origin URL of the repo it was
	// cloned from — not the other one's. A silent-reuse bug would have
	// both caches pointing at whichever URL won the race in Sync.
	if got := gitConfigGet(t, pathA, "remote.origin.url"); got != srcA {
		t.Errorf("cacheA origin.url = %q, want %q", got, srcA)
	}
	if got := gitConfigGet(t, pathB, "remote.origin.url"); got != srcB {
		t.Errorf("cacheB origin.url = %q, want %q", got, srcB)
	}

	// And each cache's content must reflect the right source.
	if !cachedRepoHasFile(t, pathA, "A.txt") {
		t.Errorf("cacheA (%s) should contain A.txt from srcA", pathA)
	}
	if !cachedRepoHasFile(t, pathB, "B.txt") {
		t.Errorf("cacheB (%s) should contain B.txt from srcB", pathB)
	}
}

// gitConfigGet reads a git config value from repoPath. Fails the test if
// the key is missing or the command errors.
func gitConfigGet(t *testing.T, repoPath, key string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repoPath, "config", "--get", key).Output()
	if err != nil {
		t.Fatalf("git config --get %s in %s: %v", key, repoPath, err)
	}
	return strings.TrimSpace(string(out))
}

// cachedRepoHasFile returns true if the bare cache at barePath exposes a
// file named filename anywhere in its remote-tracking default branch.
// Walks refs/remotes/origin/* since a bare clone stores fetched heads
// there under the modern refspec.
func cachedRepoHasFile(t *testing.T, barePath, filename string) bool {
	t.Helper()
	ref := getRemoteDefaultBranch(barePath)
	if ref == "" {
		return false
	}
	out, err := exec.Command("git", "-C", barePath, "ls-tree", "-r", "--name-only", ref).Output()
	if err != nil {
		t.Fatalf("git ls-tree %s in %s: %v", ref, barePath, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == filename {
			return true
		}
	}
	return false
}

func TestSyncFetchesExisting(t *testing.T) {
	t.Parallel()
	sourceRepo := createTestRepo(t)
	cacheRoot := t.TempDir()

	cache := New(cacheRoot, testLogger())

	// First sync: clone.
	if err := cache.Sync("ws-1", []RepoInfo{{URL: sourceRepo}}); err != nil {
		t.Fatalf("first sync failed: %v", err)
	}

	// Record the remote-tracking default head in the cache. Under the modern
	// refspec layout, fetches write to refs/remotes/origin/*, not the bare
	// repo's own refs/heads/*, so reading the bare HEAD would return the
	// fossil snapshot from initial clone.
	barePath := cache.Lookup("ws-1", sourceRepo)
	oldHead := gitRefCommit(t, barePath, getRemoteDefaultBranch(barePath))

	// Add a commit to source.
	addEmptyCommit(t, sourceRepo, "second")
	sourceHead := gitHead(t, sourceRepo)
	if sourceHead == oldHead {
		t.Fatal("source HEAD should differ after new commit")
	}

	// Second sync: should fetch (not re-clone).
	if err := cache.Sync("ws-1", []RepoInfo{{URL: sourceRepo}}); err != nil {
		t.Fatalf("second sync failed: %v", err)
	}

	// Verify the cache remote-tracking ref was updated.
	newHead := gitRefCommit(t, barePath, getRemoteDefaultBranch(barePath))
	if newHead == oldHead {
		t.Fatal("expected cache remote-tracking head to be updated after fetch")
	}
	if newHead != sourceHead {
		t.Fatalf("expected cache head %s to match source head %s", newHead, sourceHead)
	}
}

func gitHead(t *testing.T, repoPath string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", repoPath, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD failed in %s: %v", repoPath, err)
	}
	return strings.TrimSpace(string(out))
}

func TestWorktreeFromCache(t *testing.T) {
	t.Parallel()
	sourceRepo := createTestRepo(t)
	cacheRoot := t.TempDir()

	cache := New(cacheRoot, testLogger())
	if err := cache.Sync("ws-1", []RepoInfo{{URL: sourceRepo}}); err != nil {
		t.Fatalf("sync failed: %v", err)
	}

	barePath := cache.Lookup("ws-1", sourceRepo)
	if barePath == "" {
		t.Fatal("expected cached repo")
	}

	// Create a worktree from the bare cache — this is the actual use case.
	worktreeDir := filepath.Join(t.TempDir(), "work")
	cmd := exec.Command("git", "-C", barePath, "worktree", "add", "-b", "test-branch", worktreeDir, "HEAD")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("worktree add failed: %s: %v", out, err)
	}
	defer exec.Command("git", "-C", barePath, "worktree", "remove", "--force", worktreeDir).Run()

	// Verify worktree exists and is on the right branch.
	cmd = exec.Command("git", "-C", worktreeDir, "branch", "--show-current")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("show branch failed: %v", err)
	}
	if got := trimLine(string(out)); got != "test-branch" {
		t.Fatalf("expected branch 'test-branch', got %q", got)
	}
}

func TestCreateWorktree(t *testing.T) {
	t.Parallel()
	sourceRepo := createTestRepo(t)
	cacheRoot := t.TempDir()

	cache := New(cacheRoot, testLogger())
	if err := cache.Sync("ws-1", []RepoInfo{{URL: sourceRepo}}); err != nil {
		t.Fatalf("sync failed: %v", err)
	}

	workDir := t.TempDir()
	result, err := cache.CreateWorktree(WorktreeParams{
		WorkspaceID: "ws-1",
		RepoURL:     sourceRepo,
		WorkDir:     workDir,
		AgentName:   "Code Reviewer",
		TaskID:      "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
	})
	if err != nil {
		t.Fatalf("CreateWorktree failed: %v", err)
	}

	// Verify the worktree was created.
	if _, err := os.Stat(result.Path); os.IsNotExist(err) {
		t.Fatalf("worktree path does not exist: %s", result.Path)
	}

	// Verify branch name format.
	if !strings.HasPrefix(result.BranchName, "agent/code-reviewer/") {
		t.Errorf("expected branch to start with 'agent/code-reviewer/', got %q", result.BranchName)
	}

	// Verify the worktree is on the correct branch.
	cmd := exec.Command("git", "-C", result.Path, "branch", "--show-current")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("show branch failed: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != result.BranchName {
		t.Errorf("expected branch %q, got %q", result.BranchName, got)
	}
}

func TestCreateWorktreeExcludesOpenCodeSkills(t *testing.T) {
	t.Parallel()
	sourceRepo := createTestRepo(t)
	cacheRoot := t.TempDir()

	cache := New(cacheRoot, testLogger())
	if err := cache.Sync("ws-1", []RepoInfo{{URL: sourceRepo}}); err != nil {
		t.Fatalf("sync failed: %v", err)
	}

	workDir := t.TempDir()
	result, err := cache.CreateWorktree(WorktreeParams{
		WorkspaceID: "ws-1",
		RepoURL:     sourceRepo,
		WorkDir:     workDir,
		AgentName:   "OpenCode",
		TaskID:      "opencode-exclude-test",
	})
	if err != nil {
		t.Fatalf("CreateWorktree failed: %v", err)
	}

	exclude := gitInfoExclude(t, result.Path)
	if !strings.Contains(exclude, ".opencode\n") {
		t.Fatalf("expected .git/info/exclude to contain .opencode, got:\n%s", exclude)
	}
	if strings.Contains(exclude, ".config/opencode") {
		t.Fatalf("expected .git/info/exclude to not contain stale .config/opencode, got:\n%s", exclude)
	}
}

func gitInfoExclude(t *testing.T, worktreePath string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", worktreePath, "rev-parse", "--git-dir")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse --git-dir failed in %s: %v", worktreePath, err)
	}
	gitDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(worktreePath, gitDir)
	}
	data, err := os.ReadFile(filepath.Join(gitDir, "info", "exclude"))
	if err != nil {
		t.Fatalf("read .git/info/exclude failed: %v", err)
	}
	return string(data)
}

func TestCreateWorktreeNotCached(t *testing.T) {
	t.Parallel()
	cacheRoot := t.TempDir()
	cache := New(cacheRoot, testLogger())

	_, err := cache.CreateWorktree(WorktreeParams{
		WorkspaceID: "ws-1",
		RepoURL:     "https://github.com/org/nonexistent",
		WorkDir:     t.TempDir(),
		AgentName:   "Agent",
		TaskID:      "test-task-id",
	})
	if err == nil {
		t.Fatal("expected error for uncached repo")
	}
	if !strings.Contains(err.Error(), "not found in cache") {
		t.Errorf("expected 'not found in cache' error, got: %v", err)
	}
}

func TestCreateWorktreeWithRequestedBranchRef(t *testing.T) {
	t.Parallel()
	sourceRepo := createTestRepo(t)
	defaultHead := gitHead(t, sourceRepo)

	runGitAuthored(t, sourceRepo, "checkout", "-b", "review-branch")
	if err := os.WriteFile(filepath.Join(sourceRepo, "review.txt"), []byte("review\n"), 0o644); err != nil {
		t.Fatalf("write review file: %v", err)
	}
	runGitAuthored(t, sourceRepo, "add", ".")
	runGitAuthored(t, sourceRepo, "commit", "-m", "review branch commit")
	reviewHead := gitHead(t, sourceRepo)
	if reviewHead == defaultHead {
		t.Fatal("test setup failed: review branch did not advance")
	}

	cache := New(t.TempDir(), testLogger())
	if err := cache.Sync("ws-1", []RepoInfo{{URL: sourceRepo}}); err != nil {
		t.Fatalf("sync failed: %v", err)
	}

	result, err := cache.CreateWorktree(WorktreeParams{
		WorkspaceID: "ws-1",
		RepoURL:     sourceRepo,
		WorkDir:     t.TempDir(),
		Ref:         "review-branch",
		AgentName:   "Reviewer",
		TaskID:      "review-task-id",
	})
	if err != nil {
		t.Fatalf("CreateWorktree failed: %v", err)
	}

	if got := gitHead(t, result.Path); got != reviewHead {
		t.Fatalf("worktree HEAD = %s, want requested branch head %s", got, reviewHead)
	}
	if _, err := os.Stat(filepath.Join(result.Path, "review.txt")); err != nil {
		t.Fatalf("requested branch file missing: %v", err)
	}
}

func TestCreateWorktreeWithRequestedCommitRef(t *testing.T) {
	t.Parallel()
	sourceRepo := createTestRepo(t)
	firstCommit := gitHead(t, sourceRepo)
	addEmptyCommit(t, sourceRepo, "second commit")

	cache := New(t.TempDir(), testLogger())
	if err := cache.Sync("ws-1", []RepoInfo{{URL: sourceRepo}}); err != nil {
		t.Fatalf("sync failed: %v", err)
	}

	result, err := cache.CreateWorktree(WorktreeParams{
		WorkspaceID: "ws-1",
		RepoURL:     sourceRepo,
		WorkDir:     t.TempDir(),
		Ref:         firstCommit,
		AgentName:   "Reviewer",
		TaskID:      "commit-task-id",
	})
	if err != nil {
		t.Fatalf("CreateWorktree failed: %v", err)
	}

	if got := gitHead(t, result.Path); got != firstCommit {
		t.Fatalf("worktree HEAD = %s, want requested commit %s", got, firstCommit)
	}
}

func TestCreateWorktreeWithRequestedTagRef(t *testing.T) {
	t.Parallel()
	sourceRepo := createTestRepo(t)
	taggedCommit := gitHead(t, sourceRepo)
	runGitAuthored(t, sourceRepo, "tag", "v1")
	// Advance the default branch past the tag so worktree HEAD == taggedCommit
	// can only be true if the tag was actually resolved (vs falling back to
	// the default branch tip).
	addEmptyCommit(t, sourceRepo, "post-tag commit")

	cache := New(t.TempDir(), testLogger())
	if err := cache.Sync("ws-1", []RepoInfo{{URL: sourceRepo}}); err != nil {
		t.Fatalf("sync failed: %v", err)
	}

	result, err := cache.CreateWorktree(WorktreeParams{
		WorkspaceID: "ws-1",
		RepoURL:     sourceRepo,
		WorkDir:     t.TempDir(),
		Ref:         "v1",
		AgentName:   "Reviewer",
		TaskID:      "tag-task-id",
	})
	if err != nil {
		t.Fatalf("CreateWorktree failed: %v", err)
	}

	if got := gitHead(t, result.Path); got != taggedCommit {
		t.Fatalf("worktree HEAD = %s, want tagged commit %s", got, taggedCommit)
	}
}

func TestCreateWorktreeWithUnknownRequestedRef(t *testing.T) {
	t.Parallel()
	sourceRepo := createTestRepo(t)
	cache := New(t.TempDir(), testLogger())
	if err := cache.Sync("ws-1", []RepoInfo{{URL: sourceRepo}}); err != nil {
		t.Fatalf("sync failed: %v", err)
	}

	_, err := cache.CreateWorktree(WorktreeParams{
		WorkspaceID: "ws-1",
		RepoURL:     sourceRepo,
		WorkDir:     t.TempDir(),
		Ref:         "missing-ref",
		AgentName:   "Reviewer",
		TaskID:      "missing-ref-task-id",
	})
	if err == nil {
		t.Fatal("expected unknown ref error")
	}
	if !strings.Contains(err.Error(), "cannot resolve requested ref") {
		t.Fatalf("expected requested ref error, got: %v", err)
	}
}

func trimLine(s string) string {
	return strings.TrimSpace(s)
}

// gitRefCommit resolves a git ref to its commit SHA in repoPath.
func gitRefCommit(t *testing.T, repoPath, ref string) string {
	t.Helper()
	if ref == "" {
		t.Fatalf("empty ref in %s", repoPath)
	}
	cmd := exec.Command("git", "-C", repoPath, "rev-parse", ref)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse %s failed in %s: %v", ref, repoPath, err)
	}
	return strings.TrimSpace(string(out))
}

// addEmptyCommit adds an empty commit on the current branch of repoPath.
func addEmptyCommit(t *testing.T, repoPath, message string) {
	t.Helper()
	cmd := exec.Command("git", "-C", repoPath, "commit", "--allow-empty", "-m", message)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit failed in %s: %s: %v", repoPath, out, err)
	}
}

// runGitAuthored runs `git -C repoPath <args...>` with the test author env set.
func runGitAuthored(t *testing.T, repoPath string, args ...string) {
	t.Helper()
	full := append([]string{"-C", repoPath}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %s: %v", args, repoPath, out, err)
	}
}

// TestCreateWorktreeFetchesDespiteAgentBranchOnRemote reproduces the original
// stale-cache bug. Under the legacy mirror refspec (+refs/heads/*:refs/heads/*)
// the sequence below would break on the second CreateWorktree because `git
// fetch` tries to overwrite refs/heads/agent/... which is locked by the first
// worktree, and the whole fetch aborts — silently discarding the main-branch
// update too. Under the modern remote-tracking refspec, fetched heads land in
// refs/remotes/origin/* and no longer collide with worktree-locked refs.
func TestCreateWorktreeFetchesDespiteAgentBranchOnRemote(t *testing.T) {
	t.Parallel()
	sourceRepo := createTestRepo(t)
	// Capture the default branch BEFORE any detach/commit/checkout dance — we
	// need its name later to add new commits to the correct branch.
	defaultBranch := currentBranchName(t, sourceRepo)

	// Put source repo on a detached HEAD so the first worktree's agent branch
	// can be pushed back to it as a regular update (non-bare repos refuse to
	// push to the currently checked-out branch).
	runGitAuthored(t, sourceRepo, "checkout", "--detach", "HEAD")

	cacheRoot := t.TempDir()
	cache := New(cacheRoot, testLogger())
	if err := cache.Sync("ws-1", []RepoInfo{{URL: sourceRepo}}); err != nil {
		t.Fatalf("sync failed: %v", err)
	}

	// First worktree creates refs/heads/agent/... inside the bare cache.
	workDir1 := t.TempDir()
	result1, err := cache.CreateWorktree(WorktreeParams{
		WorkspaceID: "ws-1",
		RepoURL:     sourceRepo,
		WorkDir:     workDir1,
		AgentName:   "agent",
		TaskID:      "t1111111-0000-0000-0000-000000000000",
	})
	if err != nil {
		t.Fatalf("first CreateWorktree failed: %v", err)
	}

	// Simulate the agent pushing its branch back to origin (i.e. opening a PR).
	// Now sourceRepo has refs/heads/agent/... matching the locked ref in the
	// bare cache, which is the condition that triggered the legacy bug.
	if err := os.WriteFile(filepath.Join(result1.Path, "hello.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	runGitAuthored(t, result1.Path, "add", ".")
	runGitAuthored(t, result1.Path, "commit", "-m", "first task")
	runGitAuthored(t, result1.Path, "push", "origin", result1.BranchName)

	// Add a new commit to source's default branch (not the agent branch we
	// just pushed). Then re-detach so future pushes to other branches still work.
	runGitAuthored(t, sourceRepo, "checkout", defaultBranch)
	addEmptyCommit(t, sourceRepo, "new commit on default branch")
	sourceHead := gitRefCommit(t, sourceRepo, "refs/heads/"+defaultBranch)
	runGitAuthored(t, sourceRepo, "checkout", "--detach", "HEAD")

	// Second worktree: CreateWorktree fetches first. Under the legacy refspec
	// this fetch would fail (refusing to fetch into locked refs/heads/agent/...)
	// and the worktree would be based on the stale snapshot. Under the modern
	// refspec this succeeds and the new worktree sees sourceHead.
	workDir2 := t.TempDir()
	result2, err := cache.CreateWorktree(WorktreeParams{
		WorkspaceID: "ws-1",
		RepoURL:     sourceRepo,
		WorkDir:     workDir2,
		AgentName:   "agent",
		TaskID:      "t2222222-0000-0000-0000-000000000000",
	})
	if err != nil {
		t.Fatalf("second CreateWorktree failed: %v", err)
	}

	if got := gitHead(t, result2.Path); got != sourceHead {
		t.Fatalf("second worktree HEAD = %s, want %s (remote default head after new commit)", got, sourceHead)
	}
}

// currentBranchName returns the branch name that HEAD points at in repoPath.
// Fails the test if HEAD is detached.
func currentBranchName(t *testing.T, repoPath string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repoPath, "symbolic-ref", "--short", "HEAD").Output()
	if err != nil {
		t.Fatalf("symbolic-ref --short HEAD in %s: %v", repoPath, err)
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		t.Fatalf("empty branch name in %s", repoPath)
	}
	return name
}

// TestEnsureRemoteTrackingLayoutMigratesLegacyCache verifies that a cache
// created with the legacy mirror refspec is migrated in place on next use:
// the refspec is rewritten to the modern remote-tracking layout and
// refs/remotes/origin/* gets backfilled so getRemoteDefaultBranch can resolve
// the remote default.
func TestEnsureRemoteTrackingLayoutMigratesLegacyCache(t *testing.T) {
	t.Parallel()
	sourceRepo := createTestRepo(t)
	cacheRoot := t.TempDir()
	cache := New(cacheRoot, testLogger())
	if err := cache.Sync("ws-1", []RepoInfo{{URL: sourceRepo}}); err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	barePath := cache.Lookup("ws-1", sourceRepo)

	// Reset to the legacy mirror refspec to simulate a cache created by an
	// older version of the daemon.
	if err := setFetchRefspec(barePath, "+refs/heads/*:refs/heads/*"); err != nil {
		t.Fatalf("set legacy refspec: %v", err)
	}
	// Wipe any refs/remotes/origin/* that may have been populated by the initial clone.
	_ = exec.Command("git", "-C", barePath, "update-ref", "-d", "refs/remotes/origin/HEAD").Run()
	if err := exec.Command("sh", "-c", "rm -rf '"+filepath.Join(barePath, "refs", "remotes")+"'").Run(); err != nil {
		t.Fatalf("wipe refs/remotes: %v", err)
	}

	// Sanity check: we've successfully forced the cache into legacy state.
	if cur, _ := readFetchRefspec(barePath); cur != "+refs/heads/*:refs/heads/*" {
		t.Fatalf("precondition failed: refspec is %q, want legacy mirror", cur)
	}

	// ensureRemoteTrackingLayout should migrate: rewrite refspec, backfill
	// refs/remotes/origin/*, and set origin HEAD.
	if err := ensureRemoteTrackingLayout(barePath); err != nil {
		t.Fatalf("ensureRemoteTrackingLayout failed: %v", err)
	}

	cur, err := readFetchRefspec(barePath)
	if err != nil {
		t.Fatalf("read refspec after migration: %v", err)
	}
	if cur != modernFetchRefspec {
		t.Errorf("refspec = %q, want %q", cur, modernFetchRefspec)
	}

	// getRemoteDefaultBranch should now return a refs/remotes/origin/<branch>.
	ref := getRemoteDefaultBranch(barePath)
	if !strings.HasPrefix(ref, "refs/remotes/origin/") {
		t.Errorf("getRemoteDefaultBranch = %q, want refs/remotes/origin/*", ref)
	}
}

// TestCreateWorktreePathCollisionDoesNotLeakBranch verifies the secondary bug
// fix: when the worktree path already exists as a non-worktree (e.g. a plain
// directory), createWorktree must fail cleanly without leaking a branch into
// the bare repo. Previously the "already exists" retry logic would
// misclassify path collisions as branch collisions and create a second
// timestamp-suffixed branch before hitting the same path error.
func TestCreateWorktreePathCollisionDoesNotLeakBranch(t *testing.T) {
	t.Parallel()
	sourceRepo := createTestRepo(t)
	cacheRoot := t.TempDir()
	cache := New(cacheRoot, testLogger())
	if err := cache.Sync("ws-1", []RepoInfo{{URL: sourceRepo}}); err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	barePath := cache.Lookup("ws-1", sourceRepo)

	// Pre-create the target worktree path as a plain non-empty directory.
	workDir := t.TempDir()
	dirName := repoNameFromURL(sourceRepo)
	worktreePath := filepath.Join(workDir, dirName)
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatalf("pre-create worktree path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write stray file: %v", err)
	}

	_, err := cache.CreateWorktree(WorktreeParams{
		WorkspaceID: "ws-1",
		RepoURL:     sourceRepo,
		WorkDir:     workDir,
		AgentName:   "agent",
		TaskID:      "t1111111-0000-0000-0000-000000000000",
	})
	if err == nil {
		t.Fatal("expected CreateWorktree to fail when path exists as non-worktree")
	}

	// No agent/* branches should have been created in the bare repo as a
	// side effect of the failed call.
	out, runErr := exec.Command("git", "-C", barePath, "for-each-ref", "--format=%(refname)", "refs/heads/agent").Output()
	if runErr != nil {
		t.Fatalf("for-each-ref failed: %v", runErr)
	}
	if leaked := strings.TrimSpace(string(out)); leaked != "" {
		t.Errorf("branch leaked into bare repo after path-collision failure:\n%s", leaked)
	}
}

// TestGetRemoteDefaultBranchScansForCustomDefault verifies fallback (3) of
// getRemoteDefaultBranch: when the cache has refs/remotes/origin/<custom>
// (e.g. develop, trunk) but no refs/remotes/origin/HEAD and no main/master,
// the function picks the custom branch instead of returning empty.
func TestGetRemoteDefaultBranchScansForCustomDefault(t *testing.T) {
	t.Parallel()
	sourceRepo := createTestRepo(t)
	cacheRoot := t.TempDir()
	cache := New(cacheRoot, testLogger())
	if err := cache.Sync("ws-1", []RepoInfo{{URL: sourceRepo}}); err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	barePath := cache.Lookup("ws-1", sourceRepo)

	// Resolve the existing default branch's commit so we can repoint a
	// custom-named ref at it, then wipe the standard refs to force the
	// fallback path.
	existing := getRemoteDefaultBranch(barePath)
	if existing == "" {
		t.Fatalf("precondition: cache should have a default branch right after sync")
	}
	commit := gitRefCommit(t, barePath, existing)

	// Create refs/remotes/origin/develop pointing at that commit.
	runGitAuthored(t, barePath, "update-ref", "refs/remotes/origin/develop", commit)
	// Now wipe origin/HEAD (symbolic-ref -d removes the symref file itself)
	// and the common defaults so steps 1 and 2 of the resolver miss and we
	// fall through to the for-each-ref scan.
	_ = exec.Command("git", "-C", barePath, "symbolic-ref", "-d", "refs/remotes/origin/HEAD").Run()
	_ = exec.Command("git", "-C", barePath, "update-ref", "-d", "refs/remotes/origin/main").Run()
	_ = exec.Command("git", "-C", barePath, "update-ref", "-d", "refs/remotes/origin/master").Run()

	got := getRemoteDefaultBranch(barePath)
	if got != "refs/remotes/origin/develop" {
		t.Fatalf("getRemoteDefaultBranch = %q, want refs/remotes/origin/develop", got)
	}
}

// TestGetRemoteDefaultBranchFallsBackToBareHead verifies fallback (5):
// a legacy / migration-pending cache that has no refs/remotes/origin/* at all
// but still has its bare HEAD pointing at refs/heads/<branch> (the snapshot
// from the original mirror clone) should resolve to that local head instead
// of failing. This protects against transient backfill-fetch failures during
// the legacy → modern refspec migration. Gated on refs/remotes/origin/* being
// completely empty — with any modern remote-tracking refs present, the
// resolver refuses to reach back into the stale bare heads.
func TestGetRemoteDefaultBranchFallsBackToBareHead(t *testing.T) {
	t.Parallel()
	sourceRepo := createTestRepo(t)
	cacheRoot := t.TempDir()
	cache := New(cacheRoot, testLogger())
	if err := cache.Sync("ws-1", []RepoInfo{{URL: sourceRepo}}); err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	barePath := cache.Lookup("ws-1", sourceRepo)

	// Force the cache into a state that mimics "legacy mirror clone whose
	// post-migration backfill fetch failed":
	//   - bare HEAD still points at refs/heads/<default>
	//   - refs/remotes/origin/* is empty
	if err := exec.Command("sh", "-c", "rm -rf '"+filepath.Join(barePath, "refs", "remotes")+"'").Run(); err != nil {
		t.Fatalf("wipe refs/remotes: %v", err)
	}

	// Sanity: origin/* is gone, HEAD is still a symbolic ref to refs/heads/*.
	if out, err := exec.Command("git", "-C", barePath, "for-each-ref", "refs/remotes/origin/").Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		t.Fatalf("precondition failed: refs/remotes/origin/* should be empty, got %s", out)
	}

	got := getRemoteDefaultBranch(barePath)
	if !strings.HasPrefix(got, "refs/heads/") {
		t.Fatalf("getRemoteDefaultBranch = %q, want refs/heads/* fallback", got)
	}

	// And the resolved ref must actually exist — verifying bareHeadBranch's
	// rev-parse guard kicked in correctly.
	if err := exec.Command("git", "-C", barePath, "rev-parse", "--verify", got).Run(); err != nil {
		t.Fatalf("resolved ref %q does not exist: %v", got, err)
	}
}

// TestGitFetchRefreshesOriginHeadAfterDefaultChange verifies that an
// already-modern cache picks up a remote default-branch change. Plain `git
// fetch` never refreshes refs/remotes/origin/HEAD on its own, so without
// gitFetch's explicit `git remote set-head origin --auto` call the resolver
// would keep returning the original default branch forever after the
// upstream flipped (e.g. master → main on a long-lived repo). This guards
// against the "already-modern cache never refreshes origin/HEAD" regression.
func TestGitFetchRefreshesOriginHeadAfterDefaultChange(t *testing.T) {
	t.Parallel()
	sourceRepo := createTestRepo(t)
	initialBranch := currentBranchName(t, sourceRepo)

	cacheRoot := t.TempDir()
	cache := New(cacheRoot, testLogger())
	if err := cache.Sync("ws-1", []RepoInfo{{URL: sourceRepo}}); err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	barePath := cache.Lookup("ws-1", sourceRepo)

	// Precondition: cache is already modern and origin/HEAD points at the
	// source's initial default branch.
	if got := getRemoteDefaultBranch(barePath); got != "refs/remotes/origin/"+initialBranch {
		t.Fatalf("precondition: getRemoteDefaultBranch = %q, want refs/remotes/origin/%s", got, initialBranch)
	}

	// Flip the source's default: create a new branch, commit on it, stay
	// checked out on it so the source's HEAD reflects the new default. A
	// subsequent `git ls-remote` against the source advertises this new
	// HEAD, which is what set-head --auto consumes.
	runGitAuthored(t, sourceRepo, "checkout", "-b", "new-default")
	addEmptyCommit(t, sourceRepo, "new-default commit")

	// Fetch via the cache's code path. Without the set-head call, origin/HEAD
	// would still point at the old default here.
	if err := gitFetch(barePath); err != nil {
		t.Fatalf("gitFetch failed: %v", err)
	}

	// refs/remotes/origin/HEAD must now point at the new default branch.
	out, err := exec.Command("git", "-C", barePath, "symbolic-ref", "refs/remotes/origin/HEAD").Output()
	if err != nil {
		t.Fatalf("symbolic-ref origin/HEAD after fetch: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "refs/remotes/origin/new-default" {
		t.Fatalf("origin/HEAD after fetch = %q, want refs/remotes/origin/new-default", got)
	}

	// And getRemoteDefaultBranch must resolve through step 1 (verified
	// origin/HEAD) to the new default — not through step 2 where origin/main
	// or origin/master could accidentally match the old branch.
	if got := getRemoteDefaultBranch(barePath); got != "refs/remotes/origin/new-default" {
		t.Fatalf("getRemoteDefaultBranch after fetch = %q, want refs/remotes/origin/new-default", got)
	}
}

// TestGetRemoteDefaultBranchUsesBareHeadHintForCustomDefault verifies step 3
// of the resolver: when the cache has a non-standard default branch name
// (trunk, develop, …) and `git remote set-head origin --auto` didn't
// populate refs/remotes/origin/HEAD, the resolver must use the bare repo's
// own HEAD as a hint to pick refs/remotes/origin/<same name> — NOT fall
// through to a refname-order scan that would pick the wrong branch.
func TestGetRemoteDefaultBranchUsesBareHeadHintForCustomDefault(t *testing.T) {
	t.Parallel()
	sourceRepo := createTestRepo(t)
	cacheRoot := t.TempDir()
	cache := New(cacheRoot, testLogger())
	if err := cache.Sync("ws-1", []RepoInfo{{URL: sourceRepo}}); err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	barePath := cache.Lookup("ws-1", sourceRepo)

	existing := getRemoteDefaultBranch(barePath)
	if existing == "" {
		t.Fatalf("precondition: cache should have a default branch right after sync")
	}
	commit := gitRefCommit(t, barePath, existing)

	// Simulate a custom default branch: create refs/heads/trunk in the bare
	// repo and point HEAD at it. `git clone --bare` would do the equivalent
	// when the remote's default was "trunk", so this matches real-world
	// state for such remotes.
	runGitAuthored(t, barePath, "update-ref", "refs/heads/trunk", commit)
	runGitAuthored(t, barePath, "symbolic-ref", "HEAD", "refs/heads/trunk")

	// Populate two refs/remotes/origin/* entries. "feature-alpha" is
	// alphabetically earlier than "trunk" — a refname-order scan (the old
	// bug) would return feature-alpha, not trunk.
	runGitAuthored(t, barePath, "update-ref", "refs/remotes/origin/trunk", commit)
	runGitAuthored(t, barePath, "update-ref", "refs/remotes/origin/feature-alpha", commit)

	// Knock out the ahead-of-step-3 fallbacks so resolution must rely on
	// the bare-HEAD hint.
	_ = exec.Command("git", "-C", barePath, "symbolic-ref", "-d", "refs/remotes/origin/HEAD").Run()
	_ = exec.Command("git", "-C", barePath, "update-ref", "-d", "refs/remotes/origin/main").Run()
	_ = exec.Command("git", "-C", barePath, "update-ref", "-d", "refs/remotes/origin/master").Run()

	got := getRemoteDefaultBranch(barePath)
	if got != "refs/remotes/origin/trunk" {
		t.Fatalf("getRemoteDefaultBranch = %q, want refs/remotes/origin/trunk (via bare-HEAD hint)", got)
	}
}

// TestCreateWorktreeInstallsCoAuthoredByHook verifies that CreateWorktree
// installs a prepare-commit-msg hook that appends a Co-authored-by trailer
// for the Multica Agent to every commit made in the worktree.
func TestCreateWorktreeInstallsCoAuthoredByHook(t *testing.T) {
	t.Parallel()
	sourceRepo := createTestRepo(t)
	cacheRoot := t.TempDir()

	cache := New(cacheRoot, testLogger())
	if err := cache.Sync("ws-1", []RepoInfo{{URL: sourceRepo}}); err != nil {
		t.Fatalf("sync failed: %v", err)
	}

	workDir := t.TempDir()
	result, err := cache.CreateWorktree(WorktreeParams{
		WorkspaceID:         "ws-1",
		RepoURL:             sourceRepo,
		WorkDir:             workDir,
		AgentName:           "Test Agent",
		TaskID:              "a1b2c3d4-0000-0000-0000-000000000000",
		CoAuthoredByEnabled: true,
	})
	if err != nil {
		t.Fatalf("CreateWorktree failed: %v", err)
	}

	// Make a commit in the worktree and verify the hook appends the trailer.
	if err := os.WriteFile(filepath.Join(result.Path, "test.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	runGitAuthored(t, result.Path, "add", ".")
	runGitAuthored(t, result.Path, "commit", "-m", "test commit")

	// Read the commit message.
	out, err := exec.Command("git", "-C", result.Path, "log", "-1", "--format=%B").Output()
	if err != nil {
		t.Fatalf("git log failed: %v", err)
	}
	commitMsg := string(out)
	expectedTrailer := "Co-authored-by: multica-agent <github@multica.ai>"
	if !strings.Contains(commitMsg, expectedTrailer) {
		t.Errorf("commit message missing Co-authored-by trailer.\ngot:\n%s", commitMsg)
	}
}

// TestCoAuthoredByHookIdempotent verifies that the hook does not add a
// duplicate Co-authored-by trailer if one is already present in the message.
func TestCoAuthoredByHookIdempotent(t *testing.T) {
	t.Parallel()
	sourceRepo := createTestRepo(t)
	cacheRoot := t.TempDir()

	cache := New(cacheRoot, testLogger())
	if err := cache.Sync("ws-1", []RepoInfo{{URL: sourceRepo}}); err != nil {
		t.Fatalf("sync failed: %v", err)
	}

	workDir := t.TempDir()
	result, err := cache.CreateWorktree(WorktreeParams{
		WorkspaceID:         "ws-1",
		RepoURL:             sourceRepo,
		WorkDir:             workDir,
		AgentName:           "Test Agent",
		TaskID:              "b2c3d4e5-0000-0000-0000-000000000000",
		CoAuthoredByEnabled: true,
	})
	if err != nil {
		t.Fatalf("CreateWorktree failed: %v", err)
	}

	// Commit with the trailer already in the message.
	trailer := "Co-authored-by: multica-agent <github@multica.ai>"
	if err := os.WriteFile(filepath.Join(result.Path, "test.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	runGitAuthored(t, result.Path, "add", ".")
	runGitAuthored(t, result.Path, "commit", "-m", "test commit\n\n"+trailer)

	out, err := exec.Command("git", "-C", result.Path, "log", "-1", "--format=%B").Output()
	if err != nil {
		t.Fatalf("git log failed: %v", err)
	}
	commitMsg := string(out)

	// Count occurrences — should appear exactly once.
	count := strings.Count(commitMsg, trailer)
	if count != 1 {
		t.Errorf("expected exactly 1 Co-authored-by trailer, found %d.\ngot:\n%s", count, commitMsg)
	}
}

// TestCreateWorktreeRemovesCoAuthoredByHookWhenDisabled verifies the toggle-off
// path: a bare cache that already carries the Multica prepare-commit-msg hook
// (e.g. from a prior worktree created with the setting on) must drop the hook
// when the next CreateWorktree call passes CoAuthoredByEnabled=false.
// Otherwise commits keep getting the trailer even after the user disables the
// workspace setting.
func TestCreateWorktreeRemovesCoAuthoredByHookWhenDisabled(t *testing.T) {
	t.Parallel()
	sourceRepo := createTestRepo(t)
	cacheRoot := t.TempDir()

	cache := New(cacheRoot, testLogger())
	if err := cache.Sync("ws-1", []RepoInfo{{URL: sourceRepo}}); err != nil {
		t.Fatalf("sync failed: %v", err)
	}

	// First worktree: setting enabled → hook installed in the bare cache's
	// shared hooks dir.
	workDir1 := t.TempDir()
	if _, err := cache.CreateWorktree(WorktreeParams{
		WorkspaceID:         "ws-1",
		RepoURL:             sourceRepo,
		WorkDir:             workDir1,
		AgentName:           "Test Agent",
		TaskID:              "11111111-0000-0000-0000-000000000000",
		CoAuthoredByEnabled: true,
	}); err != nil {
		t.Fatalf("CreateWorktree (enabled) failed: %v", err)
	}

	barePath := cache.Lookup("ws-1", sourceRepo)
	hookPath := filepath.Join(barePath, "hooks", "prepare-commit-msg")
	if _, err := os.Stat(hookPath); err != nil {
		t.Fatalf("precondition: expected hook to be installed at %s: %v", hookPath, err)
	}

	// Second worktree on the same bare cache: setting disabled → hook must
	// be removed and a commit in the new worktree must NOT carry the
	// trailer.
	workDir2 := t.TempDir()
	result, err := cache.CreateWorktree(WorktreeParams{
		WorkspaceID:         "ws-1",
		RepoURL:             sourceRepo,
		WorkDir:             workDir2,
		AgentName:           "Test Agent",
		TaskID:              "22222222-0000-0000-0000-000000000000",
		CoAuthoredByEnabled: false,
	})
	if err != nil {
		t.Fatalf("CreateWorktree (disabled) failed: %v", err)
	}

	if _, err := os.Stat(hookPath); !os.IsNotExist(err) {
		t.Errorf("expected hook to be removed at %s, stat err=%v", hookPath, err)
	}

	if err := os.WriteFile(filepath.Join(result.Path, "test.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	runGitAuthored(t, result.Path, "add", ".")
	runGitAuthored(t, result.Path, "commit", "-m", "test commit")

	out, err := exec.Command("git", "-C", result.Path, "log", "-1", "--format=%B").Output()
	if err != nil {
		t.Fatalf("git log failed: %v", err)
	}
	commitMsg := string(out)
	if strings.Contains(commitMsg, "Co-authored-by: multica-agent") {
		t.Errorf("commit unexpectedly carries the Co-authored-by trailer with setting disabled.\ngot:\n%s", commitMsg)
	}
}

// TestCreateWorktreeRemovesLegacyCoAuthoredByHook verifies the migration
// path: bare clones already on disk from previous daemon versions carry a
// prepare-commit-msg hook that does NOT include the multicaHookMarker
// sentinel — only the older `# Installed by the Multica daemon.` comment.
// Toggling the workspace setting off must still remove those legacy hooks,
// otherwise users who flip the toggle in production keep seeing the trailer
// indefinitely (the exact bug reported in MUL-1704).
func TestCreateWorktreeRemovesLegacyCoAuthoredByHook(t *testing.T) {
	t.Parallel()
	sourceRepo := createTestRepo(t)
	cacheRoot := t.TempDir()

	cache := New(cacheRoot, testLogger())
	if err := cache.Sync("ws-1", []RepoInfo{{URL: sourceRepo}}); err != nil {
		t.Fatalf("sync failed: %v", err)
	}

	// Seed the bare cache with the exact hook content shipped by the
	// previous daemon release (no multicaHookMarker line). Keeping a
	// verbatim copy here means the test fails if recognition logic ever
	// drifts away from what production hosts actually have on disk.
	const legacyHook = `#!/bin/sh
# Multica: add Co-authored-by trailer for the Multica Agent.
# Installed by the Multica daemon. Do not edit — it will be overwritten.

COMMIT_MSG_FILE="$1"
COMMIT_SOURCE="$2"

# Skip merge and squash commits.
case "$COMMIT_SOURCE" in
  merge|squash) exit 0 ;;
esac

TRAILER="Co-authored-by: multica-agent <github@multica.ai>"

# Don't add if already present.
if grep -qF "$TRAILER" "$COMMIT_MSG_FILE"; then
  exit 0
fi

# Use git interpret-trailers for proper formatting.
git interpret-trailers --in-place --trailer "$TRAILER" "$COMMIT_MSG_FILE"
`

	barePath := cache.Lookup("ws-1", sourceRepo)
	hooksDir := filepath.Join(barePath, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("create hooks dir: %v", err)
	}
	hookPath := filepath.Join(hooksDir, "prepare-commit-msg")
	if err := os.WriteFile(hookPath, []byte(legacyHook), 0o755); err != nil {
		t.Fatalf("seed legacy hook: %v", err)
	}

	workDir := t.TempDir()
	result, err := cache.CreateWorktree(WorktreeParams{
		WorkspaceID:         "ws-1",
		RepoURL:             sourceRepo,
		WorkDir:             workDir,
		AgentName:           "Test Agent",
		TaskID:              "44444444-0000-0000-0000-000000000000",
		CoAuthoredByEnabled: false,
	})
	if err != nil {
		t.Fatalf("CreateWorktree (disabled) failed: %v", err)
	}

	if _, err := os.Stat(hookPath); !os.IsNotExist(err) {
		t.Errorf("expected legacy hook to be removed at %s, stat err=%v", hookPath, err)
	}

	if err := os.WriteFile(filepath.Join(result.Path, "test.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	runGitAuthored(t, result.Path, "add", ".")
	runGitAuthored(t, result.Path, "commit", "-m", "test commit")

	out, err := exec.Command("git", "-C", result.Path, "log", "-1", "--format=%B").Output()
	if err != nil {
		t.Fatalf("git log failed: %v", err)
	}
	if commitMsg := string(out); strings.Contains(commitMsg, "Co-authored-by: multica-agent") {
		t.Errorf("commit unexpectedly carries the Co-authored-by trailer after legacy hook removal.\ngot:\n%s", commitMsg)
	}
}

// TestRemoveCoAuthoredByHookPreservesUserHook verifies that the disable path
// only deletes hooks installed by the daemon. A prepare-commit-msg hook
// without the Multica marker (e.g. one a user added manually) must be left
// untouched even when CoAuthoredByEnabled=false.
func TestRemoveCoAuthoredByHookPreservesUserHook(t *testing.T) {
	t.Parallel()
	sourceRepo := createTestRepo(t)
	cacheRoot := t.TempDir()

	cache := New(cacheRoot, testLogger())
	if err := cache.Sync("ws-1", []RepoInfo{{URL: sourceRepo}}); err != nil {
		t.Fatalf("sync failed: %v", err)
	}

	barePath := cache.Lookup("ws-1", sourceRepo)
	hooksDir := filepath.Join(barePath, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("create hooks dir: %v", err)
	}
	hookPath := filepath.Join(hooksDir, "prepare-commit-msg")
	userHook := "#!/bin/sh\n# user hook, not Multica\nexit 0\n"
	if err := os.WriteFile(hookPath, []byte(userHook), 0o755); err != nil {
		t.Fatalf("seed user hook: %v", err)
	}

	workDir := t.TempDir()
	if _, err := cache.CreateWorktree(WorktreeParams{
		WorkspaceID:         "ws-1",
		RepoURL:             sourceRepo,
		WorkDir:             workDir,
		AgentName:           "Test Agent",
		TaskID:              "33333333-0000-0000-0000-000000000000",
		CoAuthoredByEnabled: false,
	}); err != nil {
		t.Fatalf("CreateWorktree (disabled) failed: %v", err)
	}

	got, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("user hook unexpectedly removed: %v", err)
	}
	if string(got) != userHook {
		t.Errorf("user hook contents changed.\nwant:\n%s\ngot:\n%s", userHook, string(got))
	}
}

// TestGetRemoteDefaultBranchAmbiguousOriginReturnsEmpty verifies step 4's
// safe-scan gating: when the cache has multiple refs/remotes/origin/*
// entries, none match the common defaults, and none match the bare HEAD
// either, the resolver must refuse to guess and return "". The caller
// surfaces this as a hard error instead of silently basing new agent work
// on an arbitrary refname-order-first candidate.
func TestGetRemoteDefaultBranchAmbiguousOriginReturnsEmpty(t *testing.T) {
	t.Parallel()
	sourceRepo := createTestRepo(t)
	cacheRoot := t.TempDir()
	cache := New(cacheRoot, testLogger())
	if err := cache.Sync("ws-1", []RepoInfo{{URL: sourceRepo}}); err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	barePath := cache.Lookup("ws-1", sourceRepo)

	existing := getRemoteDefaultBranch(barePath)
	if existing == "" {
		t.Fatalf("precondition: cache should have a default branch right after sync")
	}
	commit := gitRefCommit(t, barePath, existing)

	// Populate two unrelated origin branches (none of which match any of
	// the step 1-3 fallbacks).
	runGitAuthored(t, barePath, "update-ref", "refs/remotes/origin/feature-a", commit)
	runGitAuthored(t, barePath, "update-ref", "refs/remotes/origin/feature-b", commit)

	// Wipe every ref a step 1-3 fallback could pick up:
	//   step 1: origin/HEAD
	//   step 2: origin/main, origin/master
	//   step 3: the origin/<bareHEAD-name> bridge
	_ = exec.Command("git", "-C", barePath, "symbolic-ref", "-d", "refs/remotes/origin/HEAD").Run()
	_ = exec.Command("git", "-C", barePath, "update-ref", "-d", "refs/remotes/origin/main").Run()
	_ = exec.Command("git", "-C", barePath, "update-ref", "-d", "refs/remotes/origin/master").Run()
	if bareRef := bareHeadBranch(barePath); bareRef != "" {
		sameName := strings.TrimPrefix(bareRef, "refs/heads/")
		_ = exec.Command("git", "-C", barePath, "update-ref", "-d", "refs/remotes/origin/"+sameName).Run()
	}

	got := getRemoteDefaultBranch(barePath)
	if got != "" {
		t.Fatalf("getRemoteDefaultBranch = %q, want \"\" (ambiguous origin/* must not guess)", got)
	}
}
