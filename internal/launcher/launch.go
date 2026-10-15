package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
)

func (p Paths) launch(provider, id, profile, group string, native []string) error {
	if err := validateNativeArgs(provider, native); err != nil {
		return err
	}
	lock, err := lockInstance(p.label(provider, id))
	if err != nil {
		return err
	}
	defer lock.Close()
	i, err := p.load(provider, id)
	if err != nil {
		return err
	}
	if profile != "" {
		if err := regularFile(filepath.Join(p.native(i), profile+".config.toml")); err != nil {
			return err
		}
		if _, err := profileOverrides(p.native(i), profile); err != nil {
			return err
		}
	}
	cli, err := exec.LookPath(provider)
	if err != nil {
		return err
	}
	syncBin, err := helperPath()
	if err != nil {
		return err
	}
	if err := helperVersion(syncBin); err != nil {
		return err
	}
	if err := installIntegration(p.native(i), provider, syncBin); err != nil {
		return err
	}
	env := launchEnvironment(os.Environ(), p.Home, p.native(i), provider, group, "")
	base := nativeBase(provider, profile)
	if !i.Verified {
		if err := verifyLoggedOut(cli, provider, p.native(i), base, native, env); err != nil {
			return fmt.Errorf("fresh instance isolation check failed (instance retained for inspection): %w", err)
		}
		i.Verified = true
		if err := writeJSON(filepath.Join(p.label(provider, id), "instance.json"), i); err != nil {
			return err
		}
	}
	launch, err := startLaunch(syncBin, p.Home, group, provider, i.UUID)
	if err != nil {
		return err
	}
	env = launchEnvironment(os.Environ(), p.Home, p.native(i), provider, group, launch)
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		return err
	}
	if err := lock.Close(); err != nil {
		return err
	}
	args := append([]string{cli}, base...)
	if provider == "claude" {
		args = append(args, "--mcp-config", filepath.Join(p.native(i), "icarus-mcp.json"))
	}
	args = append(args, native...)
	// Replacing the process preserves the terminal, process group, signals,
	// native exit status, and job control. There is no terminal proxy.
	return syscall.Exec(cli, args, env)
}

func nativeBase(provider, profile string) []string {
	if provider == "claude" {
		return []string{"--setting-sources", "user", "--strict-mcp-config"}
	}
	args := []string{"--no-daemon", "-c", `cli_auth_credentials_store="file"`, "-c", `model_provider="openai"`}
	if profile != "" {
		args = append(args, "--profile", profile)
	}
	return args
}

func validateNativeArgs(provider string, args []string) error {
	for n := 0; n < len(args); n++ {
		arg := args[n]
		flag := strings.SplitN(arg, "=", 2)[0]
		if provider == "claude" {
			switch flag {
			case "--settings", "--setting-sources", "--mcp-config", "--remote-control", "--cloud", "--teleport", "--bare", "--bg", "--background":
				return fmt.Errorf("%s overrides Icarus's isolated native launch", flag)
			}
			switch arg {
			case "attach", "agents", "gateway", "respawn", "ultrareview":
				return fmt.Errorf("native command %s is outside isolated conversations", arg)
			}
			continue
		}
		switch flag {
		case "--remote", "--remote-auth-token-env", "--oss", "--local-provider", "--profile", "-p":
			return fmt.Errorf("%s overrides Icarus's isolated native launch", flag)
		case "-c", "--config":
			value := ""
			if parts := strings.SplitN(arg, "=", 2); len(parts) == 2 {
				value = parts[1]
			} else {
				n++
				if n >= len(args) {
					return errors.New("native config requires value")
				}
				value = args[n]
			}
			key := strings.TrimSpace(strings.SplitN(value, "=", 2)[0])
			if !allowedSetting("codex", key) {
				return fmt.Errorf("native config key %q is outside supported isolated settings", key)
			}
		default:
			if strings.HasPrefix(arg, "-p") && !strings.HasPrefix(arg, "--") {
				return errors.New("select a Codex profile with Icarus --profile")
			}
			if strings.HasPrefix(arg, "-c") && !strings.HasPrefix(arg, "--") && len(arg) > 2 {
				return errors.New("use -c KEY=VALUE for native config overrides")
			}
		}
		switch arg {
		case "app", "app-server", "remote-control", "exec-server", "agents", "cloud":
			return fmt.Errorf("native command %s is outside isolated conversations", arg)
		}
	}
	return nil
}

func launchEnvironment(ambient []string, home, native, provider, group, launch string) []string {
	keep := map[string]bool{}
	for _, key := range strings.Fields("HOME USER LOGNAME SHELL PATH TERM COLORTERM TERM_PROGRAM TERM_PROGRAM_VERSION TERM_SESSION_ID LANG LANGUAGE LC_ALL LC_CTYPE TZ TMPDIR TMP TEMP DISPLAY WAYLAND_DISPLAY XDG_RUNTIME_DIR SSH_AUTH_SOCK SSH_TTY SSH_CONNECTION EDITOR VISUAL PAGER LESS NO_COLOR FORCE_COLOR CLICOLOR CLICOLOR_FORCE COLUMNS LINES TMUX TMUX_PANE STY WINDOWID VTE_VERSION KITTY_WINDOW_ID ITERM_SESSION_ID SSL_CERT_FILE SSL_CERT_DIR HTTP_PROXY HTTPS_PROXY ALL_PROXY NO_PROXY http_proxy https_proxy all_proxy no_proxy CARGO_HOME RUSTUP_HOME GOPATH GOROOT NIX_SSL_CERT_FILE NIX_PATH") {
		keep[key] = true
	}
	values := map[string]string{}
	for _, pair := range ambient {
		key, value, ok := strings.Cut(pair, "=")
		if ok && (keep[key] || strings.HasPrefix(key, "LC_")) {
			values[key] = value
		}
	}
	values["ICARUS_HOME"] = home
	values["ICARUS_GROUP"] = group
	values["ICARUS_PROVIDER"] = provider
	if launch != "" {
		values["ICARUS_LAUNCH"] = launch
	}
	if provider == "codex" {
		values["CODEX_HOME"] = native
	} else {
		values["CLAUDE_CONFIG_DIR"] = native
		values["ANTHROPIC_CONFIG_DIR"] = native
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}

func helperPath() (string, error) {
	if path := os.Getenv("ICARUS_SYNC_BIN"); path != "" {
		return filepath.Abs(path)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	path := filepath.Join(filepath.Dir(exe), "icarus-sync")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	return exec.LookPath("icarus-sync")
}

func helperVersion(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return err
	}
	var v struct {
		Protocol int `json:"protocol"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	if v.Protocol != 1 {
		return errors.New("icarus-sync protocol mismatch; install both binaries together")
	}
	return nil
}

func startLaunch(helper, home, group, provider, instance string) (string, error) {
	req := map[string]any{"version": 1, "operation_id": uuid(), "command": map[string]any{"method": "launch_start", "provider": provider, "instance": instance}}
	b, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, helper, "--home", home, "--group", group, "api")
	cmd.Stdin = bytes.NewReader(b)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("coordination startup: %w: %s", err, stderr.String())
	}
	var response struct {
		Version int  `json:"version"`
		OK      bool `json:"ok"`
		Result  struct {
			Launch string `json:"launch"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &response); err != nil {
		return "", err
	}
	if response.Version != 1 || !response.OK || response.Result.Launch == "" {
		return "", errors.New("invalid coordination launch response")
	}
	return response.Result.Launch, nil
}

func verifyLoggedOut(cli, provider, home string, base, native, env []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if provider == "claude" {
		cmd := exec.CommandContext(ctx, cli, append(append([]string{}, base...), "auth", "status", "--json")...)
		cmd.Env = env
		out, err := cmd.Output()
		var status struct {
			LoggedIn *bool  `json:"loggedIn"`
			Method   string `json:"authMethod"`
			Provider string `json:"apiProvider"`
			Config   string `json:"configDirectory"`
		}
		if json.Unmarshal(out, &status) != nil || status.LoggedIn == nil {
			return fmt.Errorf("unrecognized Claude auth diagnostic: %w", err)
		}
		if *status.LoggedIn || status.Method != "none" || status.Provider != "firstParty" || filepath.Clean(status.Config) != filepath.Clean(home) {
			return errors.New("Claude found authentication, an alternate provider, or another configuration directory")
		}
		if !exitCode(err, 1) && err != nil {
			return err
		}
		return nil
	}
	// Doctor reports the effective configuration, including managed policy.
	// It exits nonzero when logged out; inspect fields, not its overall status.
	args := []string{}
	for n := 0; n < len(base); n++ {
		if base[n] == "--profile" {
			n++
			overrides, err := profileOverrides(home, base[n])
			if err != nil {
				return err
			}
			args = append(args, overrides...)
		} else {
			args = append(args, base[n])
		}
	}
	directory := ""
	for n := 0; n < len(native); n++ {
		arg := native[n]
		if arg == "-c" || arg == "--config" || arg == "-C" || arg == "--cd" {
			if n+1 >= len(native) {
				return errors.New("native option requires argument")
			}
			if arg == "-C" || arg == "--cd" {
				directory = native[n+1]
			} else {
				args = append(args, arg, native[n+1])
			}
			n++
		} else if strings.HasPrefix(arg, "--cd=") {
			directory = strings.TrimPrefix(arg, "--cd=")
		} else if strings.HasPrefix(arg, "--config=") {
			args = append(args, arg)
		}
	}
	args = append(args, "doctor", "--json")
	cmd := exec.CommandContext(ctx, cli, args...)
	cmd.Env = env
	cmd.Dir = directory
	out, err := cmd.Output()
	var report struct {
		SchemaVersion int `json:"schemaVersion"`
		Checks        map[string]struct {
			Status  string         `json:"status"`
			Summary string         `json:"summary"`
			Details map[string]any `json:"details"`
		} `json:"checks"`
	}
	if json.Unmarshal(out, &report) != nil || report.SchemaVersion != 1 {
		return fmt.Errorf("unrecognized Codex doctor diagnostic: %w", err)
	}
	auth, config := report.Checks["auth.credentials"], report.Checks["config.load"]
	if config.Status != "ok" || config.Details["CODEX_HOME"] != home || config.Details["model provider"] != "openai" || auth.Details["auth storage mode"] != "File" || auth.Details["auth file"] != filepath.Join(home, "auth.json") || auth.Summary != "no Codex credentials were found" {
		return fmt.Errorf("Codex effective auth/config does not establish a fresh file-backed login (config=%s, home=%v, provider=%v, storage=%v, auth=%s)", config.Status, config.Details["CODEX_HOME"], config.Details["model provider"], auth.Details["auth storage mode"], auth.Summary)
	}
	if !exitCode(err, 1) && err != nil {
		return err
	}
	return nil
}

// doctor does not accept --profile in Codex 0.162. Reproduce the selected
// supported profile settings as CLI overrides for the diagnostic invocation.
func profileOverrides(home, profile string) ([]string, error) {
	settings, err := readConfig(filepath.Join(home, profile+".config.toml"), "codex")
	if err != nil {
		return nil, err
	}
	report := ImportReport{}
	filterConfig(settings, "codex", "", &report)
	if len(report.Excluded) > 0 {
		return nil, fmt.Errorf("profile contains unsupported isolated settings: %s", strings.Join(report.Excluded, ", "))
	}
	var args []string
	var walk func(map[string]any, string) error
	walk = func(table map[string]any, prefix string) error {
		keys := make([]string, 0, len(table))
		for key := range table {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value := table[key]
			if child, ok := value.(map[string]any); ok {
				if err := walk(child, prefix+key+"."); err != nil {
					return err
				}
				continue
			}
			var b bytes.Buffer
			if err := toml.NewEncoder(&b).Encode(map[string]any{"value": value}); err != nil {
				return err
			}
			encoded := strings.TrimSpace(strings.TrimPrefix(b.String(), "value = "))
			args = append(args, "-c", prefix+key+"="+encoded)
		}
		return nil
	}
	err = walk(settings, "")
	return args, err
}

func exitCode(err error, code int) bool {
	var e *exec.ExitError
	return errors.As(err, &e) && e.ExitCode() == code
}

func installIntegration(home, provider, helper string) error {
	marker := filepath.Join(home, "icarus-integration.json")
	if data, err := os.ReadFile(marker); err == nil {
		var installed struct {
			Helper string `json:"helper"`
		}
		if err := json.Unmarshal(data, &installed); err != nil {
			return err
		}
		if installed.Helper != helper {
			return fmt.Errorf("instance uses helper %s; keep the installation path stable", installed.Helper)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	hook := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": shellQuote(helper) + " hook", "timeout": 10}}}
	events := []string{"SessionStart", "SubagentStart", "UserPromptSubmit", "Stop", "SubagentStop"}
	if provider == "claude" {
		events = append(events, "PostToolUse")
	}
	hooks := map[string]any{}
	for _, event := range events {
		hooks[event] = []any{hook}
	}
	if provider == "codex" {
		cfg, err := readConfig(filepath.Join(home, "config.toml"), provider)
		if err != nil {
			return err
		}
		cfg["cli_auth_credentials_store"] = "file"
		servers, _ := cfg["mcp_servers"].(map[string]any)
		if servers == nil {
			servers = map[string]any{}
		}
		servers["icarus"] = map[string]any{"command": helper, "args": []string{"mcp"}, "env_vars": []string{"ICARUS_HOME", "ICARUS_GROUP", "ICARUS_PROVIDER", "ICARUS_LAUNCH"}, "startup_timeout_sec": 15, "tool_timeout_sec": 86410}
		cfg["mcp_servers"] = servers
		if err := writeConfig(filepath.Join(home, "config.toml"), provider, cfg); err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(home, "hooks.json"), map[string]any{"hooks": hooks}); err != nil {
			return err
		}
		return writeJSON(marker, map[string]any{"helper": helper})
	}
	cfg, err := readConfig(filepath.Join(home, "settings.json"), provider)
	if err != nil {
		return err
	}
	cfg["hooks"] = hooks
	if err := writeConfig(filepath.Join(home, "settings.json"), provider, cfg); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(home, "icarus-mcp.json"), map[string]any{"mcpServers": map[string]any{
		"icarus": map[string]any{"command": helper, "args": []string{"mcp"}},
	}}); err != nil {
		return err
	}
	return writeJSON(marker, map[string]any{"helper": helper})
}
