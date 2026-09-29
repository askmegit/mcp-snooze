package harness

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

func multilineState(s string, quote byte) byte {
	for i := 0; i < len(s); {
		if quote != 0 {
			if quote == '"' && s[i] == '\\' {
				i++
				if i < len(s) {
					i++
				}
				continue
			}
			if i+2 < len(s) && s[i] == quote && s[i+1] == quote && s[i+2] == quote {
				quote = 0
				i += 3
				continue
			}
			i++
			continue
		}
		if s[i] == '#' {
			break
		}
		if s[i] != '"' && s[i] != '\'' {
			i++
			continue
		}
		q := s[i]
		if i+2 < len(s) && s[i+1] == q && s[i+2] == q {
			quote = q
			i += 3
			continue
		}
		i++
		for i < len(s) && s[i] != q && s[i] != '\n' && s[i] != '\r' {
			if q == '"' && s[i] == '\\' {
				i++
			}
			i++
		}
		if i < len(s) && s[i] == q {
			i++
		}
	}
	return quote
}

// notRewritable reports a value we cannot replace faithfully: one holding a triple-quoted
// string, or a multi-line array with comments that a one-line rewrite would drop.
func notRewritable(s string) bool {
	for i := 0; i < len(s); {
		if s[i] == '#' {
			return true
		}
		if s[i] != '"' && s[i] != '\'' {
			i++
			continue
		}
		q := s[i]
		if i+2 < len(s) && s[i+1] == q && s[i+2] == q {
			return true
		}
		i++
		for i < len(s) && s[i] != q && s[i] != '\n' && s[i] != '\r' {
			if q == '"' && s[i] == '\\' {
				i++
			}
			i++
		}
		if i < len(s) && s[i] == q {
			i++
		}
	}
	return false
}

type field [4]int
type edit struct {
	start, end int
	text       string
}

// CodexServers lists stdio servers declared in the mcp_servers table of a Codex config.toml.
// Entries without a string command or with non-string args are skipped.
func CodexServers(src []byte) ([]Server, error) {
	var config map[string]any
	meta, err := toml.Decode(string(src), &config)
	if err != nil {
		return nil, err
	}
	out := make([]Server, 0)
	tables, ok := config["mcp_servers"].(map[string]any)
	if !ok {
		return out, nil
	}
	var names []string
	seen := make(map[string]struct{}, len(tables))
	for _, key := range meta.Keys() {
		if len(key) < 2 || key[0] != "mcp_servers" {
			continue
		}
		name := key[1]
		if _, ok := seen[name]; !ok {
			names = append(names, name)
			seen[name] = struct{}{}
		}
	}
	for _, name := range names {
		tab, ok := tables[name].(map[string]any)
		if !ok {
			continue
		}
		cmd, ok := tab["command"].(string)
		if !ok {
			continue
		}
		argv := []string{cmd}
		if raw, exists := tab["args"]; exists {
			args, ok := raw.([]any)
			if !ok {
				continue
			}
			valid := true
			for _, arg := range args {
				value, ok := arg.(string)
				if !ok {
					valid = false
					break
				}
				argv = append(argv, value)
			}
			if !valid {
				continue
			}
		}
		wrapped, flags, argv := SplitWrapped(argv)
		enabled := true
		if value, ok := tab["enabled"].(bool); ok && !value {
			enabled = false
		}
		out = append(out, Server{Harness: "codex", Scope: "user", Name: name, Argv: argv, Wrapped: wrapped, ProxyFlags: flags, Enabled: enabled})
	}
	return out, nil
}

// tomlEqual treats NaN as equal because NaN != NaN made every edit fail.
func tomlEqual(a, b any) bool {
	switch a := a.(type) {
	case map[string]any:
		b, ok := b.(map[string]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for key, value := range a {
			other, ok := b[key]
			if !ok || !tomlEqual(value, other) {
				return false
			}
		}
		return true
	case []any:
		b, ok := b.([]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for i := range a {
			if !tomlEqual(a[i], b[i]) {
				return false
			}
		}
		return true
	case []map[string]any:
		b, ok := b.([]map[string]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for i := range a {
			if !tomlEqual(a[i], b[i]) {
				return false
			}
		}
		return true
	case float64:
		b, ok := b.(float64)
		return ok && ((math.IsNaN(a) && math.IsNaN(b)) || (a == b && math.Signbit(a) == math.Signbit(b)))
	default:
		return reflect.DeepEqual(a, b)
	}
}

// CodexSetArgv rewrites command and args while changing no other bytes. Missing args is inserted
// after command; a one-item argv removes args. Line endings (LF or CRLF) are preserved.
// Returns ErrNotFound / ErrUnsupported and leaves src untouched on failure.
func CodexSetArgv(src []byte, name string, argv []string) ([]byte, error) {
	var before map[string]any
	if _, err := toml.Decode(string(src), &before); err != nil {
		return nil, err
	}
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
	if notRewritable(t[c[2]:c[3]]) {
		return nil, ErrUnsupported
	}
	if a, ok := tab["args"]; ok && notRewritable(t[a[2]:a[3]]) {
		return nil, ErrUnsupported
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
	var after map[string]any
	if _, err := toml.Decode(t, &after); err != nil {
		return nil, ErrUnsupported
	}
	servers, ok := before["mcp_servers"].(map[string]any)
	if !ok {
		return nil, ErrUnsupported
	}
	server, ok := servers[name].(map[string]any)
	if !ok {
		return nil, ErrUnsupported
	}
	server["command"] = argv[0]
	if len(argv) == 1 {
		delete(server, "args")
	} else {
		args := make([]any, len(argv)-1)
		for i, arg := range argv[1:] {
			args[i] = arg
		}
		server["args"] = args
	}
	if !tomlEqual(after, before) {
		return nil, ErrUnsupported
	}
	return []byte(t), nil
}

func scan(t string) (map[string]map[string]field, []string, error) {
	tables, names, current := map[string]map[string]field{}, []string{}, ""
	var multiline byte
	for p := 0; p < len(t); {
		end := lineEnd(t, p)
		if multiline != 0 {
			multiline = multilineState(t[p:end], multiline)
			p = end
			continue
		}
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
				tables[current][key] = field{p, next, v, last}
				if key == "args" || last > end {
					end = next
				}
			}
		}
		multiline = multilineState(t[p:end], 0)
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
		if p+2 < len(s) && s[p+1] == s[p] && s[p+2] == s[p] {
			for i := p + 3; i+2 < len(s); i++ {
				if s[p] == '"' && s[i] == '\\' {
					i++
					continue
				}
				if s[i] == s[p] && s[i+1] == s[p] && s[i+2] == s[p] {
					return i + 3, nil
				}
			}
			return 0, errors.New("unterminated TOML string")
		}
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
	depth, q, triple := 0, byte(0), false
	for i := p; i < len(s); i++ {
		c := s[i]
		if q != 0 {
			if triple {
				if q == '"' && c == '\\' {
					i++
					continue
				}
				if c == q && i+2 < len(s) && s[i+1] == q && s[i+2] == q {
					q, triple = 0, false
					i += 2
				}
				continue
			}
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
			triple = i+2 < len(s) && s[i+1] == c && s[i+2] == c
			if triple {
				i += 2
			}
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
