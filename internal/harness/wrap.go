package harness

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type stringListFlag []string

func (f *stringListFlag) String() string { return strings.Join(*f, ",") }

func (f *stringListFlag) Set(value string) error {
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			*f = append(*f, item)
		}
	}
	return nil
}

type wrapChange struct {
	server Server
	verb   string
}

func runWrap(args []string, stdout, stderr io.Writer, undo bool) int {
	name := "wrap"
	if undo {
		name = "unwrap"
	}
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var harnesses, servers stringListFlag
	flags.Var(&harnesses, "harness", "harnesses to update (claude,codex)")
	flags.Var(&servers, "server", "server name to update (repeatable)")
	idle := flags.String("idle", "", "proxy idle timeout in seconds")
	dryRun := flags.Bool("dry-run", false, "show changes without writing")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "%s: unexpected positional arguments\n", name)
		return 2
	}
	if len(harnesses) == 0 {
		harnesses = []string{"claude", "codex"}
	}
	selectedHarnesses := make(map[string]bool, len(harnesses))
	for _, harness := range harnesses {
		if harness != "claude" && harness != "codex" {
			fmt.Fprintf(stderr, "%s: unknown harness %q\n", name, harness)
			return 2
		}
		selectedHarnesses[harness] = true
	}
	selectedServers := make(map[string]bool, len(servers))
	for _, server := range servers {
		selectedServers[server] = true
	}
	proxyFlags := []string(nil)
	if *idle != "" {
		proxyFlags = []string{"--idle", *idle}
	}

	paths := configPaths()
	var backupRun string
	wrapped, unwrapped := 0, 0
	failed := false
	for _, harness := range []string{"claude", "codex"} {
		if !selectedHarnesses[harness] {
			continue
		}
		path := paths[harness]
		original, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			fmt.Fprintf(stderr, "%s: %s: %v\n", harness, path, err)
			failed = true
			continue
		}
		entries, err := loadServers(harness, original)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %s: %v\n", harness, path, err)
			failed = true
			continue
		}
		updated := original
		changes := make([]wrapChange, 0)
		for _, server := range entries {
			if !server.Enabled || (len(selectedServers) != 0 && !selectedServers[server.Name]) {
				continue
			}
			var argv []string
			verb := "wrapped"
			if undo {
				if !server.Wrapped {
					continue
				}
				argv = server.Argv
				verb = "unwrapped"
			} else {
				if server.Wrapped {
					continue
				}
				argv = WrappedArgv(proxyBinary(), proxyFlags, server.Argv)
			}
			if harness == "claude" {
				updated, err = ClaudeSetArgv(updated, server.Scope, server.Name, argv)
			} else {
				updated, err = CodexSetArgv(updated, server.Name, argv)
			}
			if err != nil {
				fmt.Fprintf(stderr, "%s: %s: %v\n", harness, path, err)
				failed = true
				changes = nil
				break
			}
			changes = append(changes, wrapChange{server: server, verb: verb})
		}
		if len(changes) == 0 || failed && !bytes.Equal(updated, original) && len(changes) == 0 {
			continue
		}
		if *dryRun {
			printWrapChanges(stdout, changes)
			if undo {
				unwrapped += len(changes)
			} else {
				wrapped += len(changes)
			}
			continue
		}
		current, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(current, original) {
			if err == nil {
				err = errors.New("config changed since it was read")
			}
			fmt.Fprintf(stderr, "%s: %s: %v\n", harness, path, err)
			failed = true
			continue
		}
		if backupRun == "" {
			backupRun, err = createBackupRun()
			if err != nil {
				fmt.Fprintf(stderr, "%s: backup: %v\n", harness, err)
				failed = true
				continue
			}
		}
		backupPath := filepath.Join(backupRun, filepath.Base(path))
		if err = writeBackup(backupPath, original); err != nil {
			fmt.Fprintf(stderr, "%s: backup %s: %v\n", harness, backupPath, err)
			failed = true
			continue
		}
		if err = writeFileAtomic(path, updated); err != nil {
			_ = os.Remove(backupPath)
			fmt.Fprintf(stderr, "%s: %s: %v\n", harness, path, err)
			failed = true
			continue
		}
		printWrapChanges(stdout, changes)
		if undo {
			unwrapped += len(changes)
		} else {
			wrapped += len(changes)
		}
	}
	if backupRun != "" {
		if err := retainBackups(filepath.Dir(backupRun)); err != nil {
			fmt.Fprintf(stderr, "%s: backup retention: %v\n", name, err)
			failed = true
		}
	}
	fmt.Fprintf(stdout, "summary: %d wrapped, %d unwrapped", wrapped, unwrapped)
	if *dryRun {
		fmt.Fprint(stdout, " (dry run)")
	}
	fmt.Fprintln(stdout)
	if failed {
		return 1
	}
	return 0
}

func proxyBinary() string {
	executable, err := os.Executable()
	if err != nil {
		return "mcp-snooze"
	}
	if found, err := exec.LookPath("mcp-snooze"); err == nil {
		if a, errA := os.Stat(found); errA == nil {
			if b, errB := os.Stat(executable); errB == nil && os.SameFile(a, b) {
				return found
			}
		}
	}
	return executable
}

func printWrapChanges(out io.Writer, changes []wrapChange) {
	for _, change := range changes {
		fmt.Fprintf(out, "%s %s %s %s\n", change.server.Harness, change.server.Scope, change.server.Name, change.verb)
	}
}

func createBackupRun() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(home, ".mcp-snooze", "backups")
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	if err := os.Chmod(root, 0700); err != nil {
		return "", err
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	for suffix := 0; ; suffix++ {
		name := stamp
		if suffix > 0 {
			name = fmt.Sprintf("%s-%03d", stamp, suffix)
		}
		dir := filepath.Join(root, name)
		err := os.Mkdir(dir, 0700)
		if err == nil {
			return dir, nil
		}
		if !os.IsExist(err) {
			return "", err
		}
	}
}

func writeBackup(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}

func retainBackups(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	dirs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, entry.Name())
		}
	}
	sort.Strings(dirs)
	for len(dirs) > 10 {
		if err := os.RemoveAll(filepath.Join(root, dirs[0])); err != nil {
			return err
		}
		dirs = dirs[1:]
	}
	return nil
}
