package launcher

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// These diagnostics never log in or run a model. Each invocation gets a
// disposable native home and the same isolation settings as interactive use.
func TestRealLoggedOutDiagnostics(t *testing.T) {
	if os.Getenv("ICARUS_REAL_CLI_TESTS") != "1" {
		t.Skip("opt-in native CLI diagnostics")
	}
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			if _, err := exec.LookPath(provider); err != nil {
				t.Fatal(err)
			}
			p := testPaths(t)
			if provider == "codex" {
				dest := filepath.Join(p.Home, "templates/codex/default")
				if err := seedDefault(dest, provider); err != nil {
					t.Fatal(err)
				}
				put(t, filepath.Join(dest, "reviewer.config.toml"), "model_reasoning_effort = \"max\"\n")
			}
			args := []string{"new", provider, "native-diagnostic"}
			if provider == "codex" {
				args = append(args, "--profile", "reviewer")
			}
			args = append(args, "--", "--help")
			cmd := exec.Command(testBin, args...)
			cmd.Env = append(os.Environ(), "ICARUS_HOME="+p.Home, "ICARUS_SYNC_BIN="+syncBin,
				"OPENAI_API_KEY=icarus-invalid-fixture", "ANTHROPIC_API_KEY=icarus-invalid-fixture", "CLAUDE_CODE_OAUTH_TOKEN=icarus-invalid-fixture")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			i, err := p.load(provider, "native-diagnostic")
			if err != nil || !i.Verified {
				t.Fatalf("native diagnostics did not establish isolation: %v", err)
			}
		})
	}
}
