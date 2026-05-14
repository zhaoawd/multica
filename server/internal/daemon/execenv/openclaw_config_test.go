package execenv

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// TestPrepareOpenclawConfigOverridesWorkspace pins that the per-task config
// writes agents.defaults.workspace = workDir and rewrites every
// agents.list[].workspace. The upstream scanner resolves
// `agents.list[id].workspace → agents.defaults.workspace → ~/.openclaw/
// workspace`, so leaving per-agent workspaces alone would silently bypass
// the override on agents the user has explicitly configured.
func TestPrepareOpenclawConfigOverridesWorkspace(t *testing.T) {
	// Not parallel: mutates HOME / OPENCLAW_CONFIG_PATH.
	envRoot := t.TempDir()
	workDir := filepath.Join(envRoot, "workdir")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}

	// Seed a user config that mimics a real install: defaults.workspace
	// pointed at the shared ~/.openclaw/workspace AND a per-agent workspace
	// override that would otherwise win.
	userHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(userHome, ".openclaw"), 0o755); err != nil {
		t.Fatalf("mkdir user home: %v", err)
	}
	userCfg := map[string]any{
		"agents": map[string]any{
			"defaults": map[string]any{
				"workspace": "/Users/alice/.openclaw/workspace",
				"model": map[string]any{
					"primary": "deepseek/deepseek-chat",
				},
			},
			"list": []any{
				map[string]any{
					"id":        "coding-bot",
					"workspace": "/Users/alice/projects/coding-bot",
					"model":     "anthropic/claude-sonnet-4",
				},
				map[string]any{
					"id":    "scout",
					"model": "openai/gpt-5",
				},
			},
		},
		"gateway": map[string]any{
			"port": float64(18789),
		},
	}
	data, err := json.MarshalIndent(userCfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal user cfg: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userHome, ".openclaw", "openclaw.json"), data, 0o600); err != nil {
		t.Fatalf("write user cfg: %v", err)
	}

	t.Setenv("HOME", userHome)
	t.Setenv("OPENCLAW_CONFIG_PATH", "") // force the HOME fallback

	cfgPath, err := prepareOpenclawConfig(envRoot, workDir)
	if err != nil {
		t.Fatalf("prepareOpenclawConfig: %v", err)
	}
	if cfgPath != filepath.Join(envRoot, openclawConfigFile) {
		t.Errorf("cfgPath = %q, want %q", cfgPath, filepath.Join(envRoot, openclawConfigFile))
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read synthesized cfg: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse synthesized cfg: %v", err)
	}

	agents := got["agents"].(map[string]any)
	defaults := agents["defaults"].(map[string]any)
	if defaults["workspace"] != workDir {
		t.Errorf("agents.defaults.workspace = %v, want %q", defaults["workspace"], workDir)
	}

	// User's other defaults fields (model, etc.) must survive the override.
	model, ok := defaults["model"].(map[string]any)
	if !ok || model["primary"] != "deepseek/deepseek-chat" {
		t.Errorf("agents.defaults.model not preserved: %v", defaults["model"])
	}

	list := agents["list"].([]any)
	if len(list) != 2 {
		t.Fatalf("agents.list length = %d, want 2", len(list))
	}
	for i, item := range list {
		entry := item.(map[string]any)
		if entry["workspace"] != workDir {
			t.Errorf("agents.list[%d].workspace = %v, want %q (per-agent overrides must be rewritten so they don't beat defaults)", i, entry["workspace"], workDir)
		}
	}

	// Non-agents top-level fields must survive (gateway, models, providers).
	gw, ok := got["gateway"].(map[string]any)
	if !ok || gw["port"].(float64) != 18789 {
		t.Errorf("gateway section not preserved: %v", got["gateway"])
	}
}

// TestPrepareOpenclawConfigMissingUserConfig — a fresh openclaw install with
// no ~/.openclaw/openclaw.json must not break task startup. We synthesize a
// minimal config containing just the workspace override so the scanner at
// least picks up {workDir}/skills/.
func TestPrepareOpenclawConfigMissingUserConfig(t *testing.T) {
	envRoot := t.TempDir()
	workDir := filepath.Join(envRoot, "workdir")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}

	emptyHome := t.TempDir()
	t.Setenv("HOME", emptyHome)
	t.Setenv("OPENCLAW_CONFIG_PATH", "")

	cfgPath, err := prepareOpenclawConfig(envRoot, workDir)
	if err != nil {
		t.Fatalf("prepareOpenclawConfig: %v", err)
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read synthesized cfg: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse synthesized cfg: %v", err)
	}
	agents := got["agents"].(map[string]any)
	defaults := agents["defaults"].(map[string]any)
	if defaults["workspace"] != workDir {
		t.Errorf("agents.defaults.workspace = %v, want %q", defaults["workspace"], workDir)
	}
}

// TestPrepareOpenclawConfigFollowsEnvVar — when $OPENCLAW_CONFIG_PATH is
// already set on the daemon's environment (e.g. wrapping another tool that
// redirected openclaw at a custom file), the preparer reads from that path
// rather than ~/.openclaw/openclaw.json. The per-task synthesized copy then
// inherits everything from the redirected source.
func TestPrepareOpenclawConfigFollowsEnvVar(t *testing.T) {
	envRoot := t.TempDir()
	workDir := filepath.Join(envRoot, "workdir")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}

	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "alt-openclaw.json")
	srcCfg := map[string]any{
		"agents": map[string]any{
			"defaults": map[string]any{
				"workspace": "/should/be/overwritten",
			},
		},
		"meta": map[string]any{
			"marker": "from-env-var-source",
		},
	}
	data, err := json.MarshalIndent(srcCfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal src cfg: %v", err)
	}
	if err := os.WriteFile(srcPath, data, 0o600); err != nil {
		t.Fatalf("write src cfg: %v", err)
	}

	t.Setenv("OPENCLAW_CONFIG_PATH", srcPath)

	cfgPath, err := prepareOpenclawConfig(envRoot, workDir)
	if err != nil {
		t.Fatalf("prepareOpenclawConfig: %v", err)
	}
	raw, _ := os.ReadFile(cfgPath)
	var got map[string]any
	_ = json.Unmarshal(raw, &got)

	meta := got["meta"].(map[string]any)
	if meta["marker"] != "from-env-var-source" {
		t.Errorf("synthesized cfg should inherit from $OPENCLAW_CONFIG_PATH source; got meta = %v", meta)
	}
	agents := got["agents"].(map[string]any)
	defaults := agents["defaults"].(map[string]any)
	if defaults["workspace"] != workDir {
		t.Errorf("workspace = %v, want %q (override must replace the source's value)", defaults["workspace"], workDir)
	}
}

// TestPrepareOpenclawSkillWriteMatchesScanPath is the regression test the
// MUL-2219 DoD calls out: the directory Multica writes skills into MUST be
// the same directory the OpenClaw scanner reads from. We assert this by
// resolving the workspaceDir the way OpenClaw does (agents.defaults.workspace
// from the synthesized config) and proving {workspaceDir}/skills/ holds the
// skill we wrote. Previous fixes asserted "we wrote a file" without checking
// the scanner would ever see it; that's why MUL-2213 / #2621 needed a
// follow-up.
func TestPrepareOpenclawSkillWriteMatchesScanPath(t *testing.T) {
	envRoot := t.TempDir()
	workDir := filepath.Join(envRoot, "workdir")
	for _, sub := range []string{workDir, filepath.Join(envRoot, "output"), filepath.Join(envRoot, "logs")} {
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENCLAW_CONFIG_PATH", "")

	skills := []SkillContextForEnv{
		{Name: "Issue Review", Content: "Review issues thoroughly."},
		{Name: "Local Dev", Content: "Spin up the local dev env."},
	}

	cfgPath, err := prepareOpenclawConfig(envRoot, workDir)
	if err != nil {
		t.Fatalf("prepareOpenclawConfig: %v", err)
	}
	if err := writeContextFiles(workDir, "openclaw", TaskContextForEnv{
		IssueID:     "issue-1",
		AgentSkills: skills,
	}); err != nil {
		t.Fatalf("writeContextFiles: %v", err)
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read synthesized cfg: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse synthesized cfg: %v", err)
	}
	wsDir := cfg["agents"].(map[string]any)["defaults"].(map[string]any)["workspace"].(string)

	// Scan path #1 the openclaw runtime would inspect: <workspaceDir>/skills/
	for _, s := range skills {
		want := filepath.Join(wsDir, "skills", sanitizeSkillName(s.Name), "SKILL.md")
		if _, err := os.Stat(want); err != nil {
			t.Errorf("openclaw scan target %s missing — Multica's write path and the openclaw scanner are out of sync: %v", want, err)
		}
	}
}

// TestPrepareEnvironmentOpenclawWiresConfigPath — end-to-end: Prepare sets
// env.OpenclawConfigPath so the daemon can export OPENCLAW_CONFIG_PATH, and
// the path resolves to a file with the correct workspace override.
func TestPrepareEnvironmentOpenclawWiresConfigPath(t *testing.T) {
	wsRoot := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENCLAW_CONFIG_PATH", "")

	env, err := Prepare(PrepareParams{
		WorkspacesRoot: wsRoot,
		WorkspaceID:    "ws-1",
		TaskID:         "11111111-2222-3333-4444-555555555555",
		AgentName:      "scout",
		Provider:       "openclaw",
		Task: TaskContextForEnv{
			IssueID: "issue-1",
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if env.OpenclawConfigPath == "" {
		t.Fatal("Prepare(openclaw) did not set OpenclawConfigPath")
	}
	if _, err := os.Stat(env.OpenclawConfigPath); err != nil {
		t.Fatalf("OpenclawConfigPath does not exist: %v", err)
	}
	raw, _ := os.ReadFile(env.OpenclawConfigPath)
	var cfg map[string]any
	_ = json.Unmarshal(raw, &cfg)
	got := cfg["agents"].(map[string]any)["defaults"].(map[string]any)["workspace"]
	if got != env.WorkDir {
		t.Errorf("agents.defaults.workspace = %v, want %q", got, env.WorkDir)
	}
}

// TestPrepareEnvironmentNonOpenclawSkipsConfig — non-openclaw providers
// must not get a synthesized openclaw config (it would be dead weight on
// disk and confuse the GC reaper's idea of what an env contains).
func TestPrepareEnvironmentNonOpenclawSkipsConfig(t *testing.T) {
	wsRoot := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENCLAW_CONFIG_PATH", "")

	taskIDs := map[string]string{
		"claude":   "aaaaaaaa-1111-2222-3333-444444444444",
		"opencode": "bbbbbbbb-1111-2222-3333-444444444444",
		"hermes":   "cccccccc-1111-2222-3333-444444444444",
		"kiro":     "dddddddd-1111-2222-3333-444444444444",
	}
	for provider, taskID := range taskIDs {
		t.Run(provider, func(t *testing.T) {
			env, err := Prepare(PrepareParams{
				WorkspacesRoot: wsRoot,
				WorkspaceID:    "ws-1",
				TaskID:         taskID,
				AgentName:      "scout",
				Provider:       provider,
				Task:           TaskContextForEnv{IssueID: "issue-1"},
			}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatalf("Prepare(%s): %v", provider, err)
			}
			if env.OpenclawConfigPath != "" {
				t.Errorf("provider %s should not get an OpenclawConfigPath, got %q", provider, env.OpenclawConfigPath)
			}
			if _, err := os.Stat(filepath.Join(env.RootDir, openclawConfigFile)); !os.IsNotExist(err) {
				t.Errorf("provider %s left a stray openclaw-config.json", provider)
			}
		})
	}
}
