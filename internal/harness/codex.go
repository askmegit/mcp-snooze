package harness

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var errTODO = errors.New("not implemented")

type field [4]int
type edit struct {
	start, end int
	text       string
}

// CodexServers lists stdio servers declared as [mcp_servers.<name>] tables in a Codex config.toml.
// Entries with url are skipped, as are tables that only look similar (e.g. [other.mcp_servers.x]).
func CodexServers(src []byte) ([]Server, error) {
	t := string(src)
	tables, names, err := scan(t)
	if err != nil {
		return nil, err
	}
	out := make([]Server, 0, len(names))
	for _, name := range names {
		tab := tables[name]
		if _, ok := tab["url"]; ok {
			continue
		}
		c, ok := tab["command"]
		if !ok {
			continue
		}
		cmd, err := tomlString(t[c[2]:c[3]])
		if err != nil {
			return nil, fmt.Errorf("Codex server %q command: %w", name, err)
		}
		argv := []string{cmd}
		if a, ok := tab["args"]; ok {
			args, err := tomlArray(t[a[2]:a[3]])
			if err != nil {
				return nil, fmt.Errorf("Codex server %q args: %w", name, err)
			}
			argv = append(argv, args...)
		}
		wrapped, flags, argv := SplitWrapped(argv)
		enabled := true
		if f, ok := tab["enabled"]; ok && strings.TrimSpace(t[f[2]:f[3]]) == "false" {
			enabled = false
		}
		out = append(out, Server{Harness: "codex", Scope: "user", Name: name, Argv: argv, Wrapped: wrapped, ProxyFlags: flags, Enabled: enabled})
	}
	return out, nil
}

// CodexSetArgv rewrites command and args while changing no other bytes. Missing args is inserted
// after command; a one-item argv removes args. Line endings (LF or CRLF) are preserved.
// Returns ErrNotFound / ErrUnsupported and leaves src untouched on failure.
func CodexSetArgv(src []byte, name string, argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("Codex server argv must include a command")
	}
	t := string(src)
	tables, _, err := scan(t)
	if err != nil {
		return nil, err
	}
	tab := tables[name]
	if tab == nil {
		return nil, ErrNotFound
	}
	if _, ok := tab["url"]; ok {
		return nil, ErrNotFound
	}
	c, ok := tab["command"]
	if !ok {
		return nil, ErrNotFound
	}
	edits := []edit{{c[2], c[3], quote(argv[0])}}
	if a, ok := tab["args"]; ok {
		if len(argv) == 1 {
			edits = append(edits, edit{a[0], a[1], ""})
		} else {
			edits = append(edits, edit{a[2], a[3], arrayQuote(argv[1:])})
		}
	} else if len(argv) > 1 {
		ending := "\n"
		if strings.Contains(t, "\r\n") {
			ending = "\r\n"
		}
		line := t[c[0]:c[1]]
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		insert := indent + "args = " + arrayQuote(argv[1:]) + ending
		if c[1] == len(t) && !strings.HasSuffix(t, "\n") {
			insert = ending + strings.TrimSuffix(insert, ending)
		}
		edits = append(edits, edit{c[1], c[1], insert})
	}
	if len(edits) == 2 && edits[0].start < edits[1].start {
		edits[0], edits[1] = edits[1], edits[0]
	}
	for _, e := range edits {
		t = t[:e.start] + e.text + t[e.end:]
	}
	return []byte(t), nil
}

func scan(t string) (map[string]map[string]field, []string, error) {
	tables, names, current := map[string]map[string]field{}, []string{}, ""
	for p := 0; p < len(t); {
		end := lineEnd(t, p)
		line := strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(t[p:end], "\n"), "\r"))
		if strings.HasPrefix(line, "[") {
			current = header(line)
			if current != "" && tables[current] == nil {
				tables[current] = map[string]field{}
				names = append(names, current)
			}
		} else if current != "" && line != "" && !strings.HasPrefix(line, "#") {
			key, v, ok := assignment(t[p:end])
			if ok && (key == "command" || key == "args" || key == "url" || key == "enabled") {
				v += p
				last, err := valueEnd(t, v)
				if err != nil {
					return nil, nil, err
				}
				next := lineEnd(t, last)
				if key == "args" {
					end = next
				}
				tables[current][key] = field{p, next, v, last}
				if key == "args" {
					end = next
				}
			}
		}
		p = end
	}
	return tables, names, nil
}
func lineEnd(t string, p int) int {
	if i := strings.IndexByte(t[p:], '\n'); i >= 0 {
		return p + i + 1
	}
	return len(t)
}
func header(line string) string {
	if strings.HasPrefix(line, "[[") {
		return ""
	}
	q, esc, close := byte(0), false, -1
	for i := 1; i < len(line); i++ {
		c := line[i]
		if q != 0 {
			if q == '"' && c == '\\' && !esc {
				esc = true
				continue
			}
			if c == q && !esc {
				q = 0
			}
			esc = false
		} else if c == '"' || c == '\'' {
			q = c
		} else if c == ']' {
			close = i
			break
		}
	}
	if close < 0 {
		return ""
	}
	rest := strings.TrimSpace(line[close+1:])
	if rest != "" && !strings.HasPrefix(rest, "#") {
		return ""
	}
	key := strings.TrimSpace(line[1:close])
	const prefix = "mcp_servers."
	if !strings.HasPrefix(key, prefix) {
		return ""
	}
	key = strings.TrimSpace(key[len(prefix):])
	if key == "" {
		return ""
	}
	if key[0] == '"' || key[0] == '\'' {
		end, err := quotedEnd(key, 0)
		if err != nil || strings.TrimSpace(key[end:]) != "" {
			return ""
		}
		name, err := tomlString(key[:end])
		if err == nil {
			return name
		}
		return ""
	}
	for _, c := range key {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return ""
		}
	}
	return key
}
func assignment(s string) (string, int, bool) {
	i := strings.IndexByte(s, '=')
	if i < 0 {
		return "", 0, false
	}
	key := strings.TrimSpace(s[:i])
	if key == "" || strings.HasPrefix(strings.TrimSpace(s), "#") {
		return "", 0, false
	}
	v := i + 1
	for v < len(s) && (s[v] == ' ' || s[v] == '\t') {
		v++
	}
	return key, v, true
}
func valueEnd(s string, p int) (int, error) {
	if p >= len(s) {
		return 0, errors.New("missing TOML value")
	}
	if s[p] == '"' || s[p] == '\'' {
		return quotedEnd(s, p)
	}
	if s[p] != '[' {
		end := lineEnd(s, p)
		if i := strings.IndexByte(s[p:end], '#'); i >= 0 {
			end = p + i
		}
		for end > p && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\r') {
			end--
		}
		return end, nil
	}
	depth, q := 0, byte(0)
	for i := p; i < len(s); i++ {
		c := s[i]
		if q != 0 {
			if q == '"' && c == '\\' {
				i++
				continue
			}
			if c == q {
				q = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			q = c
			continue
		}
		if c == '#' {
			if n := strings.IndexByte(s[i:], '\n'); n >= 0 {
				i += n
			} else {
				return 0, errors.New("unterminated TOML array")
			}
			continue
		}
		if c == '[' {
			depth++
		} else if c == ']' {
			depth--
			if depth == 0 {
				return i + 1, nil
			}
		}
	}
	return 0, errors.New("unterminated TOML array")
}
func quotedEnd(s string, p int) (int, error) {
	if p >= len(s) || (s[p] != '"' && s[p] != '\'') {
		return 0, errors.New("expected TOML string")
	}
	q := s[p]
	for i := p + 1; i < len(s); i++ {
		if q == '"' && s[i] == '\\' {
			i++
			continue
		}
		if s[i] == q {
			return i + 1, nil
		}
		if s[i] == '\n' || s[i] == '\r' {
			return 0, errors.New("newline in TOML string")
		}
	}
	return 0, errors.New("unterminated TOML string")
}
func tomlString(s string) (string, error) {
	s = strings.TrimSpace(s)
	end, err := quotedEnd(s, 0)
	if err != nil || end != len(s) {
		return "", errors.New("expected TOML string")
	}
	if s[0] == '\'' {
		return s[1 : len(s)-1], nil
	}
	return strconv.Unquote(s)
}
func tomlArray(s string) ([]string, error) {
	var out []string
	for i := 1; i < len(s)-1; {
		for i < len(s)-1 {
			if strings.ContainsRune(" \t\r\n,", rune(s[i])) {
				i++
			} else if s[i] == '#' {
				if n := strings.IndexByte(s[i:], '\n'); n >= 0 {
					i += n + 1
				} else {
					return nil, errors.New("invalid TOML array")
				}
			} else {
				break
			}
		}
		if i >= len(s)-1 {
			break
		}
		end, err := quotedEnd(s, i)
		if err != nil {
			return nil, err
		}
		v, err := tomlString(s[i:end])
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		i = end
	}
	return out, nil
}
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString("\\n")
		case '\t':
			b.WriteString("\\t")
		case '\r':
			b.WriteString("\\r")
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, "\\u%04X", r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
func arrayQuote(a []string) string {
	v := make([]string, len(a))
	for i, s := range a {
		v[i] = quote(s)
	}
	return "[" + strings.Join(v, ", ") + "]"
}
