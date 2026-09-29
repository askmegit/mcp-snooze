package harness

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
)

type scanRow struct {
	Harness   string   `json:"harness"`
	Scope     string   `json:"scope"`
	Name      string   `json:"name"`
	Command   string   `json:"command"`
	Args      []string `json:"args"`
	Wrapped   bool     `json:"wrapped"`
	Enabled   bool     `json:"enabled"`
	Processes int      `json:"processes"`
	RSSMB     scanRSS  `json:"rss_mb"`
}
type scanRSS float64

func (r scanRSS) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatFloat(float64(r), 'f', 1, 64)), nil
}

type processStats [2]float64 // count, RSS MiB

func runScan(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("scan", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "scan: unexpected positional arguments")
		return 2
	}
	paths := configPaths()
	var servers []Server
	loaded, failed := false, false
	for _, name := range []string{"claude", "codex"} {
		data, err := os.ReadFile(paths[name])
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			fmt.Fprintf(stderr, "%s: %s: %v\n", name, paths[name], err)
			failed = true
			continue
		}
		parsed, err := loadServers(name, data)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %s: %v\n", name, paths[name], err)
			failed = true
			continue
		}
		loaded = true
		servers = append(servers, parsed...)
	}
	sort.Slice(servers, func(i, j int) bool {
		a, b := servers[i], servers[j]
		if a.Harness != b.Harness {
			return a.Harness < b.Harness
		}
		if a.Scope != b.Scope {
			return a.Scope < b.Scope
		}
		return a.Name < b.Name
	})
	stats := make([]processStats, len(servers))
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		if output, err := exec.Command("ps", "-Ao", "pid=,rss=,args=").Output(); err == nil {
			stats = parseProcessOutput(string(output), servers)
		}
	}
	rows := make([]scanRow, len(servers))
	for i, s := range servers {
		maskedArgv := maskSecretArgs(s.Argv)
		rows[i] = scanRow{s.Harness, s.Scope, s.Name, maskedArgv[0], append([]string{}, maskedArgv[1:]...), s.Wrapped, s.Enabled, int(stats[i][0]), scanRSS(stats[i][1])}
	}
	if *jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			fmt.Fprintf(stderr, "scan: %v\n", err)
			return 1
		}
	} else {
		printScanTable(stdout, rows)
	}
	if !loaded && failed {
		return 1
	}
	return 0
}

// interpreters may appear in front of the configured command in ps output
// (a `#!/usr/bin/env node` script shows up as `node /path/to/script ...`).
var interpreters = map[string]bool{"node": true, "python": true, "python3": true, "Python": true,
	"java": true, "bun": true, "deno": true, "ruby": true, "sh": true, "bash": true, "zsh": true}

// argvMatches reports whether the configured argv starts the process argv, either at its first
// token or right after an interpreter; the command is compared by basename, extra trailing
// process args are allowed.
func argvMatches(proc, argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	for start := 0; start <= 1; start++ {
		if start == 1 && !interpreters[filepath.Base(proc[0])] {
			break
		}
		if len(proc)-start < len(argv) || filepath.Base(proc[start]) != filepath.Base(argv[0]) {
			continue
		}
		matched := true
		for j := 1; j < len(argv); j++ {
			if proc[start+j] != argv[j] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// Best-effort prefix argv match on ps output. ps drops quoting (args with spaces never match) and
// launchers (npx, uvx, JVM wrapper scripts) run their child under another argv, so those count 0; upgrade path is a
// per-server pid file written by the proxy.
func parseProcessOutput(output string, servers []Server) []processStats {
	stats := make([]processStats, len(servers))
	seenPIDs := make(map[int]struct{})
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		rss, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		args := fields[2:]
		if executable := filepath.Base(args[0]); executable == "mcp-snooze" || executable == "mcp-snooze.exe" {
			continue
		}
		for i, server := range servers {
			if !argvMatches(args, server.Argv) {
				continue
			}
			if _, duplicate := seenPIDs[pid]; duplicate {
				break
			}
			seenPIDs[pid] = struct{}{}
			stats[i][0]++
			stats[i][1] += rss / 1024
			break
		}
	}
	return stats
}

func maskSecretArgs(args []string) []string {
	masked := append([]string(nil), args...)
	for i, arg := range masked {
		if i > 0 && isSecretFlag(masked[i-1]) {
			masked[i] = "***"
			continue
		}
		if equals := strings.IndexByte(arg, '='); equals >= 0 && containsSecretName(arg[:equals]) {
			masked[i] = arg[:equals+1] + "***"
		}
	}
	return masked
}

func isSecretFlag(arg string) bool {
	return strings.HasPrefix(arg, "-") && !strings.Contains(arg, "=") && containsSecretName(arg)
}

func containsSecretName(name string) bool {
	name = strings.ToLower(name)
	return strings.Contains(name, "key") || strings.Contains(name, "token") || strings.Contains(name, "secret") || strings.Contains(name, "password")
}

func printScanTable(out io.Writer, rows []scanRow) {
	if len(rows) == 0 {
		fmt.Fprintln(out, "No stdio MCP servers found in Claude Code or Codex configs.")
		return
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "HARNESS\tSCOPE\tNAME\tSNOOZED\tPROCS\tRSS\tCOMMAND")
	notSnoozed, processes, rss := 0, 0, 0.0
	for _, r := range rows {
		snoozed := "no"
		if r.Wrapped {
			snoozed = "yes"
		}
		cmd := truncateCommand(strings.Join(append([]string{r.Command}, r.Args...), " "))
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%.1f\t%s\n", r.Harness, r.Scope, r.Name, snoozed, r.Processes, r.RSSMB, cmd)
		if r.Enabled && !r.Wrapped {
			notSnoozed++
			processes += r.Processes
			rss += float64(r.RSSMB)
		}
	}
	_ = w.Flush()
	fmt.Fprintln(out)
	if notSnoozed > 0 {
		fmt.Fprintf(out, "%d stdio server(s) not snoozed, holding %.1f MB across %d process(es). Run: mcp-snooze wrap\n", notSnoozed, rss, processes)
	} else {
		fmt.Fprintln(out, "All stdio servers are snoozed.")
	}
}

func truncateCommand(s string) string {
	r := []rune(s)
	if len(r) <= 60 {
		return s
	}
	return string(r[:57]) + "..."
}
