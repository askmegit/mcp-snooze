// Package proxy implements the lazy stdio MCP proxy.
package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Main runs the proxy: mcp-snooze [flags] -- <command> [args...].
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp-snooze", flag.ContinueOnError)
	fs.SetOutput(stderr)
	idle := fs.Float64("idle", 600, "seconds before an idle MCP server is reaped")
	cacheDir := fs.String("cache-dir", "", "directory for cached MCP initialization and lists")
	maxAge := fs.Float64("max-age", 604800, "maximum cache age in seconds")
	startTimeout := fs.Float64("start-timeout", 60, "seconds to wait for proxy-owned MCP requests")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: mcp-snooze [--idle SECONDS] [--cache-dir DIR] [--max-age SECONDS] [--start-timeout SECONDS] -- <command> [args...]")
		fs.PrintDefaults()
	}

	separator := -1
	for i, arg := range args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 {
		fs.Usage()
		fmt.Fprintln(stderr, "mcp-snooze: expected -- followed by the MCP server command")
		return 2
	}
	if err := fs.Parse(args[:separator]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	command := args[separator+1:]
	if len(command) == 0 {
		fmt.Fprintln(stderr, "mcp-snooze: expected -- followed by the MCP server command")
		return 2
	}
	if *cacheDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			fmt.Fprintf(stderr, "mcp-snooze: determine user cache directory: %v\n", err)
			return 2
		}
		*cacheDir = filepath.Join(base, "mcp-snooze")
	}

	p := &proxy{
		command:      append([]string(nil), command...),
		idle:         seconds(*idle),
		maxAge:       seconds(*maxAge),
		startTimeout: seconds(*startTimeout),
		cacheDir:     *cacheDir,
		stdin:        stdin,
		stdout:       stdout,
		stderr:       stderr,
		pending:      make(map[string]pendingRequest),
		servers:      make(map[string]*childProcess),
		internal:     make(map[string]*internalRequest),
		stopCh:       make(chan struct{}),
	}
	return p.run()
}

func seconds(value float64) time.Duration {
	return time.Duration(value * float64(time.Second))
}

type proxy struct {
	command      []string
	idle         time.Duration
	maxAge       time.Duration
	startTimeout time.Duration
	cacheDir     string
	stdin        io.Reader
	stdout       io.Writer
	stderr       io.Writer

	mu         sync.Mutex
	writeMu    sync.Mutex
	cacheWrite sync.Mutex
	closeOnce  sync.Once
	child      *childProcess
	starting   *startAttempt
	initSeen   bool
	initReady  bool
	initParams json.RawMessage
	initResult json.RawMessage
	cachePath  string
	cache      *cacheFile
	pending    map[string]pendingRequest
	servers    map[string]*childProcess
	internal   map[string]*internalRequest
	serial     uint64
	lastClient time.Time
	stopping   bool
	stopCh     chan struct{}
}

type pendingRequest struct {
	id   json.RawMessage
	proc *childProcess
}

type internalRequest struct {
	proc     *childProcess
	response json.RawMessage
	done     chan struct{}
}

type startAttempt struct {
	done   chan struct{}
	proc   *childProcess
	result json.RawMessage
	err    error
}

func (p *proxy) run() int {
	go p.idleLoop()
	lines := make(chan inputLine)
	go readInput(p.stdin, lines)
	signals := make(chan os.Signal, 1)
	stopSignals := notifySignals(signals)
	defer stopSignals()
	for {
		select {
		case item := <-lines:
			if item.err != nil && item.err != io.EOF {
				fmt.Fprintf(p.stderr, "mcp-snooze: read client input: %v\n", item.err)
				p.close()
				return 1
			}
			if item.line != "" {
				p.handle(item.line)
			}
			if item.err == io.EOF {
				p.close()
				return 0
			}
		case <-signals:
			p.close()
			return 0
		}
	}
}

type inputLine struct {
	line string
	err  error
}

func readInput(r io.Reader, lines chan<- inputLine) {
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			lines <- inputLine{line: line}
		}
		if err != nil {
			lines <- inputLine{err: err}
			return
		}
	}
}

func (p *proxy) writeJSON(value any) {
	data, err := json.Marshal(value)
	if err != nil {
		fmt.Fprintf(p.stderr, "mcp-snooze: encode JSON-RPC message: %v\n", err)
		return
	}
	p.writeLine(string(data))
}

func (p *proxy) writeLine(line string) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	if _, err := io.WriteString(p.stdout, line); err != nil {
		fmt.Fprintf(p.stderr, "mcp-snooze: write client output: %v\n", err)
	}
}

func (p *proxy) handle(line string) {
	var message map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &message); err != nil {
		fmt.Fprintf(p.stderr, "mcp-snooze: invalid client message: %v\n", err)
		return
	}
	if message == nil {
		p.writeJSON(map[string]any{
			"jsonrpc": "2.0",
			"id":      nil,
			"error":   map[string]any{"code": -32600, "message": "Invalid Request"},
		})
		return
	}
	method := rawString(message["method"])
	_, hasID := message["id"]

	if method == "initialize" && hasID {
		p.initialize(message)
		return
	}
	if hasID && method != "" {
		p.touchClient()
		go p.request(message, line)
		return
	}
	if !hasID && method == "notifications/initialized" {
		p.touchClient()
		return
	}

	id, hasID := message["id"]
	if method == "" && hasID {
		token := idKey(id)
		p.mu.Lock()
		proc := p.servers[token]
		delete(p.servers, token)
		active := proc != nil && p.child == proc && !p.stopping
		p.mu.Unlock()
		if active {
			p.touchClient()
			if !proc.send(line) {
				fmt.Fprintln(p.stderr, "mcp-snooze: could not forward client response")
			}
		}
		return
	}

	p.mu.Lock()
	if method == "notifications/cancelled" {
		var params map[string]json.RawMessage
		_ = json.Unmarshal(message["params"], &params)
		if requestID, ok := params["requestId"]; ok {
			delete(p.pending, idKey(requestID))
		}
	}
	proc := p.child
	ready := proc != nil && !p.stopping && p.initReady && !proc.exited()
	p.mu.Unlock()
	p.touchClient()
	if ready && !proc.send(line) {
		fmt.Fprintln(p.stderr, "mcp-snooze: could not forward client message")
	}
}

func (p *proxy) initialize(message map[string]json.RawMessage) {
	params, ok := message["params"]
	if !ok {
		params = json.RawMessage(`{}`)
	}
	cwd, err := os.Getwd()
	if err != nil {
		p.writeJSON(errorResponse(message["id"], fmt.Sprintf("get working directory: %v", err)))
		return
	}
	path := filepath.Join(p.cacheDir, cacheKey(p.command, cwd, protocolVersion(params))+".json")
	cached := readCache(path, p.maxAge)

	p.mu.Lock()
	if p.stopping {
		p.mu.Unlock()
		p.writeJSON(errorResponse(message["id"], "proxy stopped"))
		return
	}
	p.initSeen = true
	p.initParams = append(json.RawMessage(nil), params...)
	p.cachePath = path
	p.cache = cached
	p.lastClient = time.Now()
	p.mu.Unlock()

	if cached != nil {
		p.writeJSON(resultResponse(message["id"], cached.Initialize))
		return
	}
	id := append(json.RawMessage(nil), message["id"]...)
	go func() {
		_, result, err := p.ensure()
		if err != nil {
			p.writeJSON(errorResponse(id, err.Error()))
			return
		}
		p.writeJSON(resultResponse(id, result))
	}()
}

func (p *proxy) request(message map[string]json.RawMessage, line string) {
	id := message["id"]
	method := rawString(message["method"])
	if method == "ping" {
		p.writeJSON(resultResponse(id, json.RawMessage(`{}`)))
		return
	}

	p.mu.Lock()
	proc, cached, seen := p.child, p.cache, p.initSeen
	p.mu.Unlock()
	if !seen {
		p.writeJSON(errorResponse(id, "initialize must be called before other requests"))
		return
	}
	if (proc == nil || proc.exited()) && cached != nil && !hasCursor(message["params"]) {
		if response, ok := cached.Lists[method]; ok {
			if result, exists := objectField(response, "result"); exists {
				p.writeJSON(resultResponse(id, result))
				return
			}
		}
	}

	proc, _, err := p.ensure()
	if err != nil {
		p.writeJSON(errorResponse(id, err.Error()))
		return
	}
	token := idKey(id)
	p.mu.Lock()
	if p.child != proc || p.stopping {
		p.mu.Unlock()
		p.writeJSON(errorResponse(id, "MCP server exited before request forwarding"))
		return
	}
	p.pending[token] = pendingRequest{id: append(json.RawMessage(nil), id...), proc: proc}
	p.mu.Unlock()
	if !proc.send(line) {
		p.mu.Lock()
		_, owned := p.pending[token]
		delete(p.pending, token)
		p.mu.Unlock()
		if owned {
			p.writeJSON(errorResponse(id, "MCP server exited before request forwarding"))
		}
	}
}

func (p *proxy) touchClient() {
	p.mu.Lock()
	p.lastClient = time.Now()
	p.mu.Unlock()
}

func (p *proxy) idleLoop() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.reapIdle()
		}
	}
}

func (p *proxy) reapIdle() {
	p.mu.Lock()
	proc := p.child
	due := proc != nil && p.initReady && len(p.pending) == 0 && len(p.internal) == 0 && len(p.servers) == 0 && time.Since(p.lastClient) >= p.idle
	if due {
		p.child = nil
		p.initReady = false
		p.servers = make(map[string]*childProcess)
	}
	p.mu.Unlock()
	if due {
		terminateProcess(proc)
	}
}

func (p *proxy) close() {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.stopping = true
		close(p.stopCh)
		attempt := p.starting
		p.mu.Unlock()
		if attempt != nil {
			<-attempt.done
		}
		p.mu.Lock()
		proc := p.child
		p.child = nil
		p.initReady = false
		p.servers = make(map[string]*childProcess)
		for id, entry := range p.internal {
			delete(p.internal, id)
			entry.response = nil
			close(entry.done)
		}
		p.mu.Unlock()
		if proc != nil {
			terminateProcess(proc)
		}
	})
}

func protocolVersion(params json.RawMessage) string {
	var values map[string]json.RawMessage
	_ = json.Unmarshal(params, &values)
	raw, ok := values["protocolVersion"]
	if !ok {
		return "None"
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	if bytes.Equal(raw, []byte("true")) {
		return "True"
	}
	if bytes.Equal(raw, []byte("false")) {
		return "False"
	}
	if bytes.Equal(raw, []byte("null")) {
		return "None"
	}
	return string(raw)
}

func rawString(raw json.RawMessage) string {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

func idKey(id json.RawMessage) string {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(id))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return string(id)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return string(id)
	}
	return string(canonical)
}

func hasCursor(params json.RawMessage) bool {
	var values map[string]json.RawMessage
	_ = json.Unmarshal(params, &values)
	_, exists := values["cursor"]
	return exists
}

func objectField(raw json.RawMessage, name string) (json.RawMessage, bool) {
	var values map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil || values == nil {
		return nil, false
	}
	value, ok := values[name]
	return value, ok
}

func resultResponse(id, result json.RawMessage) map[string]json.RawMessage {
	return map[string]json.RawMessage{"jsonrpc": json.RawMessage(`"2.0"`), "id": id, "result": result}
}

func errorResponse(id json.RawMessage, message string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": -32603, "message": message},
	}
}
