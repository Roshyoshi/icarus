package launcher

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

var labelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

type Instance struct {
	Version  int    `json:"version"`
	Provider string `json:"provider"`
	ID       string `json:"id"`
	UUID     string `json:"uuid"`
	Verified bool   `json:"verified_logged_out"`
}

type Paths struct{ Home string }

func Run(args []string) error {
	if len(args) == 0 || isHelp(args[0]) || args[0] == "help" {
		printHelp("")
		return nil
	}
	command := args[0]
	switch command {
	case "template":
		return importCommand(args[1:])
	case "new", "open", "list", "version":
		if len(args) > 1 && isHelp(args[1]) {
			printHelp(command)
			return nil
		}
	default:
		return usageError("", "unknown command %q", command)
	}
	if command == "list" || command == "version" {
		if len(args) > 1 {
			return usageError(command, "unexpected argument %q", args[1])
		}
		if command == "version" {
			fmt.Println("icarus 0.1.0 (subprocess protocol v1)")
			return nil
		}
		p, err := runtimePaths()
		if err != nil {
			return err
		}
		return p.list()
	}
	if len(args) < 3 {
		if len(args) == 2 && (args[1] == "codex" || args[1] == "claude") {
			return usageError(command, "missing instance ID")
		}
		return usageError(command, "expected a provider (codex or claude) and an instance ID")
	}
	if isHelp(args[2]) && (args[1] == "codex" || args[1] == "claude") {
		printHelp(command)
		return nil
	}
	provider, id := args[1], args[2]
	if err := validate(provider, id); err != nil {
		return usageError(command, "%v", err)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	template := "default"
	if command == "new" {
		flags.StringVar(&template, "template", "default", "template snapshot")
	}
	profile := flags.String("profile", "", "native Codex settings profile")
	group := flags.String("group", "default", "coordination group")
	if err := flags.Parse(args[3:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printHelp(command)
			return nil
		}
		return usageError(command, "%v", err)
	}
	if !labelPattern.MatchString(template) {
		return usageError(command, "invalid template label %q", template)
	}
	if !labelPattern.MatchString(*group) {
		return usageError(command, "invalid group label %q", *group)
	}
	if *profile != "" && (provider != "codex" || !labelPattern.MatchString(*profile)) {
		return usageError(command, "--profile requires a valid Codex profile name")
	}
	p, err := runtimePaths()
	if err != nil {
		return err
	}
	if command == "new" {
		if _, err := p.create(provider, id, template); err != nil {
			return err
		}
	}
	return p.launch(provider, id, *profile, *group, flags.Args())
}

func runtimePaths() (Paths, error) {
	home := os.Getenv("ICARUS_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return Paths{}, err
		}
		home = filepath.Join(userHome, ".local", "share", "icarus")
	}
	home, err := filepath.Abs(home)
	if err != nil {
		return Paths{}, err
	}
	if err := os.MkdirAll(home, 0700); err != nil {
		return Paths{}, err
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return Paths{}, err
	}
	return Paths{home}, nil
}

func validate(provider, id string) error {
	if provider != "codex" && provider != "claude" {
		return fmt.Errorf("unsupported provider %q", provider)
	}
	if !labelPattern.MatchString(id) {
		return errors.New("labels must match [A-Za-z0-9][A-Za-z0-9_-]{0,63}")
	}
	return nil
}

func uuid() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (p Paths) label(provider, id string) string {
	return filepath.Join(p.Home, "instances", provider, id)
}
func (p Paths) native(i Instance) string { return filepath.Join(p.Home, "homes", i.UUID) }

func (p Paths) create(provider, id, template string) (Instance, error) {
	if err := validate(provider, id); err != nil {
		return Instance{}, err
	}
	if !labelPattern.MatchString(template) {
		return Instance{}, errors.New("invalid template")
	}
	label := p.label(provider, id)
	if err := os.MkdirAll(filepath.Dir(label), 0700); err != nil {
		return Instance{}, err
	}
	// mkdir is the no-replace operation; even concurrent creators cannot reuse
	// an existing label. Never derive the native credential directory from it.
	if err := os.Mkdir(label, 0700); err != nil {
		return Instance{}, fmt.Errorf("create %s/%s: %w", provider, id, err)
	}
	i := Instance{Version: 1, Provider: provider, ID: id, UUID: uuid()}
	success := false
	homeCreated := false
	defer func() {
		if !success {
			_ = os.RemoveAll(label)
			if homeCreated {
				_ = os.RemoveAll(p.native(i))
			}
		}
	}()
	if err := os.MkdirAll(filepath.Dir(p.native(i)), 0700); err != nil {
		return Instance{}, err
	}
	if err := os.Mkdir(p.native(i), 0700); err != nil {
		return Instance{}, err
	}
	homeCreated = true
	source := filepath.Join(p.Home, "templates", provider, template)
	if _, err := os.Stat(source); errors.Is(err, os.ErrNotExist) && template == "default" {
		if err := seedDefault(source, provider); err != nil {
			return Instance{}, err
		}
	}
	if err := copyTree(source, p.native(i)); err != nil {
		return Instance{}, err
	}
	if err := sanitizeInstance(p.native(i), provider); err != nil {
		return Instance{}, err
	}
	if err := writeJSON(filepath.Join(label, "instance.json"), i); err != nil {
		return Instance{}, err
	}
	success = true
	return i, nil
}

func (p Paths) load(provider, id string) (Instance, error) {
	var i Instance
	b, err := os.ReadFile(filepath.Join(p.label(provider, id), "instance.json"))
	if err != nil {
		return i, err
	}
	if err := json.Unmarshal(b, &i); err != nil {
		return i, err
	}
	if i.Version != 1 || i.Provider != provider || i.ID != id || !regexp.MustCompile(`^[a-f0-9-]{36}$`).MatchString(i.UUID) {
		return i, errors.New("invalid instance metadata")
	}
	return i, nil
}

func (p Paths) list() error {
	for _, provider := range []string{"codex", "claude"} {
		entries, err := os.ReadDir(filepath.Join(p.Home, "instances", provider))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				fmt.Printf("%s\t%s\n", provider, entry.Name())
			}
		}
	}
	return nil
}

func writeJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(b, '\n'))
}

func atomicWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".icarus-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func copyTree(source, dest string) error {
	return filepath.WalkDir(source, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink %s", path)
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("not a regular file: %s", path)
		}
		if err := regularFile(path); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		info, err := d.Info()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600|info.Mode().Perm()&0111)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		if err == nil {
			err = out.Sync()
		}
		closeErr := out.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
}

func lockInstance(path string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(path, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
