package harness

// ClaudeServers lists stdio servers in a Claude Code .claude.json: top-level mcpServers (scope "user")
// and projects[<path>].mcpServers (scope = path). A server is stdio when it has a command and its type
// is absent or "stdio".
func ClaudeServers(src []byte) ([]Server, error) { return nil, errTODO }

// ClaudeSetArgv returns src re-encoded (2-space indent, numbers preserved via json.Number) with the
// named server's command = argv[0] and args = argv[1:] (args removed when len(argv) == 1).
// Every other field, including env, is untouched. Returns ErrNotFound for unknown scope/name.
func ClaudeSetArgv(src []byte, scope, name string, argv []string) ([]byte, error) {
	return nil, errTODO
}
