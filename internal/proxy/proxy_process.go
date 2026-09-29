package proxy

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

type childProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	write  sync.Mutex
	done   chan struct{}
	exitMu sync.Mutex
	exit   error
}

func (c *childProcess) send(line string) bool {
	c.write.Lock()
	defer c.write.Unlock()
	select {
	case <-c.done:
		return false
	default:
	}
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	_, err := io.WriteString(c.stdin, line)
	return err == nil
}

func (c *childProcess) exited() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *childProcess) exitCode() string {
	c.exitMu.Lock()
	defer c.exitMu.Unlock()
	return exitCode(c.exit)
}

func (p *proxy) ensure() (*childProcess, json.RawMessage, error) {
	p.mu.Lock()
	if p.stopping {
		p.mu.Unlock()
		return nil, nil, fmt.Errorf("proxy stopped")
	}
	if p.child != nil && !p.child.exited() && p.initReady {
		proc, result := p.child, append(json.RawMessage(nil), p.initResult...)
		p.mu.Unlock()
		return proc, result, nil
	}
	if !p.initSeen {
		p.mu.Unlock()
		return nil, nil, fmt.Errorf("initialize must be called before other requests")
	}
	attempt := p.starting
	if attempt == nil {
		attempt = &startAttempt{done: make(chan struct{})}
		p.starting = attempt
		go p.start(attempt)
	}
	p.mu.Unlock()
	<-attempt.done
	if attempt.err != nil {
		return nil, nil, attempt.err
	}
	return attempt.proc, append(json.RawMessage(nil), attempt.result...), nil
}

func (p *proxy) start(attempt *startAttempt) {
	proc, err := startProcess(p.command, p.stderr)
	if err != nil {
		p.finishStart(attempt, nil, nil, fmt.Errorf("could not start MCP server: %w", err))
		return
	}
	p.mu.Lock()
	if p.stopping {
		p.mu.Unlock()
		terminateProcess(proc)
		p.finishStart(attempt, nil, nil, fmt.Errorf("proxy stopped"))
		return
	}
	if p.child != nil {
		old := p.child
		p.mu.Unlock()
		terminateProcess(old)
		p.mu.Lock()
	}
	p.child = proc
	p.initReady = false
	p.initResult = nil
	p.servers = make(map[string]*childProcess)
	p.mu.Unlock()
	go p.readChild(proc)

	p.mu.Lock()
	p.serial++
	requestID := fmt.Sprintf("snooze-%d", p.serial)
	entry := &internalRequest{proc: proc, done: make(chan struct{})}
	p.internal[requestID] = entry
	params := append(json.RawMessage(nil), p.initParams...)
	p.mu.Unlock()
	request, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": requestID, "method": "initialize", "params": params})
	if !proc.send(string(request)) {
		p.mu.Lock()
		delete(p.internal, requestID)
		p.mu.Unlock()
		p.discard(proc)
		p.finishStart(attempt, nil, nil, fmt.Errorf("MCP server exited before responding"))
		return
	}
	response, err := p.waitInternal(proc, requestID, entry, "initialize")
	if err != nil {
		p.finishStart(attempt, nil, nil, err)
		return
	}
	result, ok := objectField(response, "result")
	if !ok {
		detail := "invalid response"
		if rpcError, found := objectField(response, "error"); found {
			if message, found := objectField(rpcError, "message"); found {
				detail = rawString(message)
				if detail == "" {
					detail = string(message)
				}
			} else {
				detail = string(rpcError)
			}
		}
		p.discard(proc)
		p.finishStart(attempt, nil, nil, fmt.Errorf("MCP initialize failed: %s", detail))
		return
	}
	if !validResultObject(result) {
		p.discard(proc)
		p.finishStart(attempt, nil, nil, fmt.Errorf("MCP initialize returned a non-object result"))
		return
	}

	p.mu.Lock()
	if p.child != proc || p.stopping {
		p.mu.Unlock()
		p.finishStart(attempt, nil, nil, fmt.Errorf("MCP server exited during initialize"))
		return
	}
	p.initResult = append(json.RawMessage(nil), result...)
	p.initReady = true
	p.mu.Unlock()
	if !proc.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`) {
		p.discard(proc)
		p.finishStart(attempt, nil, nil, fmt.Errorf("MCP server exited during initialize"))
		return
	}
	if err := p.refresh(proc, result); err != nil {
		p.discard(proc)
		p.finishStart(attempt, nil, nil, err)
		return
	}
	p.mu.Lock()
	p.lastClient = time.Now()
	p.mu.Unlock()
	p.finishStart(attempt, proc, result, nil)
}

func (p *proxy) finishStart(attempt *startAttempt, proc *childProcess, result json.RawMessage, err error) {
	p.mu.Lock()
	attempt.proc, attempt.result, attempt.err = proc, append(json.RawMessage(nil), result...), err
	if p.starting == attempt {
		p.starting = nil
	}
	close(attempt.done)
	p.mu.Unlock()
}

func (p *proxy) waitInternal(proc *childProcess, requestID string, entry *internalRequest, method string) (json.RawMessage, error) {
	timer := time.NewTimer(p.startTimeout)
	defer timer.Stop()
	select {
	case <-entry.done:
		if len(entry.response) == 0 {
			return nil, fmt.Errorf("MCP server exited before responding")
		}
		return entry.response, nil
	case <-timer.C:
		p.mu.Lock()
		if p.internal[requestID] == entry {
			delete(p.internal, requestID)
		}
		p.mu.Unlock()
		p.discard(proc)
		return nil, fmt.Errorf("MCP server %s timed out after %gs", method, p.startTimeout.Seconds())
	case <-p.stopCh:
		return nil, fmt.Errorf("proxy stopped")
	}
}

func (p *proxy) discard(proc *childProcess) {
	p.mu.Lock()
	if p.child != proc {
		p.mu.Unlock()
		return
	}
	p.child = nil
	p.initReady = false
	failed := make([]json.RawMessage, 0)
	for key, request := range p.pending {
		if request.proc == proc {
			failed = append(failed, request.id)
			delete(p.pending, key)
		}
	}
	for key, server := range p.servers {
		if server == proc {
			delete(p.servers, key)
		}
	}
	entries := p.takeInternalLocked(proc)
	p.mu.Unlock()
	for _, entry := range entries {
		entry.response = nil
		close(entry.done)
	}
	terminateProcess(proc)
	for _, id := range failed {
		p.writeJSON(errorResponse(id, "MCP server exited with code "+proc.exitCode()))
	}
}

func (p *proxy) childExited(proc *childProcess, exit error) {
	proc.exitMu.Lock()
	proc.exit = exit
	proc.exitMu.Unlock()
	p.mu.Lock()
	if p.child != proc {
		p.mu.Unlock()
		return
	}
	p.child = nil
	p.initReady = false
	failed := make([]json.RawMessage, 0)
	for key, request := range p.pending {
		if request.proc == proc {
			failed = append(failed, request.id)
			delete(p.pending, key)
		}
	}
	for key, server := range p.servers {
		if server == proc {
			delete(p.servers, key)
		}
	}
	entries := p.takeInternalLocked(proc)
	p.mu.Unlock()
	for _, entry := range entries {
		entry.response = nil
		close(entry.done)
	}
	for _, id := range failed {
		p.writeJSON(errorResponse(id, "MCP server exited with code "+exitCode(exit)))
	}
}

func (p *proxy) takeInternalLocked(proc *childProcess) []*internalRequest {
	entries := make([]*internalRequest, 0)
	for key, entry := range p.internal {
		if entry.proc == proc {
			entries = append(entries, entry)
			delete(p.internal, key)
		}
	}
	return entries
}

func exitCode(err error) string {
	if err == nil {
		return "0"
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return fmt.Sprint(exit.ExitCode())
	}
	return err.Error()
}

func (p *proxy) readChild(proc *childProcess) {
	reader := bufio.NewReader(proc.stdout)
	for {
		line, err := reader.ReadString('\n')
		if len(line) != 0 {
			p.handleChildLine(proc, strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"))
		}
		if err != nil {
			if err != io.EOF {
				fmt.Fprintf(p.stderr, "mcp-snooze: child reader failed: %v\n", err)
			}
			break
		}
	}
	waitErr := proc.cmd.Wait()
	proc.exitMu.Lock()
	proc.exit = waitErr
	proc.exitMu.Unlock()
	p.childExited(proc, waitErr)
	close(proc.done)
}

func (p *proxy) handleChildLine(proc *childProcess, line string) {
	var message map[string]json.RawMessage
	if json.Unmarshal([]byte(line), &message) != nil || message == nil {
		fmt.Fprintln(p.stderr, "mcp-snooze: ignored invalid child stdout line")
		return
	}
	id, hasID := message["id"]
	method := rawString(message["method"])
	if hasID && method == "" {
		internalID := rawString(id)
		if strings.HasPrefix(internalID, "snooze-") {
			p.mu.Lock()
			entry := p.internal[internalID]
			if entry != nil && entry.proc == proc {
				delete(p.internal, internalID)
				entry.response = append(json.RawMessage(nil), []byte(line)...)
				close(entry.done)
			}
			p.mu.Unlock()
			return
		}
		token := idKey(id)
		p.mu.Lock()
		request, ok := p.pending[token]
		if ok && request.proc == proc {
			delete(p.pending, token)
			p.lastClient = time.Now()
		}
		active := p.child == proc
		p.mu.Unlock()
		if active {
			p.writeLine(line)
		}
		return
	}

	p.mu.Lock()
	active := p.child == proc && !p.stopping
	if active && method != "" && hasID {
		p.servers[idKey(id)] = proc
	}
	p.mu.Unlock()
	if !active {
		return
	}
	p.writeLine(line)
	if method == "notifications/tools/list_changed" {
		go p.refreshTools(proc)
	}
}

func (p *proxy) refresh(proc *childProcess, initialize json.RawMessage) error {
	lists := make(map[string]json.RawMessage)
	complete := true
	for _, method := range listMethods(initialize) {
		response, err := p.rpc(proc, method)
		if err != nil {
			return err
		}
		result, ok := objectField(response, "result")
		if !ok {
			complete = false
			continue
		}
		lists[method] = responseWithResult(result)
	}
	if !complete {
		return nil
	}
	fresh := &cacheFile{Initialize: append(json.RawMessage(nil), initialize...), Lists: lists}
	p.mu.Lock()
	old, path := p.cache, p.cachePath
	p.cache = fresh
	p.mu.Unlock()
	p.writeCache(path, fresh)
	if old == nil {
		return nil
	}
	notices := make(map[string]bool)
	for _, method := range unionKeys(old.Lists, fresh.Lists) {
		if jsonEqual(old.Lists[method], fresh.Lists[method]) {
			continue
		}
		switch {
		case method == "tools/list":
			notices["notifications/tools/list_changed"] = true
		case strings.HasPrefix(method, "resources/"):
			notices["notifications/resources/list_changed"] = true
		case method == "prompts/list":
			notices["notifications/prompts/list_changed"] = true
		}
	}
	notifications := make([]string, 0, len(notices))
	for method := range notices {
		notifications = append(notifications, method)
	}
	sort.Strings(notifications)
	for _, method := range notifications {
		p.writeJSON(map[string]string{"jsonrpc": "2.0", "method": method})
	}
	return nil
}

func (p *proxy) refreshTools(proc *childProcess) {
	response, err := p.rpc(proc, "tools/list")
	if err != nil {
		if !p.isStopping() {
			fmt.Fprintf(p.stderr, "mcp-snooze: tools/list refresh failed: %v\n", err)
		}
		return
	}
	result, ok := objectField(response, "result")
	if !ok {
		return
	}
	p.mu.Lock()
	if p.child != proc || p.cache == nil {
		p.mu.Unlock()
		return
	}
	fresh := &cacheFile{Initialize: append(json.RawMessage(nil), p.cache.Initialize...), Lists: make(map[string]json.RawMessage, len(p.cache.Lists))}
	for method, value := range p.cache.Lists {
		fresh.Lists[method] = append(json.RawMessage(nil), value...)
	}
	fresh.Lists["tools/list"] = responseWithResult(result)
	p.cache = fresh
	path := p.cachePath
	p.mu.Unlock()
	p.writeCache(path, fresh)
}

func (p *proxy) rpc(proc *childProcess, method string) (json.RawMessage, error) {
	p.mu.Lock()
	if p.child != proc || p.stopping {
		p.mu.Unlock()
		return nil, fmt.Errorf("MCP server stopped")
	}
	p.serial++
	id := fmt.Sprintf("snooze-%d", p.serial)
	entry := &internalRequest{proc: proc, done: make(chan struct{})}
	p.internal[id] = entry
	p.mu.Unlock()
	request, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method})
	if !proc.send(string(request)) {
		p.mu.Lock()
		delete(p.internal, id)
		p.mu.Unlock()
		return nil, fmt.Errorf("MCP server exited before responding")
	}
	return p.waitInternal(proc, id, entry, method)
}

func (p *proxy) isStopping() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopping
}

func (p *proxy) writeCache(path string, value *cacheFile) {
	p.cacheWrite.Lock()
	defer p.cacheWrite.Unlock()
	if err := os.MkdirAll(p.cacheDir, 0700); err != nil {
		fmt.Fprintf(p.stderr, "mcp-snooze: cache write failed: %v\n", err)
		return
	}
	data, err := json.Marshal(value)
	if err != nil {
		fmt.Fprintf(p.stderr, "mcp-snooze: cache write failed: %v\n", err)
		return
	}
	file, err := os.CreateTemp(p.cacheDir, ".mcp-snooze-*.tmp")
	if err != nil {
		fmt.Fprintf(p.stderr, "mcp-snooze: cache write failed: %v\n", err)
		return
	}
	temp := file.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(temp)
		}
	}()
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temp, path)
	}
	if err != nil {
		fmt.Fprintf(p.stderr, "mcp-snooze: cache write failed: %v\n", err)
		return
	}
	ok = true
}

type cacheFile struct {
	Initialize json.RawMessage            `json:"initialize"`
	Lists      map[string]json.RawMessage `json:"lists"`
}

func cacheKey(command []string, cwd, version string) string {
	parts := append(append([]string(nil), command...), cwd, version)
	hash := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(hash[:])
}

func readCache(path string, maxAge time.Duration) *cacheFile {
	stat, err := os.Stat(path)
	if err != nil || time.Since(stat.ModTime()) > maxAge {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var value cacheFile
	if json.Unmarshal(data, &value) != nil || !validCache(&value) {
		return nil
	}
	return &value
}

func validCache(value *cacheFile) bool {
	if value == nil || !validResultObject(value.Initialize) || value.Lists == nil {
		return false
	}
	for _, method := range listMethods(value.Initialize) {
		response, ok := value.Lists[method]
		if !ok {
			return false
		}
		if _, ok := objectField(response, "result"); !ok {
			return false
		}
		if _, hasError := objectField(response, "error"); hasError {
			return false
		}
	}
	return true
}

func validResultObject(raw json.RawMessage) bool {
	var value map[string]json.RawMessage
	return json.Unmarshal(raw, &value) == nil && value != nil
}

func listMethods(initialize json.RawMessage) []string {
	var result map[string]json.RawMessage
	_ = json.Unmarshal(initialize, &result)
	var capabilities map[string]json.RawMessage
	_ = json.Unmarshal(result["capabilities"], &capabilities)
	methods := []string{"tools/list"}
	if _, ok := capabilities["resources"]; ok {
		methods = append(methods, "resources/list", "resources/templates/list")
	}
	if _, ok := capabilities["prompts"]; ok {
		methods = append(methods, "prompts/list")
	}
	return methods
}

func responseWithResult(result json.RawMessage) json.RawMessage {
	data, _ := json.Marshal(map[string]json.RawMessage{"result": result})
	return data
}

func jsonEqual(left, right json.RawMessage) bool {
	var leftValue, rightValue any
	leftDecoder := json.NewDecoder(bytes.NewReader(left))
	leftDecoder.UseNumber()
	rightDecoder := json.NewDecoder(bytes.NewReader(right))
	rightDecoder.UseNumber()
	return leftDecoder.Decode(&leftValue) == nil && rightDecoder.Decode(&rightValue) == nil && reflect.DeepEqual(leftValue, rightValue)
}

func unionKeys(left, right map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(left)+len(right))
	seen := make(map[string]bool, len(left)+len(right))
	for key := range left {
		seen[key] = true
		keys = append(keys, key)
	}
	for key := range right {
		if !seen[key] {
			keys = append(keys, key)
		}
	}
	return keys
}
