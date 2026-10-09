package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
)

// `flats mcp` lets an agent start Flats as a stdio MCP server, the one MCP
// form Claude Code, Codex and Cursor plugins all launch the same way. It
// relays each newline-delimited JSON-RPC message from stdin to the host's
// Streamable HTTP endpoint and writes every message of the reply to stdout,
// so the tools, resources and instructions are exactly the host's. The host
// comes from --url, FLATS_URL or `flats connect`, never from the plugin.

func (a *app) mcpBridge(args []string) error {
	flags := a.flags("mcp")
	if _, err := a.parse(flags, args, 0, 0); err != nil {
		return err
	}
	hc := *a.env.HTTP
	hc.Timeout = 0 // a tool call may legitimately run long; stdin EOF or a signal ends it
	b := &mcpBridge{
		endpoint: strings.TrimRight(a.url, "/") + "/mcp",
		hc:       &hc,
		out:      a.env.Stdout,
	}
	fmt.Fprintf(a.errw, "flats mcp: relaying to %s (%s)\n", b.endpoint, a.urlSource)
	return b.run(a.ctx, a.env.Stdin)
}

type mcpBridge struct {
	endpoint string
	hc       *http.Client

	outMu sync.Mutex
	out   io.Writer

	mu       sync.Mutex
	session  string // Mcp-Session-Id, when the host assigns one
	protocol string // negotiated MCP-Protocol-Version
}

// maxBridgeLine bounds one inbound message. Inline uploads are base64, so
// allow well above the host's default 20 MiB upload limit; the host enforces
// its own body limit.
const maxBridgeLine = 256 << 20

func (b *mcpBridge) run(ctx context.Context, stdin io.Reader) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		r := bufio.NewReaderSize(stdin, 64<<10)
		for {
			line, err := readBridgeLine(r)
			if len(bytes.TrimSpace(line)) > 0 {
				select {
				case lines <- line:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				readErr <- err
				return
			}
		}
	}()

	var inflight sync.WaitGroup
	defer func() {
		// The client closed stdin or we were signalled: stop pending calls,
		// then end the host session.
		cancel()
		inflight.Wait()
		b.endSession()
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case line := <-lines:
			if !json.Valid(line) {
				b.emitError(nil, -32700, "parse error: not a JSON-RPC message")
				continue
			}
			msg := bytes.TrimSpace(line)
			if isInitialize(msg) {
				// Later messages need the session and protocol version that
				// initialize negotiates, and clients wait for its reply anyway.
				b.forward(ctx, msg)
				continue
			}
			inflight.Add(1)
			go func() {
				defer inflight.Done()
				b.forward(ctx, msg)
			}()
		}
	}
}

func readBridgeLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxBridgeLine {
			return nil, fmt.Errorf("an MCP message exceeds %d MiB", maxBridgeLine>>20)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, err
	}
}

type rpcEnvelope struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

func isInitialize(msg []byte) bool {
	var m rpcEnvelope
	return json.Unmarshal(msg, &m) == nil && m.Method == "initialize"
}

// forward POSTs one message and relays the reply. Failures that leave a
// request unanswered become a JSON-RPC error for that request, so the agent
// sees why instead of waiting.
func (b *mcpBridge) forward(ctx context.Context, msg []byte) {
	var env rpcEnvelope
	_ = json.Unmarshal(msg, &env) // a batch array leaves env empty
	isRequest := env.Method != "" && len(env.ID) > 0 && string(env.ID) != "null"
	fail := func(format string, args ...any) {
		if isRequest {
			b.emitError(env.ID, -32603, fmt.Sprintf(format, args...))
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint, bytes.NewReader(msg))
	if err != nil {
		fail("flats mcp: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	b.setSessionHeaders(req.Header)
	resp, err := b.hc.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			fail("cannot reach Flats at %s: %v (is the host running? choose it with `flats connect <url>`)", b.endpoint, err)
		}
		return
	}
	defer resp.Body.Close()
	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		b.mu.Lock()
		b.session = id
		b.mu.Unlock()
	}

	switch {
	case resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent:
		return
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		// The SDK answers some failures with a JSON-RPC error body; relay it.
		if isRequest && json.Valid(body) && bytes.Contains(body, []byte(`"jsonrpc"`)) {
			b.emit(body, env.Method == "initialize")
			return
		}
		fail("Flats at %s answered %s: %s", b.endpoint, resp.Status, strings.TrimSpace(string(body)))
		return
	}

	ctype, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch ctype {
	case "text/event-stream":
		answered := false
		err := readSSE(resp.Body, func(data []byte) {
			if replyTo(data, env.ID) {
				answered = true
			}
			b.emit(data, env.Method == "initialize")
		})
		if !answered && ctx.Err() == nil {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			fail("Flats event stream ended without a reply: %v", err)
		}
	default:
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			if ctx.Err() == nil {
				fail("reading the Flats reply: %v", err)
			}
			return
		}
		if len(bytes.TrimSpace(body)) == 0 {
			fail("Flats sent an empty reply")
			return
		}
		b.emit(body, env.Method == "initialize")
	}
}

func (b *mcpBridge) setSessionHeaders(h http.Header) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.session != "" {
		h.Set("Mcp-Session-Id", b.session)
	}
	if b.protocol != "" {
		h.Set("MCP-Protocol-Version", b.protocol)
	}
}

// replyTo reports whether data is the response to request id.
func replyTo(data, id json.RawMessage) bool {
	if len(id) == 0 {
		return false
	}
	var m rpcEnvelope
	return json.Unmarshal(data, &m) == nil && m.Method == "" && bytes.Equal(bytes.TrimSpace(m.ID), bytes.TrimSpace(id))
}

// emit writes one message as a single stdout line. An initialize reply
// also records the negotiated protocol version for later requests.
func (b *mcpBridge) emit(msg []byte, initialize bool) {
	var line bytes.Buffer
	if err := json.Compact(&line, msg); err != nil {
		return
	}
	if initialize {
		var r struct {
			Result struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"result"`
		}
		if json.Unmarshal(line.Bytes(), &r) == nil && r.Result.ProtocolVersion != "" {
			b.mu.Lock()
			b.protocol = r.Result.ProtocolVersion
			b.mu.Unlock()
		}
	}
	line.WriteByte('\n')
	b.outMu.Lock()
	defer b.outMu.Unlock()
	_, _ = b.out.Write(line.Bytes())
}

func (b *mcpBridge) emitError(id json.RawMessage, code int, message string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	msg, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
	b.emit(msg, false)
}

// endSession tells a stateful host that the client is gone.
func (b *mcpBridge) endSession() {
	b.mu.Lock()
	session := b.session
	b.mu.Unlock()
	if session == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, b.endpoint, nil)
	if err != nil {
		return
	}
	b.setSessionHeaders(req.Header)
	if resp, err := b.hc.Do(req); err == nil {
		resp.Body.Close()
	}
}

// readSSE calls fn with the data of each server-sent event.
func readSSE(r io.Reader, fn func([]byte)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxBridgeLine)
	var data []byte
	flush := func() {
		if len(data) > 0 {
			fn(data)
			data = nil
		}
	}
	for sc.Scan() {
		line := sc.Bytes()
		switch {
		case len(line) == 0:
			flush()
		case bytes.HasPrefix(line, []byte("data:")):
			v := bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" "))
			if data != nil {
				data = append(data, '\n')
			}
			data = append(data, v...)
		}
	}
	flush()
	return sc.Err()
}
