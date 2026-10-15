package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type clock struct {
	mu    sync.Mutex
	start time.Time
	last  int64
}

func (c *clock) stamp() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := time.Since(c.start).Nanoseconds()
	if n <= c.last {
		n = c.last + 1
	}
	c.last = n
	return n
}

type Call struct {
	Index       int      `json:"index"`
	Worker      int      `json:"worker"`
	Phase       string   `json:"phase"`
	Route       string   `json:"route"`
	Fault       string   `json:"fault,omitempty"`
	KillDelayUS int64    `json:"kill_delay_us,omitempty"`
	Invoke      int64    `json:"invoke_ns"`
	Submitted   int64    `json:"submitted_ns"`
	Return      int64    `json:"return_ns"`
	Request     Request  `json:"request"`
	Observed    Observed `json:"observed"`
}

type transport struct {
	binary, home, group string
	clock               *clock
}

func cleanEnvironment() []string {
	env := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, "ICARUS_") {
			env = append(env, entry)
		}
	}
	return env
}
func (t transport) cli(req Request, crash string, killAfter time.Duration) (Observed, int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.binary, "--home", t.home, "--group", t.group, "api")
	cmd.Env = cleanEnvironment()
	if crash != "" {
		cmd.Env = append(cmd.Env, "ICARUS_TEST_CRASH="+crash)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Observed{Kind: "transport", Stderr: err.Error()}, 0
	}
	if err = cmd.Start(); err != nil {
		return Observed{Kind: "transport", Stderr: err.Error()}, 0
	}
	_, writeErr := io.WriteString(stdin, canonical(req))
	_ = stdin.Close()
	submitted := t.clock.stamp()
	var timer *time.Timer
	if killAfter >= 0 {
		timer = time.AfterFunc(killAfter, func() { _ = cmd.Process.Kill() })
	}
	err = cmd.Wait()
	if timer != nil {
		timer.Stop()
	}
	code := cmd.ProcessState.ExitCode()
	out := Observed{Kind: "response", Exit: code, Stderr: stderr.String()}
	if json.Unmarshal(stdout.Bytes(), &out.Response) == nil && out.Response.Version == 1 {
		expected := 1
		if out.Response.OK {
			expected = 0
		}
		// A deliberate kill can arrive after a complete protocol reply. That
		// reply is observed, even though the test then terminates the process.
		if code != expected && !(killAfter >= 0 && code == -1) {
			out.Kind = "transport"
			out.Stderr += fmt.Sprintf("; protocol reply expects exit %d, got %d", expected, code)
		}
		return out, submitted
	}
	if writeErr != nil {
		out.Stderr += "; stdin: " + writeErr.Error()
	}
	out.Kind = "transport"
	if crash == "before_commit" && code == 86 {
		out.Kind = "known_abort"
	}
	if (crash == "after_commit" || crash == "after_notify") && code == 86 {
		out.Kind = "known_commit"
	}
	if killAfter >= 0 && code == -1 {
		out.Kind = "unknown"
	}
	if err != nil {
		out.Stderr += "; " + err.Error()
	}
	if stdout.Len() > 0 {
		out.Stderr += "; stdout: " + stdout.String()
	}
	return out, submitted
}

type rpcResponse struct {
	Value object
	Err   error
}
type mcpClient struct {
	cmd     *exec.Cmd
	input   io.WriteCloser
	mu      sync.Mutex
	writeMu sync.Mutex
	next    int
	pending map[int]chan rpcResponse
	fatal   error
	done    chan struct{}
	stderr  bytes.Buffer
	clock   *clock
}

func (t transport) mcp(launch string) (*mcpClient, error) {
	cmd := exec.Command(t.binary, "--home", t.home, "--group", t.group, "--launch", launch, "mcp")
	cmd.Env = cleanEnvironment()
	c := &mcpClient{cmd: cmd, pending: map[int]chan rpcResponse{}, done: make(chan struct{}), clock: t.clock}
	var err error
	c.input, err = cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = &c.stderr
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 4096), 2<<20)
		for scanner.Scan() {
			var v object
			if err := json.Unmarshal(scanner.Bytes(), &v); err != nil {
				c.fail(err)
				break
			}
			id := integer(v["id"])
			c.mu.Lock()
			ch := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ch != nil {
				ch <- rpcResponse{Value: v}
			}
		}
		err := scanner.Err()
		if err == nil {
			err = io.EOF
		}
		c.fail(err)
		close(c.done)
	}()
	result, _, err := c.rpc("initialize", object{"protocolVersion": "2024-11-05", "capabilities": object{}, "clientInfo": object{"name": "icarus-linear-verifier", "version": "1"}})
	if err != nil {
		c.close()
		return nil, err
	}
	if result["error"] != nil {
		c.close()
		return nil, fmt.Errorf("initialize: %v", result)
	}
	c.writeMu.Lock()
	_, err = io.WriteString(c.input, "{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n")
	c.writeMu.Unlock()
	if err != nil {
		c.close()
		return nil, err
	}
	return c, nil
}
func (c *mcpClient) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fatal = err
	for id, ch := range c.pending {
		ch <- rpcResponse{Err: err}
		delete(c.pending, id)
	}
}
func (c *mcpClient) rpc(method string, params object) (object, int64, error) {
	ch := make(chan rpcResponse, 1)
	c.mu.Lock()
	if c.fatal != nil {
		err := c.fatal
		c.mu.Unlock()
		return nil, 0, err
	}
	c.next++
	id := c.next
	c.pending[id] = ch
	c.mu.Unlock()
	frame := canonical(object{"jsonrpc": "2.0", "id": id, "method": method, "params": params}) + "\n"
	c.writeMu.Lock()
	_, err := io.WriteString(c.input, frame)
	submitted := c.clock.stamp()
	c.writeMu.Unlock()
	if err != nil {
		return nil, submitted, err
	}
	select {
	case r := <-ch:
		return r.Value, submitted, r.Err
	case <-time.After(20 * time.Second):
		return nil, submitted, fmt.Errorf("MCP request %d timed out", id)
	}
}
func (c *mcpClient) call(req Request) (Observed, int64) {
	args := decode[object](req.Command)
	delete(args, "method")
	args["member"] = req.Member
	args["launch"] = req.Launch
	if mutation(method(req)) {
		args["operation_id"] = req.OperationID
	}
	raw, submitted, err := c.rpc("tools/call", object{"name": method(req), "arguments": args})
	if err != nil {
		return Observed{Kind: "transport", Stderr: err.Error()}, submitted
	}
	if raw["error"] != nil {
		return Observed{Kind: "transport", Stderr: canonical(raw)}, submitted
	}
	result, _ := raw["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) != 1 {
		return Observed{Kind: "transport", Stderr: canonical(raw)}, submitted
	}
	item, _ := content[0].(map[string]any)
	var response Response
	if err = json.Unmarshal([]byte(str(item["text"])), &response); err != nil {
		return Observed{Kind: "transport", Stderr: err.Error()}, submitted
	}
	if result["isError"] == response.OK {
		return Observed{Kind: "transport", Stderr: "MCP isError disagrees with response"}, submitted
	}
	return Observed{Kind: "response", Response: response}, submitted
}
func (c *mcpClient) close() {
	_ = c.input.Close()
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
		_ = c.cmd.Process.Kill()
		<-c.done
	}
	_ = c.cmd.Wait()
}
