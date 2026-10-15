package launcher

import "fmt"

// UsageError distinguishes invalid arguments from runtime failures.
type UsageError struct {
	command string
	message string
}

func (e *UsageError) Error() string {
	prefix, help := "", "icarus"
	if e.command != "" {
		prefix = e.command + ": "
		help += " " + e.command
	}
	return fmt.Sprintf("%s%s\nusage: %s\nTry '%s --help' for more information.",
		prefix, e.message, commandUsage(e.command), help)
}

func usageError(command, format string, args ...any) error {
	return &UsageError{command: command, message: fmt.Sprintf(format, args...)}
}

func commandUsage(command string) string {
	switch command {
	case "new", "open":
		return "icarus " + command + " {codex|claude} ID [OPTIONS] [-- NATIVE_ARGS...]"
	case "template", "template import":
		return "icarus template import {codex|claude} NAME --from PATH [OPTIONS]"
	case "list", "version":
		return "icarus " + command
	default:
		return "icarus COMMAND [ARGS...]"
	}
}

func isHelp(arg string) bool { return arg == "-h" || arg == "--help" }

func printHelp(command string) {
	fmt.Println("usage:", commandUsage(command))
	switch command {
	case "new", "open":
		if command == "new" {
			fmt.Println("\nCreate a persistent, logged-out instance and open its native CLI.")
		} else {
			fmt.Println("\nOpen an existing instance, preserving its login and native session data.")
		}
		fmt.Println("\nOptions:")
		if command == "new" {
			fmt.Println("  --template NAME  Template snapshot (default: default)")
		}
		fmt.Println("  --profile NAME   Codex settings profile (NAME.config.toml)")
		fmt.Println("  --group GROUP    Coordination group (default: default)")
		fmt.Println("  -h, --help       Show this help")
		fmt.Println("\nRun in the current directory. Pass native CLI arguments after --.")
	case "template", "template import":
		fmt.Println("\nImport supported settings into an editable template snapshot.")
		fmt.Println("\nOptions:")
		fmt.Println("  --from PATH         Source configuration directory (required)")
		fmt.Println("  --instruction PATH  Explicit instruction file (at most one)")
		fmt.Println("  --skill PATH        Explicit skill directory (repeatable)")
		fmt.Println("  -h, --help          Show this help")
	case "list":
		fmt.Println("\nList providers and instance IDs.")
	case "version":
		fmt.Println("\nShow the Icarus version.")
	default:
		fmt.Println(`
Commands:
  new              Create an instance and open its native CLI
  open             Open an existing instance
  list             List instances
  template import  Import a template snapshot
  version          Show the version

Runtime data: ICARUS_HOME (default ~/.local/share/icarus).
Use 'icarus COMMAND --help' for command help.`)
	}
}
