package harness

import "errors"

var errTODO = errors.New("not implemented")

// CodexServers lists stdio servers declared as [mcp_servers.<name>] tables in a Codex config.toml.
// Entries with url are skipped, as are tables that only look similar (e.g. [other.mcp_servers.x]).
func CodexServers(src []byte) ([]Server, error) { return nil, errTODO }

// CodexSetArgv rewrites the command and args of server name so that argv[0] is the command and
// argv[1:] the args, changing no other bytes of src. Missing args is inserted right after command;
// when len(argv) == 1 an args key is removed. Line endings (LF or CRLF) are preserved.
// Returns ErrNotFound / ErrUnsupported and leaves src untouched on failure.
func CodexSetArgv(src []byte, name string, argv []string) ([]byte, error) { return nil, errTODO }
