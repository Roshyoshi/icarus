package launcher

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
)

var testBin, fixtureBin, syncBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "icarus-go-tests-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	goBin := filepath.Join(runtime.GOROOT(), "bin", "go")
	testBin = filepath.Join(dir, "icarus")
	fixtureBin = filepath.Join(dir, "providers")
	if err := os.Mkdir(fixtureBin, 0700); err != nil {
		panic(err)
	}
	for _, build := range [][2]string{{testBin, "../../cmd/icarus"}, {filepath.Join(fixtureBin, "codex"), "./testdata/provider"}} {
		if b, err := exec.Command(goBin, "build", "-o", build[0], build[1]).CombinedOutput(); err != nil {
			panic(string(b))
		}
	}
	if err := os.Link(filepath.Join(fixtureBin, "codex"), filepath.Join(fixtureBin, "claude")); err != nil {
		panic(err)
	}
	syncBin, err = filepath.Abs("../../target/debug/icarus-sync")
	if err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func testPaths(t *testing.T) Paths { t.Helper(); return Paths{filepath.Join(t.TempDir(), "icarus")} }
func put(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func runCLI(t *testing.T, p Paths, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(testBin, args...)
	cmd.Env = append(os.Environ(), "ICARUS_HOME="+p.Home, "ICARUS_SYNC_BIN="+syncBin, "PATH="+fixtureBin+":"+os.Getenv("PATH"), "OPENAI_API_KEY=ambient-openai", "CODEX_ACCESS_TOKEN=ambient-token", "ANTHROPIC_API_KEY=ambient-anthropic", "CLAUDE_CODE_OAUTH_TOKEN=ambient-claude", "CLAUDE_CODE_USE_BEDROCK=1", "AWS_ACCESS_KEY_ID=ambient-aws", "CLAUDE_CONFIG_DIR=/must/not/use", "ANTHROPIC_CONFIG_DIR=/must/not/use", "CODEX_HOME=/must/not/use")
	return cmd.CombinedOutput()
}

func TestInstancesLoginIndependenceAndNativeExit(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			p := testPaths(t)
			record := filepath.Join(t.TempDir(), "launch.json")
			if out, err := runCLI(t, p, "new", provider, "a", "--", "--fixture-record", record, "login-fixture"); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			a, err := p.load(provider, "a")
			if err != nil {
				t.Fatal(err)
			}
			if !a.Verified {
				t.Fatal("fresh login was not checked")
			}
			var launch struct {
				Env      map[string]string `json:"env"`
				LoggedIn bool              `json:"logged_in"`
				Args     []string          `json:"args"`
			}
			b, _ := os.ReadFile(record)
			if err := json.Unmarshal(b, &launch); err != nil {
				t.Fatal(err)
			}
			if launch.LoggedIn {
				t.Fatal("ambient credentials leaked")
			}
			for _, key := range []string{"OPENAI_API_KEY", "CODEX_ACCESS_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "AWS_ACCESS_KEY_ID"} {
				if launch.Env[key] != "" {
					t.Fatalf("leaked %s", key)
				}
			}
			if out, err := runCLI(t, p, "new", provider, "b", "--", "--fixture-record", record); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			b, _ = os.ReadFile(record)
			_ = json.Unmarshal(b, &launch)
			if launch.LoggedIn {
				t.Fatal("A authenticated B")
			}
			other, _ := p.load(provider, "b")
			if other.UUID == a.UUID {
				t.Fatal("backing directory reused")
			}
			if out, err := runCLI(t, p, "open", provider, "a", "--", "--fixture-record", record); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			b, _ = os.ReadFile(record)
			_ = json.Unmarshal(b, &launch)
			if !launch.LoggedIn {
				t.Fatal("A lost its login")
			}
			before, _ := os.ReadFile(filepath.Join(p.label(provider, "a"), "instance.json"))
			if _, err := runCLI(t, p, "new", provider, "a"); err == nil {
				t.Fatal("existing ID creation succeeded")
			}
			after, _ := os.ReadFile(filepath.Join(p.label(provider, "a"), "instance.json"))
			if !bytes.Equal(before, after) {
				t.Fatal("existing instance altered")
			}
			_, err = runCLI(t, p, "open", provider, "a", "--", "--fixture-exit", "42")
			if !exitCode(err, 42) {
				t.Fatalf("native exit lost: %v", err)
			}
			_, err = runCLI(t, p, "open", provider, "a", "--", "--fixture-signal")
			e, ok := err.(*exec.ExitError)
			if !ok || e.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
				t.Fatalf("native signal lost: %v", err)
			}
		})
	}
}

func TestManagedAuthenticationFailsClosed(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		p := testPaths(t)
		i, err := p.create(provider, "blocked", "default")
		if err != nil {
			t.Fatal(err)
		}
		put(t, filepath.Join(p.native(i), "managed-auth"), "policy fixture")
		out, err := runCLI(t, p, "open", provider, "blocked")
		if err == nil || !strings.Contains(string(out), "isolation check failed") {
			t.Fatalf("managed auth accepted: %v %s", err, out)
		}
		i, err = p.load(provider, "blocked")
		if err != nil || i.Verified {
			t.Fatal("failed check marked verified")
		}
	}
}

func TestImportProfilesAndTemplateSnapshots(t *testing.T) {
	p := testPaths(t)
	source := t.TempDir()
	put(t, filepath.Join(source, "config.toml"), `model = "gpt-6-astra"
model_reasoning_effort = "max"
notify = ["do-not-run"]
cli_auth_credentials_store = "keyring"
[projects."/tmp"]
trust_level = "trusted"
[mcp_servers.old]
command = "do-not-run"
[profiles.legacy]
model = "gpt-6-astra"
model_provider = "credential-helper"
`)
	put(t, filepath.Join(source, "reviewer.config.toml"), "model_reasoning_effort = \"max\"\n")
	put(t, filepath.Join(source, "auth.json"), "DO NOT COPY")
	report, err := p.importTemplate("codex", "default", source, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Excluded) < 4 {
		t.Fatalf("missing exclusions: %+v", report)
	}
	a, err := p.create("codex", "a", "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.native(a), "auth.json")); !os.IsNotExist(err) {
		t.Fatal("credentials copied")
	}
	if _, err := os.Stat(filepath.Join(p.native(a), "legacy.config.toml")); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(p.Home, "templates/codex/default/reviewer.config.toml"), "model_reasoning_effort = \"low\"\n")
	b, err := p.create("codex", "b", "default")
	if err != nil {
		t.Fatal(err)
	}
	old, _ := os.ReadFile(filepath.Join(p.native(a), "reviewer.config.toml"))
	fresh, _ := os.ReadFile(filepath.Join(p.native(b), "reviewer.config.toml"))
	if bytes.Equal(old, fresh) || !bytes.Contains(old, []byte("max")) {
		t.Fatal("template snapshots not independent")
	}
	record := filepath.Join(t.TempDir(), "launch.json")
	if out, err := runCLI(t, p, "open", "codex", "a", "--profile", "reviewer", "--", "--fixture-record", record); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	data, _ := os.ReadFile(record)
	if !bytes.Contains(data, []byte(`"--profile","reviewer"`)) {
		t.Fatal("profile not passed to native CLI")
	}
	if _, err := p.importTemplate("codex", "default", source, nil, nil); err == nil {
		t.Fatal("template overwritten")
	}
}

func TestClaudeImportAndExplicitAssets(t *testing.T) {
	p := testPaths(t)
	source := t.TempDir()
	skill := filepath.Join(t.TempDir(), "review")
	put(t, filepath.Join(source, "settings.json"), `{"model":"opus","modelSettings":{"claude-opus-5-5":{"effortLevel":"xhigh"}},"env":{"ANTHROPIC_API_KEY":"secret"},"apiKeyHelper":"do-not-run","hooks":{"Stop":[]},"permissions":{"deny":["Bash(rm *)"]}}`)
	put(t, filepath.Join(source, "CLAUDE.md"), "selected instruction")
	put(t, filepath.Join(skill, "SKILL.md"), "selected skill")
	report, err := p.importTemplate("claude", "default", source, []string{filepath.Join(source, "CLAUDE.md")}, []string{skill})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Excluded) < 3 {
		t.Fatalf("missing credential exclusions: %+v", report)
	}
	i, err := p.create("claude", "a", "default")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(p.native(i), "settings.json"))
	if bytes.Contains(data, []byte("secret")) || bytes.Contains(data, []byte("do-not-run")) {
		t.Fatal("unsafe settings imported")
	}
	if _, err := os.Stat(filepath.Join(p.native(i), "skills/review/SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if err := installIntegration(p.native(i), "claude", syncBin); err != nil {
		t.Fatal(err)
	}
	config, _ := readConfig(filepath.Join(p.native(i), "settings.json"), "claude")
	config["hooks"] = map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{"command": "my-restriction", "type": "command"}}}}}
	if err := writeConfig(filepath.Join(p.native(i), "settings.json"), "claude", config); err != nil {
		t.Fatal(err)
	}
	if err := installIntegration(p.native(i), "claude", syncBin); err != nil {
		t.Fatal(err)
	}
	got, _ := readConfig(filepath.Join(p.native(i), "settings.json"), "claude")
	if !reflect.DeepEqual(got, config) {
		t.Fatal("reopen overwrote native restrictions")
	}
}

func TestConcurrentCreateIsNoReplace(t *testing.T) {
	p := testPaths(t)
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := p.create("codex", "one", "default"); results <- err }()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("%d creators succeeded", success)
	}
}

func TestSourceOverridesAndTemplateCredentialsAreRejected(t *testing.T) {
	for _, tc := range []struct {
		provider string
		args     []string
	}{{"codex", []string{"--config", "cli_auth_credentials_store=\"keyring\""}}, {"codex", []string{"--remote=x"}}, {"codex", []string{"-cmodel_provider=x"}}, {"claude", []string{"--settings", "credentials.json"}}, {"claude", []string{"--setting-sources=project"}}} {
		if validateNativeArgs(tc.provider, tc.args) == nil {
			t.Fatalf("accepted %v", tc.args)
		}
	}
	p := testPaths(t)
	source := filepath.Join(p.Home, "templates/codex/default")
	if err := seedDefault(source, "codex"); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(source, "auth.json"), "secret")
	if _, err := p.create("codex", "bad", "default"); err == nil {
		t.Fatal("template credentials accepted")
	}
	if _, err := os.Stat(p.label("codex", "bad")); !os.IsNotExist(err) {
		t.Fatal("failed creation retained partial label")
	}
}
