package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
)

// ClaudeServers lists stdio servers in a Claude Code .claude.json: top-level mcpServers (scope "user")
// and projects[<path>].mcpServers (scope = path). A server is stdio when it has a command and its type
// is absent or "stdio".
func ClaudeServers(src []byte) ([]Server, error) {
	root, err := decodeClaude(src)
	if err != nil {
		return nil, err
	}
	var servers []Server
	add := func(scope string, entries map[string]any) {
		for name, value := range entries {
			fields, ok := value.(map[string]any)
			if !ok {
				continue
			}
			command, ok := claudeCommand(fields)
			if !ok {
				continue
			}
			full := []string{command}
			if args, ok := fields["args"].([]any); ok {
				for _, value := range args {
					if arg, ok := value.(string); ok {
						full = append(full, arg)
					}
				}
			}
			wrapped, flags, argv := SplitWrapped(full)
			servers = append(servers, Server{Harness: "claude", Scope: scope, Name: name, Argv: argv, Wrapped: wrapped, ProxyFlags: flags, Enabled: true})
		}
	}
	if entries, ok := root["mcpServers"].(map[string]any); ok {
		add("user", entries)
	}
	if projects, ok := root["projects"].(map[string]any); ok {
		for scope, value := range projects {
			project, ok := value.(map[string]any)
			if !ok {
				continue
			}
			if entries, ok := project["mcpServers"].(map[string]any); ok {
				add(scope, entries)
			}
		}
	}
	sort.Slice(servers, func(i, j int) bool {
		if servers[i].Scope == servers[j].Scope {
			return servers[i].Name < servers[j].Name
		}
		return servers[i].Scope < servers[j].Scope
	})
	return servers, nil
}

// ClaudeSetArgv returns src re-encoded (2-space indent, numbers preserved via json.Number) with the
// named server's command = argv[0] and args = argv[1:] (args removed when len(argv) == 1).
// Every other field, including env, is untouched. Returns ErrNotFound for unknown scope/name.
func ClaudeSetArgv(src []byte, scope, name string, argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("argv must contain a command")
	}
	root, err := decodeClaude(src)
	if err != nil {
		return nil, err
	}
	fields, ok := claudeFields(root, scope, name)
	if !ok {
		return nil, ErrNotFound
	}
	if _, ok := claudeCommand(fields); !ok {
		return nil, ErrNotFound
	}
	fields["command"] = argv[0]
	if len(argv) == 1 {
		delete(fields, "args")
	} else {
		args := make([]any, len(argv)-1)
		for i, arg := range argv[1:] {
			args[i] = arg
		}
		fields["args"] = args
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(root); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func decodeClaude(src []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(src))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	return root, nil
}

func claudeCommand(fields map[string]any) (string, bool) {
	command, ok := fields["command"].(string)
	if !ok {
		return "", false
	}
	if kind, exists := fields["type"]; exists && kind != "stdio" {
		return "", false
	}
	return command, true
}

func claudeFields(root map[string]any, scope, name string) (map[string]any, bool) {
	var entries map[string]any
	if scope == "user" {
		entries, _ = root["mcpServers"].(map[string]any)
	} else if projects, ok := root["projects"].(map[string]any); ok {
		if project, ok := projects[scope].(map[string]any); ok {
			entries, _ = project["mcpServers"].(map[string]any)
		}
	}
	if fields, ok := entries[name].(map[string]any); ok {
		return fields, true
	}
	return nil, false
}
