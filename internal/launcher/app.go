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

const usage = `icarus template import codex|claude NAME --from PATH [--instruction PATH] [--skill PATH]
icarus new codex|claude ID [--template NAME] [--profile NAME] [--group GROUP] [-- NATIVE_ARGS...]
icarus open codex|claude ID [--profile NAME] [--group GROUP] [-- NATIVE_ARGS...]
icarus list

new creates a persistent, logged-out instance and opens its native CLI.
Runtime data: ICARUS_HOME (default ~/.local/share/icarus).
Profile names select Codex's NAME.config.toml. Additional args follow --.
`

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
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		fmt.Print(usage)
		return nil
	}
	if args[0] == "version" {
		fmt.Println("icarus 0.1.0 (subprocess protocol v1)")
		return nil
	}
	home := os.Getenv("ICARUS_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		home = filepath.Join(userHome, ".local", "share", "icarus")
	}
	home, err := filepath.Abs(home)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0700); err != nil {
		return err
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return err
	}
	p := Paths{home}
	if args[0] == "list" {
		return p.list()
	}
	if args[0] == "template" {
		return p.importCommand(args[1:])
	}
	if len(args) < 3 || (args[0] != "new" && args[0] != "open") {
		return errors.New(usage)
	}
	provider, id := args[1], args[2]
	if err := validate(provider, id); err != nil {
		return err
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	template := flags.String("template", "default", "template snapshot")
	profile := flags.String("profile", "", "native Codex settings profile")
	group := flags.String("group", "default", "coordination group")
	if err := flags.Parse(args[3:]); err != nil {
		return err
	}
	if !labelPattern.MatchString(*template) || !labelPattern.MatchString(*group) {
		return errors.New("invalid template or group label")
	}
	if *profile != "" && (provider != "codex" || !labelPattern.MatchString(*profile)) {
		return errors.New("--profile requires a valid Codex profile name")
	}
	if args[0] == "open" && *template != "default" {
		return errors.New("--template only applies to new instances")
	}
	if args[0] == "new" {
		if _, err := p.create(provider, id, *template); err != nil {
			return err
		}
	}
	return p.launch(provider, id, *profile, *group, flags.Args())
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
