package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/flint-pay/flint-cli/internal/opindex"
)

const latestMCPProtocolVersion = "2025-11-25"

const (
	mcpMaxStreamEvents   = 100
	mcpMaxStreamDuration = 2 * time.Minute
	mcpMaxOutputBytes    = 32 << 20
	// Read results larger than this are refused with advice to narrow them:
	// clients truncate far smaller results, and the agent pays for every byte.
	// Writes always return their result so a completed write is never hidden.
	mcpMaxResultBytes       = 1 << 20
	mcpMaxStderrBytes       = 1 << 20
	mcpMaxLineBytes         = 8 << 20
	mcpConfirmationDeadline = 10 * time.Minute
)

// The server only offers tools, which every listed version supports. Fields a
// version predates (annotations, titles, structured results, elicitation,
// progress messages) are omitted for it.
var supportedMCPProtocolVersions = map[string]bool{
	"2025-11-25": true,
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

// mcpToolName is the MCP name of a canonical command. Dots become underscores
// because clients such as the Claude API accept only [a-zA-Z0-9_-] in tool
// names; no canonical name contains an underscore, so the mapping reverses.
type jsonRPCRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id,omitempty"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params,omitempty"`
}

func mcpRequestKey(id any) string {
	raw, err := json.Marshal(id)
	if err != nil {
		return fmt.Sprintf("%T:%v", id, id)
	}
	return string(raw)
}

type mcpSession struct {
	write       func(map[string]any) error
	mu          sync.Mutex
	elicitation bool
	nextID      int
	pending     map[string]chan map[string]any
}

type mcpSessionKey struct{}

// mcpInFlightCall is a running tools/call. clientCancelled records that the
// client sent notifications/cancelled for it, so it gets no response.
type mcpInFlightCall struct {
	cancel          context.CancelFunc
	clientCancelled atomic.Bool
}

func newMCPSession(write func(map[string]any) error) *mcpSession {
	return &mcpSession{write: write, pending: map[string]chan map[string]any{}}
}

func mcpSessionFrom(ctx context.Context) *mcpSession {
	session, _ := ctx.Value(mcpSessionKey{}).(*mcpSession)
	return session
}

func (s *mcpSession) configure(capabilities map[string]any, protocolVersion string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Elicitation arrived in 2025-06-18. An empty object means form mode;
	// 2025-11-25 clients may instead list the modes they support.
	elicitation, ok := capabilities["elicitation"].(map[string]any)
	_, form := elicitation["form"]
	s.elicitation = ok && protocolVersion >= "2025-06-18" && (len(elicitation) == 0 || form)
}

func (s *mcpSession) canElicit() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.elicitation
}

// request sends a JSON-RPC request to the client and waits for its response.
func (s *mcpSession) request(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	s.mu.Lock()
	s.nextID++
	id := fmt.Sprintf("flint-%d", s.nextID)
	key := mcpRequestKey(id)
	reply := make(chan map[string]any, 1)
	s.pending[key] = reply
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, key)
		s.mu.Unlock()
	}()
	if err := s.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case response := <-reply:
		return response, nil
	case <-ctx.Done():
		_ = s.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": id, "reason": "The tool call ended before the client responded."}})
		return nil, ctx.Err()
	}
}

// deliver routes a client response to the server request awaiting it.
func (s *mcpSession) deliver(id any, response map[string]any) bool {
	s.mu.Lock()
	reply, ok := s.pending[mcpRequestKey(id)]
	s.mu.Unlock()
	if ok {
		select {
		case reply <- response:
		default:
		}
	}
	return ok
}

type mcpInputRecord struct {
	line    []byte
	tooLong bool
	err     error
}

// readMCPLine returns one newline-delimited message. A line longer than limit
// is drained and reported as tooLong so the server can reject it and continue.
func readMCPLine(reader *bufio.Reader, limit int) ([]byte, bool, error) {
	var line []byte
	tooLong := false
	for {
		chunk, err := reader.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > limit+2 {
				tooLong = true
				line = nil
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && (!errors.Is(err, io.EOF) || (len(line) == 0 && !tooLong)) {
			return nil, false, err
		}
		line = bytes.TrimSuffix(line, []byte("\n"))
		line = bytes.TrimSuffix(line, []byte("\r"))
		if !tooLong && len(line) > limit {
			return nil, true, nil
		}
		return line, tooLong, nil
	}
}

func (a *App) serveMCP(opts Options) int {
	if e := a.validateMCPServeOptions(opts); e != nil {
		return a.fail(e, opts)
	}
	index, err := a.operationIndex()
	if err != nil {
		return a.fail(mcpDiscoverySnapshotError(err), opts)
	}
	a.mcpAuth = &mcpAuthState{index: index}
	ctx, cancelInput := context.WithCancel(a.commandContext())
	defer cancelInput()
	reader := bufio.NewReaderSize(a.Stdin, 64<<10)
	input := make(chan mcpInputRecord)
	go func() {
		defer close(input)
		for ctx.Err() == nil {
			line, tooLong, err := readMCPLine(reader, mcpMaxLineBytes)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					select {
					case input <- mcpInputRecord{err: err}:
					case <-ctx.Done():
					}
				}
				return
			}
			select {
			case input <- mcpInputRecord{line: line, tooLong: tooLong}:
			case <-ctx.Done():
				return
			}
		}
	}()
	defer func() {
		cancelInput()
		// Close owned input asynchronously: inherited blocking OS descriptors
		// can make Close wait for Read. Shutdown must not wait on either; the
		// reader only sends to input, and exits if its pending read returns.
		if closer, ok := a.Stdin.(io.Closer); ok {
			go func() { _ = closer.Close() }()
		}
	}()
	encoder := json.NewEncoder(a.Stdout)
	encoder.SetEscapeHTML(false)
	var encoderMu sync.Mutex
	writeResponse := func(response map[string]any) error {
		encoderMu.Lock()
		defer encoderMu.Unlock()
		return encoder.Encode(response)
	}
	session := newMCPSession(writeResponse)
	var inFlightMu sync.Mutex
	inFlight := map[string]*mcpInFlightCall{}
	var workers sync.WaitGroup
	startToolCall := func(req jsonRPCRequest, negotiatedVersion string) {
		key := mcpRequestKey(req.ID)
		inFlightMu.Lock()
		_, duplicate := inFlight[key]
		if !duplicate {
			baseCtx := a.Context
			if baseCtx == nil {
				baseCtx = context.Background()
			}
			toolCtx, cancel := context.WithCancel(context.WithValue(baseCtx, mcpSessionKey{}, session))
			call := &mcpInFlightCall{cancel: cancel}
			inFlight[key] = call
			workers.Add(1)
			go func(req jsonRPCRequest, protocolVersion, key string) {
				defer workers.Done()
				response := a.handleMCPVersionContext(toolCtx, req, opts, protocolVersion)
				if toolCtx.Err() != nil {
					response = rpcError(req.ID, -32800, "Request canceled")
				}
				inFlightMu.Lock()
				delete(inFlight, key)
				inFlightMu.Unlock()
				cancel()
				// The receiver of notifications/cancelled should not respond to
				// the cancelled request; only a server shutdown reports -32800.
				if call.clientCancelled.Load() {
					return
				}
				if err := writeResponse(response); err != nil {
					fmt.Fprintln(a.Stderr, "MCP output failed: "+err.Error())
				}
			}(req, negotiatedVersion, key)
		}
		inFlightMu.Unlock()
		if duplicate {
			_ = writeResponse(rpcError(req.ID, -32600, "A request with this id is already running"))
		}
	}
	cancelInFlight := func() {
		inFlightMu.Lock()
		defer inFlightMu.Unlock()
		for _, call := range inFlight {
			call.cancel()
		}
	}
	defer func() {
		cancelInFlight()
		workers.Wait()
	}()
	initializeSeen := false
	initialized := false
	protocolVersion := latestMCPProtocolVersion
	for {
		var record mcpInputRecord
		select {
		case <-ctx.Done():
			return ExitOK
		case next, ok := <-input:
			if !ok {
				return ExitOK
			}
			record = next
		}
		if ctx.Err() != nil {
			return ExitOK
		}
		if record.err != nil {
			fmt.Fprintln(a.Stderr, "MCP input failed: "+record.err.Error())
			return ExitNetwork
		}
		var response map[string]any
		var req jsonRPCRequest
		message := mcpClassifyMessage(record)
		switch message.kind {
		case mcpMessageBlank:
			continue
		case mcpMessageInvalid:
			if message.log != "" {
				fmt.Fprintln(a.Stderr, "MCP: "+message.log)
			}
			response = message.response
		case mcpMessageClientResponse:
			if !session.deliver(message.id, message.body) {
				fmt.Fprintln(a.Stderr, "MCP: ignored a response to unknown request id "+mcpRequestKey(message.id))
			}
			continue
		case mcpMessageNotification:
			req = message.request
			if req.Method == "notifications/initialized" && initializeSeen {
				initialized = true
			}
			if req.Method == "notifications/cancelled" {
				if requestID, ok := req.Params["requestId"]; ok {
					key := mcpRequestKey(requestID)
					inFlightMu.Lock()
					call := inFlight[key]
					inFlightMu.Unlock()
					if call != nil {
						call.clientCancelled.Store(true)
						call.cancel()
					}
				}
			}
			if !strings.HasPrefix(req.Method, "notifications/") {
				// A request without an id is a notification and never gets a
				// response. Do not run it: a tool call must report its result.
				fmt.Fprintln(a.Stderr, "MCP: ignored "+terminalSafe(req.Method)+" sent without an id; requests need an id to receive a response")
			}
			continue
		case mcpMessageRequest:
			req = message.request
			switch {
			case strings.HasPrefix(req.Method, "notifications/"):
				response = rpcError(req.ID, -32601, "Method not found: "+req.Method+" is a notification and must be sent without an id")
			case req.Method != "initialize" && req.Method != "ping" && !initialized:
				response = rpcError(req.ID, -32002, "Server is not initialized")
			case req.Method == "initialize" && initializeSeen:
				response = rpcError(req.ID, -32600, "Initialize may only be called once")
			case req.Method == "tools/call":
				startToolCall(req, protocolVersion)
				continue
			default:
				response = a.handleMCPVersion(req, opts, protocolVersion)
				if req.Method == "initialize" {
					if _, failed := response["error"]; !failed {
						initializeSeen = true
						initialized = false
						if negotiated, ok := lookupPath(response, "result.protocolVersion"); ok {
							protocolVersion, _ = negotiated.(string)
						}
						capabilities, _ := req.Params["capabilities"].(map[string]any)
						session.configure(capabilities, protocolVersion)
						workers.Add(1)
						go func() { defer workers.Done(); a.mcpPrefetchAuth(ctx, opts) }()
					}
				}
			}
		}
		if err := writeResponse(response); err != nil {
			fmt.Fprintln(a.Stderr, "MCP output failed: "+err.Error())
			return ExitNetwork
		}
	}
}

type mcpMessageKind int

type mcpMessage struct {
	kind     mcpMessageKind
	request  jsonRPCRequest
	response map[string]any
	id       any
	body     map[string]any
	log      string
}

// mcpClassifyMessage applies JSON-RPC 2.0 framing rules to one input line.
func mcpClassifyMessage(record mcpInputRecord) mcpMessage {
	if record.tooLong {
		return mcpMessage{kind: mcpMessageInvalid, response: rpcError(nil, -32600, fmt.Sprintf("Invalid Request: message exceeds the %d byte limit", mcpMaxLineBytes)), log: fmt.Sprintf("rejected a message larger than %d bytes", mcpMaxLineBytes)}
	}
	trimmed := bytes.TrimSpace(record.line)
	if len(trimmed) == 0 {
		return mcpMessage{kind: mcpMessageBlank}
	}
	var value any
	if !utf8.Valid(record.line) {
		return mcpMessage{kind: mcpMessageInvalid, response: rpcError(nil, -32700, "Parse error: invalid UTF-8")}
	}
	if err := decodeJSONNumbers(trimmed, &value); err != nil {
		return mcpMessage{kind: mcpMessageInvalid, response: rpcError(nil, -32700, "Parse error")}
	}
	if _, batch := value.([]any); batch {
		return mcpMessage{kind: mcpMessageInvalid, response: rpcError(nil, -32600, "Invalid Request: JSON-RPC batches are not supported; send one message per line")}
	}
	object, ok := value.(map[string]any)
	if !ok {
		return mcpMessage{kind: mcpMessageInvalid, response: rpcError(nil, -32600, "Invalid Request")}
	}
	id, hasID := object["id"]
	validID := mcpValidID(id)
	responseID := any(nil)
	if validID {
		responseID = id
	}
	version, _ := object["jsonrpc"].(string)
	rawMethod, hasMethod := object["method"]
	if !hasMethod {
		_, hasResult := object["result"]
		_, hasError := object["error"]
		if version == "2.0" && validID && (hasResult || hasError) {
			return mcpMessage{kind: mcpMessageClientResponse, id: id, body: object}
		}
		return mcpMessage{kind: mcpMessageInvalid, response: rpcError(responseID, -32600, "Invalid Request")}
	}
	method, _ := rawMethod.(string)
	if version != "2.0" || method == "" || (hasID && !validID) {
		return mcpMessage{kind: mcpMessageInvalid, response: rpcError(responseID, -32600, "Invalid Request")}
	}
	req := jsonRPCRequest{JSONRPC: version, Method: method}
	if hasID {
		req.ID = id
	}
	if rawParams, exists := object["params"]; exists && rawParams != nil {
		params, ok := rawParams.(map[string]any)
		if !ok {
			if !hasID {
				return mcpMessage{kind: mcpMessageBlank}
			}
			return mcpMessage{kind: mcpMessageInvalid, response: rpcError(id, -32602, "Invalid params: params must be an object")}
		}
		if meta, ok := params["_meta"].(map[string]any); ok {
			if token, exists := meta["progressToken"]; exists && !mcpValidID(token) {
				if !hasID {
					return mcpMessage{kind: mcpMessageBlank}
				}
				return mcpMessage{kind: mcpMessageInvalid, response: rpcError(id, -32602, "Invalid params: _meta.progressToken must be a string or safe integer")}
			}
		}
		req.Params = params
	}
	if !hasID {
		return mcpMessage{kind: mcpMessageNotification, request: req}
	}
	return mcpMessage{kind: mcpMessageRequest, request: req}
}

// mcpValidID preserves the JSON spelling: fractional and exponent forms are
// invalid even when their numeric value is an integer.
func mcpValidID(id any) bool {
	switch value := id.(type) {
	case string:
		return true
	case json.Number:
		integer, err := strconv.ParseInt(string(value), 10, 64)
		return err == nil && integer >= -(1<<53-1) && integer <= 1<<53-1
	}
	return false
}

func (a *App) handleMCP(req jsonRPCRequest, opts Options) map[string]any {
	return a.handleMCPVersion(req, opts, latestMCPProtocolVersion)
}

func (a *App) handleMCPVersion(req jsonRPCRequest, opts Options, protocolVersion string) map[string]any {
	ctx := a.Context
	if ctx == nil {
		ctx = context.Background()
	}
	return a.handleMCPVersionContext(ctx, req, opts, protocolVersion)
}

func (a *App) handleMCPVersionContext(ctx context.Context, req jsonRPCRequest, opts Options, protocolVersion string) map[string]any {
	policy := a.mcpPolicy(opts)
	switch req.Method {
	case "initialize":
		requested, _ := req.Params["protocolVersion"].(string)
		if requested == "" {
			return rpcError(req.ID, -32602, "initialize requires params.protocolVersion")
		}
		if _, ok := req.Params["capabilities"].(map[string]any); !ok {
			return rpcError(req.ID, -32602, "initialize requires params.capabilities")
		}
		clientInfo, ok := req.Params["clientInfo"].(map[string]any)
		if !ok {
			return rpcError(req.ID, -32602, "initialize requires params.clientInfo")
		}
		clientName, _ := clientInfo["name"].(string)
		clientVersion, _ := clientInfo["version"].(string)
		if clientName == "" || clientVersion == "" {
			return rpcError(req.ID, -32602, "initialize clientInfo requires name and version")
		}
		protocolVersion := latestMCPProtocolVersion
		if supportedMCPProtocolVersions[requested] {
			protocolVersion = requested
		}
		return rpcResult(req.ID, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "flint", "version": a.Info.Version},
			"instructions":    mcpInstructions(policy),
		})
	case "ping":
		return rpcResult(req.ID, map[string]any{})
	case "tools/list":
		tools, e := a.mcpAdvertisedTools(policy, protocolVersion)
		if e != nil {
			return rpcError(req.ID, -32603, e.Message)
		}
		return rpcResult(req.ID, map[string]any{"tools": tools})
	case "tools/call":
		name, ok := req.Params["name"].(string)
		if !ok || name == "" {
			return rpcError(req.ID, -32602, "tools/call requires params.name")
		}
		arguments := map[string]any{}
		if raw, exists := req.Params["arguments"]; exists {
			arguments, ok = raw.(map[string]any)
			if !ok {
				return rpcError(req.ID, -32602, "tools/call params.arguments must be an object")
			}
		}
		meta, _ := req.Params["_meta"].(map[string]any)
		if token, ok := meta["progressToken"]; ok && token != nil {
			if session := mcpSessionFrom(ctx); session != nil {
				ctx = context.WithValue(ctx, mcpProgressKey{}, &mcpProgress{session: session, token: token, messages: protocolVersion >= "2025-03-26"})
			}
		}
		return a.mcpDispatch(ctx, req.ID, name, arguments, opts, policy, protocolVersion >= "2025-06-18")
	default:
		return rpcError(req.ID, -32601, "Method not found")
	}
}

func rpcResult(id, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}
func rpcError(id any, code int, message string) map[string]any {
	response := map[string]any{"jsonrpc": "2.0", "error": map[string]any{"code": code, "message": message}}
	if id != nil {
		response["id"] = id
	}
	return response
}

// mcpAuthState is a connection-local snapshot. Discovery reads it without IO.
type mcpAuthState struct {
	index *opindex.Index
	mu    sync.RWMutex
	auth  AuthContext
	known bool
}

func (s *mcpAuthState) snapshot() (AuthContext, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.auth, s.known
}
func (s *mcpAuthState) store(auth AuthContext) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auth, s.known = auth, true
}
func (a *App) mcpPrefetchAuth(ctx context.Context, opts Options) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	opts.Timeout = 5 * time.Second
	opts.Quiet, opts.Debug = true, false
	probe := *a
	probe.Stderr = io.Discard
	resolved, _, err := probe.resolveConfig(opts)
	if err != nil {
		return
	}
	command, ok := a.Registry.ByName("auth.status")
	if !ok {
		return
	}
	_, _, envelope, authErr := probe.authenticateCommand(ctx, command, opts, resolved)
	if authErr == nil && envelope.Data.AuthType != "none" {
		a.mcpAuth.store(envelope.Data)
	}
}
