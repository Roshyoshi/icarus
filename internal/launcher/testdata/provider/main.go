package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func main() {
	provider := filepath.Base(os.Args[0])
	home := os.Getenv("CODEX_HOME")
	if provider == "claude" {
		home = os.Getenv("CLAUDE_CONFIG_DIR")
	}
	credential := filepath.Join(home, "fixture-login")
	_, authErr := os.Stat(credential)
	_, policyErr := os.Stat(filepath.Join(home, "managed-auth"))
	logged := authErr == nil || policyErr == nil || os.Getenv("OPENAI_API_KEY") != "" || os.Getenv("ANTHROPIC_API_KEY") != ""
	args := os.Args[1:]
	for _, arg := range args {
		if arg == "doctor" {
			storage, summary := "File", "no Codex credentials were found"
			if logged {
				storage, summary = "Keyring", "authenticated"
			}
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"schemaVersion": 1, "checks": map[string]any{
				"auth.credentials": map[string]any{"status": "fail", "summary": summary, "details": map[string]any{"auth storage mode": storage, "auth file": filepath.Join(home, "auth.json")}},
				"config.load":      map[string]any{"status": "ok", "details": map[string]any{"CODEX_HOME": home, "model provider": "openai"}},
			}})
			os.Exit(1)
		}
		if arg == "auth" {
			method := "none"
			if logged {
				method = "oauth_token"
			}
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"loggedIn": logged, "authMethod": method, "apiProvider": "firstParty", "configDirectory": home})
			if !logged {
				os.Exit(1)
			}
			return
		}
	}
	for n, arg := range args {
		switch arg {
		case "login-fixture":
			if err := os.WriteFile(credential, []byte("FAKE TEST LOGIN"), 0600); err != nil {
				panic(err)
			}
		case "--fixture-record":
			env := map[string]string{}
			for _, pair := range os.Environ() {
				k, v, _ := strings.Cut(pair, "=")
				env[k] = v
			}
			b, _ := json.Marshal(map[string]any{"args": args, "env": env, "logged_in": logged, "pid": os.Getpid()})
			if err := os.WriteFile(args[n+1], b, 0600); err != nil {
				panic(err)
			}
		case "--fixture-exit":
			code, _ := strconv.Atoi(args[n+1])
			os.Exit(code)
		case "--fixture-signal":
			_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		}
	}
	fmt.Println("fixture native CLI")
}
