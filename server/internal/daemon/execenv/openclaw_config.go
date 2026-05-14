package execenv

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// openclawConfigFile is the per-task synthesized OpenClaw config the daemon
// points the openclaw CLI at via OPENCLAW_CONFIG_PATH. It sits in the env
// root (alongside workdir/, output/, logs/) so the GC reaper sweeps it with
// the rest of the task env.
const openclawConfigFile = "openclaw-config.json"

// prepareOpenclawConfig writes a per-task OpenClaw config to envRoot and
// returns its absolute path. The daemon sets OPENCLAW_CONFIG_PATH to this
// path on the spawned openclaw subprocess so the CLI resolves its
// `agents.defaults.workspace` (and any `agents.list[].workspace`) to the
// task workdir — which is what makes OpenClaw's native skill scanner pick
// up the per-task skills we write under `<workDir>/skills/`.
//
// We deep-copy the user's existing config (priority: $OPENCLAW_CONFIG_PATH
// > ~/.openclaw/openclaw.json) and only override workspace fields. This
// preserves everything the openclaw CLI needs to actually run a task —
// registered agents in `agents.list[]`, model providers, gateway settings,
// channels — while pinning workspace to the per-task workdir.
//
// Missing or malformed user configs are non-fatal: we synthesize a minimal
// config with just the workspace override. Older openclaw versions that
// require additional fields will surface their own validation errors when
// the CLI starts; this preparer never blocks task startup on config IO.
func prepareOpenclawConfig(envRoot, workDir string) (string, error) {
	cfg, err := loadUserOpenclawConfig()
	if err != nil {
		// Fall through to a fresh config rather than failing the task —
		// "no native skill discovery" is a tolerable degradation, "task
		// won't start because we can't parse your existing config" is not.
		cfg = map[string]any{}
	}

	overrideOpenclawWorkspace(cfg, workDir)

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal openclaw config: %w", err)
	}
	outPath := filepath.Join(envRoot, openclawConfigFile)
	// 0o600 — the config may carry the user's openclaw gateway auth token
	// and provider API keys. Match the permissions OpenClaw uses for its
	// own ~/.openclaw/openclaw.json.
	if err := os.WriteFile(outPath, data, 0o600); err != nil {
		return "", fmt.Errorf("write openclaw config: %w", err)
	}
	return outPath, nil
}

// loadUserOpenclawConfig reads the user's existing OpenClaw config so the
// per-task synthesized copy inherits registered agents, model providers,
// and gateway settings. Resolution order matches the CLI:
//
//  1. $OPENCLAW_CONFIG_PATH (lets a parent process — e.g. another daemon
//     wrapper — already redirect openclaw at a custom file)
//  2. ~/.openclaw/openclaw.json (the CLI's default)
//
// Returns an empty config (not an error) when neither source exists, so a
// fresh openclaw install can still get a per-task workspace override.
func loadUserOpenclawConfig() (map[string]any, error) {
	var path string
	if v := os.Getenv("OPENCLAW_CONFIG_PATH"); v != "" {
		path = v
	} else if home, err := os.UserHomeDir(); err == nil {
		path = filepath.Join(home, ".openclaw", "openclaw.json")
	}
	if path == "" {
		return map[string]any{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("read openclaw config %s: %w", path, err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse openclaw config %s: %w", path, err)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return cfg, nil
}

// overrideOpenclawWorkspace pins agents.defaults.workspace AND every
// agents.list[].workspace to workDir. OpenClaw's workspace resolution order
// is `agents.list[id].workspace → agents.defaults.workspace → ~/.openclaw/
// workspace`, so a per-agent workspace in the user config would otherwise
// silently win and bypass our override.
//
// Mutates cfg in place. Safe to call with any subset of agents/defaults/list
// missing — intermediate maps are created on demand.
func overrideOpenclawWorkspace(cfg map[string]any, workDir string) {
	agents, _ := cfg["agents"].(map[string]any)
	if agents == nil {
		agents = map[string]any{}
		cfg["agents"] = agents
	}

	defaults, _ := agents["defaults"].(map[string]any)
	if defaults == nil {
		defaults = map[string]any{}
		agents["defaults"] = defaults
	}
	defaults["workspace"] = workDir

	// `agents.list` is an array of agent entries per the openclaw schema. We
	// rewrite each entry's workspace so per-agent overrides don't silently
	// re-route the scan paths back to the user's shared workspace.
	if list, ok := agents["list"].([]any); ok {
		for _, item := range list {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			entry["workspace"] = workDir
		}
	}
}
