package launcher

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

//go:embed defaults/*
var defaults embed.FS

type ImportReport struct {
	Included []string `json:"included"`
	Excluded []string `json:"excluded"`
}

type pathsFlag []string

func (p *pathsFlag) String() string     { return strings.Join(*p, ",") }
func (p *pathsFlag) Set(s string) error { *p = append(*p, s); return nil }

func importCommand(args []string) error {
	if len(args) == 0 {
		return usageError("template", "missing subcommand")
	}
	if isHelp(args[0]) {
		printHelp("template")
		return nil
	}
	if args[0] != "import" {
		return usageError("template", "unknown subcommand %q", args[0])
	}
	const command = "template import"
	if len(args) > 1 && isHelp(args[1]) {
		printHelp(command)
		return nil
	}
	if len(args) < 3 {
		return usageError(command, "expected a provider (codex or claude) and a template name")
	}
	if isHelp(args[2]) && (args[1] == "codex" || args[1] == "claude") {
		printHelp(command)
		return nil
	}
	provider, name := args[1], args[2]
	if err := validate(provider, name); err != nil {
		return usageError(command, "%v", err)
	}
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	from := f.String("from", "", "settings source directory")
	var instructions, skills pathsFlag
	f.Var(&instructions, "instruction", "explicit instruction file (one)")
	f.Var(&skills, "skill", "explicit skill directory (repeatable)")
	if err := f.Parse(args[3:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printHelp(command)
			return nil
		}
		return usageError(command, "%v", err)
	}
	if len(f.Args()) != 0 {
		return usageError(command, "unexpected argument %q", f.Args()[0])
	}
	if *from == "" {
		return usageError(command, "missing required option --from PATH")
	}
	if len(instructions) > 1 {
		return usageError(command, "at most one --instruction is allowed")
	}
	p, err := runtimePaths()
	if err != nil {
		return err
	}
	report, err := p.importTemplate(provider, name, *from, instructions, skills)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(b))
	return nil
}

func (p Paths) importTemplate(provider, name, source string, instructions, skills []string) (ImportReport, error) {
	var report ImportReport
	if err := validate(provider, name); err != nil {
		return report, err
	}
	dest := filepath.Join(p.Home, "templates", provider, name)
	if _, err := os.Lstat(dest); err == nil {
		return report, errors.New("template already exists; edit it or choose another name")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return report, err
	}
	temp, err := os.MkdirTemp(filepath.Dir(dest), ".import-")
	if err != nil {
		return report, err
	}
	defer os.RemoveAll(temp)
	filename := "config.toml"
	if provider == "claude" {
		filename = "settings.json"
	}
	config, err := readConfig(filepath.Join(source, filename), provider)
	if err != nil {
		return report, err
	}
	baseConfig := map[string]any{}
	for key, value := range config {
		if provider == "codex" && key == "profiles" {
			continue
		}
		baseConfig[key] = value
	}
	filtered := filterConfig(baseConfig, provider, "", &report)
	if err := writeConfig(filepath.Join(temp, filename), provider, filtered); err != nil {
		return report, err
	}
	if provider == "codex" {
		// The current CLI uses separate profile files. Convert older inline
		// profiles as well, rejecting collisions instead of choosing silently.
		if profiles, ok := config["profiles"].(map[string]any); ok {
			for profile, value := range profiles {
				if !labelPattern.MatchString(profile) {
					return report, fmt.Errorf("invalid profile %q", profile)
				}
				settings, ok := value.(map[string]any)
				if !ok {
					return report, errors.New("profile must be a table")
				}
				out := filterConfig(settings, provider, "profiles."+profile+".", &report)
				if err := writeConfig(filepath.Join(temp, profile+".config.toml"), provider, out); err != nil {
					return report, err
				}
			}
		}
		profiles, err := filepath.Glob(filepath.Join(source, "*.config.toml"))
		if err != nil {
			return report, err
		}
		for _, path := range profiles {
			name := strings.TrimSuffix(filepath.Base(path), ".config.toml")
			if !labelPattern.MatchString(name) {
				return report, fmt.Errorf("invalid profile %q", name)
			}
			if _, err := os.Stat(filepath.Join(temp, filepath.Base(path))); err == nil {
				return report, fmt.Errorf("duplicate inline/file profile %q", name)
			}
			cfg, err := readConfig(path, provider)
			if err != nil {
				return report, err
			}
			out := filterConfig(cfg, provider, name+".config.toml.", &report)
			if err := writeConfig(filepath.Join(temp, filepath.Base(path)), provider, out); err != nil {
				return report, err
			}
		}
	}
	for _, path := range instructions {
		if err := regularFile(path); err != nil {
			return report, err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return report, err
		}
		if len(b) > 1<<20 {
			return report, errors.New("instruction exceeds 1 MiB")
		}
		name := "AGENTS.md"
		if provider == "claude" {
			name = "CLAUDE.md"
		}
		if err := atomicWrite(filepath.Join(temp, name), b); err != nil {
			return report, err
		}
		report.Included = append(report.Included, "explicit instruction: "+path)
	}
	for _, path := range skills {
		name := filepath.Base(filepath.Clean(path))
		if !labelPattern.MatchString(name) {
			return report, errors.New("invalid skill name")
		}
		if err := copyTree(path, filepath.Join(temp, "skills", name)); err != nil {
			return report, err
		}
		report.Included = append(report.Included, "explicit skill: "+path)
	}
	report.Excluded = append(report.Excluded, "all files except supported settings/profiles and explicitly selected instructions/skills (credentials, histories, sessions, trust caches, executable integrations)")
	sort.Strings(report.Included)
	sort.Strings(report.Excluded)
	if err := writeJSON(filepath.Join(temp, "import-report.json"), report); err != nil {
		return report, err
	}
	if err := os.Rename(temp, dest); err != nil {
		return report, err
	}
	return report, nil
}

func allowedSetting(provider, path string) bool {
	if provider == "codex" {
		switch path {
		case "model", "model_reasoning_effort", "plan_mode_reasoning_effort", "personality", "service_tier", "model_verbosity", "model_reasoning_summary", "approval_policy", "approvals_reviewer", "sandbox_mode", "web_search",
			"tui.fullscreen_transcript", "tui.alternate_screen", "tui.status_line", "tui.status_line_use_colors", "tui.theme", "tui.terminal_title", "tui.animations", "tui.notifications",
			"features.terminal_resize_reflow", "features.js_repl":
			return true
		}
	} else {
		switch path {
		case "model", "effortLevel", "tui", "theme", "language", "alwaysThinkingEnabled", "permissions.allow", "permissions.deny", "permissions.ask", "permissions.defaultMode":
			return true
		}
		parts := strings.Split(path, ".")
		return len(parts) == 3 && parts[0] == "modelSettings" && parts[2] == "effortLevel"
	}
	return false
}

func filterConfig(input map[string]any, provider, prefix string, report *ImportReport) map[string]any {
	var walk func(map[string]any, string) map[string]any
	walk = func(m map[string]any, base string) map[string]any {
		out := map[string]any{}
		for key, value := range m {
			path := base + key
			if child, ok := value.(map[string]any); ok {
				if len(child) == 0 {
					report.Excluded = append(report.Excluded, prefix+path)
				}
				filtered := walk(child, path+".")
				if len(filtered) > 0 {
					out[key] = filtered
				}
				continue
			}
			if allowedSetting(provider, path) {
				out[key] = value
				report.Included = append(report.Included, prefix+path)
			} else {
				report.Excluded = append(report.Excluded, prefix+path)
			}
		}
		return out
	}
	return walk(input, "")
}

func regularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("expected regular file: %s", path)
	}
	if info.Size() > 16<<20 {
		return fmt.Errorf("file exceeds 16 MiB: %s", path)
	}
	return nil
}

func readConfig(path, provider string) (map[string]any, error) {
	if err := regularFile(path); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if provider == "codex" {
		err = toml.Unmarshal(b, &out)
	} else {
		err = json.Unmarshal(b, &out)
	}
	return out, err
}

func writeConfig(path, provider string, config map[string]any) error {
	if provider == "claude" {
		return writeJSON(path, config)
	}
	var b bytes.Buffer
	if err := toml.NewEncoder(&b).Encode(config); err != nil {
		return err
	}
	return atomicWrite(path, b.Bytes())
}

func seedDefault(dest, provider string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return err
	}
	temp, err := os.MkdirTemp(filepath.Dir(dest), ".seed-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	source, name := "defaults/codex.toml", "config.toml"
	if provider == "claude" {
		source, name = "defaults/claude.json", "settings.json"
	}
	b, err := defaults.ReadFile(source)
	if err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(temp, name), b); err != nil {
		return err
	}
	if err := os.Rename(temp, dest); err != nil {
		if _, statErr := os.Stat(filepath.Join(dest, name)); statErr == nil {
			return nil
		}
		return err
	}
	return nil
}

func sanitizeInstance(home, provider string) error {
	entries, err := os.ReadDir(home)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "skills" && entry.IsDir() || name == "import-report.json" || name == "AGENTS.md" || name == "CLAUDE.md" {
			continue
		}
		config := name == "config.toml" || name == "settings.json" || provider == "codex" && strings.HasSuffix(name, ".config.toml")
		if !config {
			return fmt.Errorf("unsupported template file %s; credentials and integrations are never copied", name)
		}
		settings, err := readConfig(filepath.Join(home, name), provider)
		if err != nil {
			return err
		}
		report := ImportReport{}
		filtered := filterConfig(settings, provider, "", &report)
		if len(report.Excluded) > 0 {
			return fmt.Errorf("unsupported template settings in %s: %s", name, strings.Join(report.Excluded, ", "))
		}
		if err := writeConfig(filepath.Join(home, name), provider, filtered); err != nil {
			return err
		}
	}
	return nil
}
