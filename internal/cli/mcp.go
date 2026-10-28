package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
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
func mcpToolName(canonical string) string { return strings.ReplaceAll(canonical, ".", "_") }

// mcpCanonicalToolName accepts an MCP tool name or the older dotted form.
func mcpCanonicalToolName(name string) string {
	return strings.ReplaceAll(strings.TrimSpace(name), "_", ".")
}

// mcpDefaultToolPrefixes is the tool set served without --tools: the resources
// most integrations start with. Large groups contribute only their core
// tools, since every tool costs the agent context. --tools all serves every
// tool, and --tools default,GROUP adds whole groups to this set.
var mcpDefaultToolPrefixes = []string{
	"customers.create", "customers.get", "customers.list", "customers.update",
	"products.create", "products.get", "products.list",
	"payment-intents.create", "payment-intents.get", "payment-intents.list", "payment-intents.confirm", "payment-intents.capture", "payment-intents.cancel",
	"payment-links.create", "payment-links.get", "payment-links.list",
	"checkout-sessions.create", "checkout-sessions.get",
	"refunds.create", "refunds.get", "refunds.list",
	"webhook-events.list", "webhook-events.get",
	"webhook-endpoints.create", "webhook-endpoints.list",
}

const mcpDefaultToolSetSummary = "customers, products, payment intents, refunds, webhook endpoints and events, and the core checkout session and payment link tools"

// mcpAlwaysEnabled names the tools every server offers, whatever --tools
// says, so the guidance in help and the instructions always resolves.
func mcpAlwaysEnabled(name string) bool {
	return name == "help" || strings.HasPrefix(name, "schema.")
}

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

type mcpCappedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (b *mcpCappedBuffer) Write(value []byte) (int, error) {
	if b.Len()+len(value) > b.limit {
		b.exceeded = true
		return 0, fmt.Errorf("MCP command output exceeds %d bytes", b.limit)
	}
	return b.Buffer.Write(value)
}

// mcpPolicy holds the safety choices the human made when starting the server.
// Agents can narrow, but never widen, what these flags allow.
type mcpPolicy struct {
	allowLive        bool
	allowDestructive bool
	// allTools is --tools all. Otherwise toolPrefixes holds the canonical
	// prefixes --tools named, and no --tools at all means the default set.
	allTools     bool
	toolPrefixes []string
	toolsFlag    []string // the --tools entries as the human wrote them
}

func mcpBoolOption(opts Options, name string) bool {
	values := opts.Raw[name]
	return len(values) > 0 && values[len(values)-1] == "true"
}

func mcpPolicyFromOptions(opts Options) mcpPolicy {
	policy := mcpPolicy{allowLive: mcpBoolOption(opts, "allow-live"), allowDestructive: mcpBoolOption(opts, "allow-destructive")}
	for _, raw := range opts.Raw["tools"] {
		for _, entry := range strings.Split(raw, ",") {
			entry = strings.TrimSpace(entry)
			prefix := strings.TrimRight(mcpCanonicalToolName(entry), ".")
			if prefix == "" {
				continue
			}
			policy.toolsFlag = append(policy.toolsFlag, entry)
			switch prefix {
			case "all":
				policy.allTools = true
			case "default":
				policy.toolPrefixes = append(policy.toolPrefixes, mcpDefaultToolPrefixes...)
			default:
				policy.toolPrefixes = append(policy.toolPrefixes, prefix)
			}
		}
	}
	return policy
}

func mcpPrefixMatches(prefix, name string) bool {
	return name == prefix || strings.HasPrefix(name, prefix+".")
}

// defaultToolSet reports whether the server runs without --tools.
func (p mcpPolicy) defaultToolSet() bool { return len(p.toolsFlag) == 0 }

func (p mcpPolicy) includes(name string) bool {
	if p.allTools || mcpAlwaysEnabled(name) {
		return true
	}
	prefixes := p.toolPrefixes
	if p.defaultToolSet() {
		prefixes = mcpDefaultToolPrefixes
	}
	for _, prefix := range prefixes {
		if mcpPrefixMatches(prefix, name) {
			return true
		}
	}
	return false
}

// toolSetDescription names the active tool set for instructions and errors.
func (p mcpPolicy) toolSetDescription() string {
	switch {
	case p.allTools:
		return "every Flint tool (--tools all)"
	case p.defaultToolSet():
		return "the default tool set (" + mcpDefaultToolSetSummary + ")"
	default:
		return "the tools selected with --tools " + strings.Join(p.toolsFlag, ",")
	}
}

func (a *App) validateMCPServeOptions(opts Options) *CLIError {
	policy := mcpPolicyFromOptions(opts)
	if len(policy.toolsFlag) == 0 && strings.TrimSpace(strings.Join(opts.Raw["tools"], "")) != "" {
		return usageError("INVALID_FLAG_VALUE", "--tools must name at least one tool group or tool, such as customers, or default or all.", "tools")
	}
	all := a.mcpAllTools()
	var unknown []string
	for _, prefix := range policy.toolPrefixes {
		matched := false
		for _, tool := range all {
			if mcpPrefixMatches(prefix, tool.CanonicalName) {
				matched = true
				break
			}
		}
		if !matched {
			unknown = append(unknown, prefix)
		}
	}
	if len(unknown) > 0 {
		var names []string
		for _, prefix := range unknown {
			names = append(names, mcpToolName(prefix))
		}
		e := usageError("UNKNOWN_TOOL_PREFIX", "--tools "+strings.Join(names, ",")+" matches no MCP tool. Use tool groups such as customers or payment-intents, tool names such as payment-intents_create, default for the default set, or all.", "tools")
		suggestions := []string{}
		for _, prefix := range unknown {
			candidates := mcpToolGroups(all)
			if strings.Contains(prefix, ".") {
				for _, tool := range all {
					candidates = append(candidates, tool.CanonicalName)
				}
			}
			suggestions = append(suggestions, mcpToolNames(mcpNameSuggestions(prefix, candidates))...)
		}
		e.Details = suggestionDetails(strings.Join(names, ","), suggestions)
		return e
	}
	if opts.Live && !policy.allowLive {
		return usageError("LIVE_REQUIRES_ALLOW_LIVE", "flint mcp serve --live requires --allow-live, which lets tools act on live (production) data.", "live")
	}
	return nil
}

// mcpSession carries per-connection state that tool calls need: the client's
// declared capabilities and server-to-client requests over the shared stream.
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

const (
	mcpMessageBlank mcpMessageKind = iota
	mcpMessageInvalid
	mcpMessageRequest
	mcpMessageNotification
	mcpMessageClientResponse
)

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
	validID := false
	switch id.(type) {
	case string, json.Number:
		validID = true
	}
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
		req.Params = params
	}
	if !hasID {
		return mcpMessage{kind: mcpMessageNotification, request: req}
	}
	return mcpMessage{kind: mcpMessageRequest, request: req}
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
	policy := mcpPolicyFromOptions(opts)
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
		tools := []map[string]any{}
		for _, cmd := range a.mcpTools(policy) {
			schema, e := schemaForMCPTool(cmd, policy)
			if e != nil {
				return rpcError(req.ID, -32603, "Could not generate tool input schema for "+mcpToolName(cmd.CanonicalName))
			}
			title := mcpToolTitle(cmd)
			tool := map[string]any{"name": mcpToolName(cmd.CanonicalName), "description": mcpToolDescription(cmd, schema), "inputSchema": schema}
			// Output schemas are optional in MCP. Each embedded its full
			// component closure (about 19 MB in total), so agents fetch one on
			// demand with the schema_output tool instead.
			if protocolVersion >= "2025-03-26" {
				annotations := mcpToolAnnotations(cmd)
				annotations["title"] = title
				tool["annotations"] = annotations
			}
			if protocolVersion >= "2025-06-18" {
				tool["title"] = title
			}
			tools = append(tools, tool)
		}
		return rpcResult(req.ID, map[string]any{"tools": tools})
	case "tools/call":
		name, nameOK := req.Params["name"].(string)
		if !nameOK || name == "" {
			return rpcError(req.ID, -32602, "tools/call requires params.name")
		}
		cmd, ok := a.mcpTool(name, policy)
		if !ok {
			return a.mcpUnknownToolError(req.ID, name, policy)
		}
		arguments := map[string]any{}
		if rawArguments, exists := req.Params["arguments"]; exists {
			var argumentsOK bool
			arguments, argumentsOK = rawArguments.(map[string]any)
			if !argumentsOK {
				return rpcError(req.ID, -32602, "tools/call params.arguments must be an object")
			}
		}
		if message := mcpGateViolation(arguments, policy); message != "" {
			return rpcResult(req.ID, mcpToolError(message, nil))
		}
		if err := validateMCPToolArguments(cmd, arguments, policy); err != nil {
			return rpcResult(req.ID, mcpToolError("Tool arguments do not match the advertised input schema: "+err.Error(), nil))
		}
		if message := a.mcpSelectionViolation(arguments, opts); message != "" {
			return rpcResult(req.ID, mcpToolError(message, nil))
		}
		structured := protocolVersion >= "2025-06-18"
		arguments = mcpCanonicalArguments(cmd, arguments)
		if result, handled := a.mcpLocalToolResult(ctx, cmd, arguments, opts, policy, structured); handled {
			return rpcResult(req.ID, result)
		}
		meta, _ := req.Params["_meta"].(map[string]any)
		if token, ok := meta["progressToken"]; ok && token != nil {
			if session := mcpSessionFrom(ctx); session != nil {
				ctx = context.WithValue(ctx, mcpProgressKey{}, &mcpProgress{session: session, token: token, messages: protocolVersion >= "2025-03-26"})
			}
		}
		return rpcResult(req.ID, a.runMCPTool(ctx, cmd, arguments, opts, policy, structured))
	default:
		return rpcError(req.ID, -32601, "Method not found")
	}
}

func mcpInstructions(policy mcpPolicy) string {
	live := "Live (production) credentials and contexts are refused; the human must restart the server with --allow-live to permit them."
	if policy.allowLive {
		live = "Live (production) access is permitted: pass _flint.live=true to acknowledge a live credential."
	}
	confirm := "Destructive operations, and sensitive ones in live mode, need the human's confirmation: the server asks through elicitation when the client supports it, otherwise the call fails with guidance for the human."
	if policy.allowDestructive {
		confirm = "Destructive operations, and sensitive ones in live mode, require _flint.confirm=true."
	}
	tools := "This server exposes " + policy.toolSetDescription() + "; help and the schema tools are always available."
	if !policy.allTools {
		tools += " The user can expose more with flint mcp serve --tools all, or --tools default,<group>."
	}
	return strings.Join([]string{
		"Flint Pay MCP server. Each tool runs one Flint CLI command against the Flint public API, named <resource>_<action> (for example customers_create, or customers_addresses_create for nested resources), using the credential configured for this server (FLINT_API_KEY, FLINT_PROFILE, or a saved flint login).",
		tools,
		"Results that reached the API name the credential environment, sandbox or live, in a \"Flint environment:\" text line; CLI warnings follow in a \"Warnings:\" line.",
		live,
		confirm,
		"Money amounts are integer minor units (2500 is 25.00 USD). Lists return one page and a next_page_token (empty on the last page; there is no has_more): pass it as page_token, or set _flint.all.",
		"Before a write, use _flint.dry_run=\"client\" (or _flint.preview for destructive operations) to inspect the request without making it. Set _flint.idempotency_key on writes and reuse the same key when retrying after a timeout, or the operation may run twice.",
		"Call help without a topic for an index, help with a tool name for its arguments and required fields, schema_input for a tool's full input schema, and schema_output for its result schema.",
	}, " ")
}

// mcpHiddenLocalTools are local commands that change the human's own setup.
// Agents must not rewrite the default profile or merchant guard (config set/unset)
// or remove the server's credential (auth logout); the human runs these.
var mcpHiddenLocalTools = map[string]bool{"config.set": true, "config.unset": true, "auth.logout": true}

func mcpCommandExposed(cmd *Command) bool {
	return cmd != nil && cmd.CanonicalName == cmd.Name && cmd.CanonicalName != "mcp.serve" && !cmd.MCPHidden && !mcpHiddenLocalTools[cmd.CanonicalName]
}

// mcpAllTools lists every tool the server can expose. History is split so
// listing stays read-only while clearing is a separate destructive tool, and
// doctor loses --fix, which rewrites the global default profile unasked.
func (a *App) mcpAllTools() []*Command {
	a.Registry.ensureAPICommands()
	tools := []*Command{}
	for _, cmd := range a.Registry.Commands {
		if !mcpCommandExposed(cmd) {
			continue
		}
		switch cmd.CanonicalName {
		case "history":
			list, clear := mcpHistoryTools(cmd)
			tools = append(tools, list, clear)
		case "doctor":
			doctor := *cmd
			doctor.Arguments = nil
			for _, arg := range cmd.Arguments {
				if arg.Name != "fix" {
					doctor.Arguments = append(doctor.Arguments, arg)
				}
			}
			tools = append(tools, &doctor)
		default:
			tools = append(tools, cmd)
		}
	}
	return tools
}

func mcpHistoryTools(history *Command) (*Command, *Command) {
	list := *history
	list.Arguments = nil
	for _, arg := range history.Arguments {
		if arg.Name != "clear" {
			list.Arguments = append(list.Arguments, arg)
		}
	}
	list.Description = "List recent Flint resource IDs recorded by this CLI"
	clear := list
	clear.Name, clear.CanonicalName = "history.clear", "history.clear"
	clear.Description = "Clear all recent Flint resource IDs recorded by this CLI"
	clear.Destructive = true
	clear.Supports = Supports{JSONOutput: history.Supports.JSONOutput, NoInput: history.Supports.NoInput}
	return &list, &clear
}

func (a *App) mcpTools(policy mcpPolicy) []*Command {
	tools := []*Command{}
	for _, tool := range a.mcpAllTools() {
		if policy.includes(tool.CanonicalName) {
			tools = append(tools, tool)
		}
	}
	return tools
}

// mcpTool finds an enabled tool by MCP name or by its older dotted name.
func (a *App) mcpTool(name string, policy mcpPolicy) (*Command, bool) {
	canonical := mcpCanonicalToolName(name)
	if !policy.includes(canonical) {
		return nil, false
	}
	for _, tool := range a.mcpAllTools() {
		if tool.CanonicalName == canonical {
			return tool, true
		}
	}
	return nil, false
}

func mcpToolNames(canonical []string) []string {
	names := make([]string, 0, len(canonical))
	for _, name := range canonical {
		names = append(names, mcpToolName(name))
	}
	return names
}

func (a *App) mcpUnknownToolError(id any, name string, policy mcpPolicy) map[string]any {
	canonical := mcpCanonicalToolName(name)
	for _, tool := range a.mcpAllTools() {
		if tool.CanonicalName == canonical {
			group, _, _ := strings.Cut(canonical, ".")
			enable := "with " + group + " added to --tools"
			if policy.defaultToolSet() {
				enable = "with --tools default," + group
			}
			return rpcError(id, -32602, "Unknown tool: "+mcpToolName(canonical)+" is not enabled on this server, which exposes "+policy.toolSetDescription()+". Do not retry; the user can enable it by restarting the server "+enable+" (or --tools all).")
		}
	}
	var names []string
	for _, tool := range a.mcpTools(policy) {
		names = append(names, tool.CanonicalName)
	}
	var suggestions []string
	if alias, ok := a.Registry.ByName(canonical); ok && alias.CanonicalName != canonical && policy.includes(alias.CanonicalName) {
		suggestions = append(suggestions, alias.CanonicalName)
	}
	for _, suggestion := range mcpNameSuggestions(canonical, names) {
		if len(suggestions) < 3 && !containsString(suggestions, suggestion) {
			suggestions = append(suggestions, suggestion)
		}
	}
	suggestions = mcpToolNames(suggestions)
	message := "Unknown tool: " + name + "."
	if len(suggestions) > 0 {
		message += " Did you mean " + strings.Join(suggestions, ", ") + "?"
	}
	response := rpcError(id, -32602, message)
	response["error"].(map[string]any)["data"] = map[string]any{"suggestions": suggestions}
	return response
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func mcpToolGroups(tools []*Command) []string {
	seen := map[string]bool{}
	var groups []string
	for _, tool := range tools {
		group, _, _ := strings.Cut(tool.CanonicalName, ".")
		if !seen[group] {
			seen[group] = true
			groups = append(groups, group)
		}
	}
	return groups
}

// mcpSingular folds each dotted segment to a rough singular, so customer and
// customers.list match customers and customers.lists.
func mcpSingular(name string) string {
	segments := strings.Split(name, ".")
	for i, segment := range segments {
		switch {
		case strings.HasSuffix(segment, "ies"):
			segment = strings.TrimSuffix(segment, "ies") + "y"
		case strings.HasSuffix(segment, "sses"), strings.HasSuffix(segment, "xes"):
			segment = segment[:len(segment)-2]
		case strings.HasSuffix(segment, "s") && !strings.HasSuffix(segment, "ss"):
			segment = strings.TrimSuffix(segment, "s")
		}
		segments[i] = segment
	}
	return strings.Join(segments, ".")
}

// mcpNameSuggestions ranks canonical names: singular and plural forms of the
// input first, then names that extend it, then the closest by edit distance.
func mcpNameSuggestions(input string, names []string) []string {
	input = strings.ToLower(mcpCanonicalToolName(input))
	type candidate struct {
		name         string
		rank, length int
	}
	var candidates []candidate
	limit := max(2, len(input)/3)
	singular := mcpSingular(input)
	for _, name := range names {
		distance := levenshteinDistance(input, name)
		rank := 2
		switch {
		case mcpSingular(name) == singular:
			rank = 0
		case strings.HasPrefix(name, input) || strings.HasPrefix(input, name+".") || strings.HasPrefix(mcpSingular(name), singular):
			rank = 1
		case distance > limit:
			continue
		}
		candidates = append(candidates, candidate{name, rank, distance})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].rank != candidates[j].rank {
			return candidates[i].rank < candidates[j].rank
		}
		if candidates[i].length != candidates[j].length {
			return candidates[i].length < candidates[j].length
		}
		return candidates[i].name < candidates[j].name
	})
	var out []string
	for _, c := range candidates {
		if len(out) == 3 {
			break
		}
		out = append(out, c.name)
	}
	return out
}

func levenshteinDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current := make([]int, len(b)+1)
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
		}
		previous = current
	}
	return previous[len(b)]
}

// mcpGateViolation rejects controls the human withheld from agents.
func mcpGateViolation(arguments map[string]any, policy mcpPolicy) string {
	controls, _ := arguments["_flint"].(map[string]any)
	if _, ok := controls["live"]; ok && !policy.allowLive {
		return "_flint.live is not accepted: this MCP server was started without --allow-live, so agents cannot act on live (production) data. Do not retry. If the user wants live access, they must restart the server with flint mcp serve --allow-live."
	}
	if _, ok := controls["confirm"]; ok && !policy.allowDestructive {
		return "_flint.confirm is not accepted: on this MCP server only the user can confirm destructive or live-sensitive operations. Call the tool without _flint.confirm; the server will ask the user when the client supports elicitation. Otherwise the user can restart the server with flint mcp serve --allow-destructive."
	}
	return ""
}

// mcpSelectionSources says where the human set each server selection.
var mcpSelectionSources = map[string]string{"flag": "a command-line flag", "environment": "the environment", "project": "the project's .flint/config.json", "profile": "the profile's saved settings"}

// mcpSelectionViolation keeps an agent's _flint.profile and _flint.merchant
// within the human's choices. An agent may add a merchant guard, or pick a
// profile when the server pins none, but may not replace the profile,
// merchant guard, or context the server resolves (from flags, environment,
// project, or profile configuration). Repeating the server's value is fine.
func (a *App) mcpSelectionViolation(arguments map[string]any, parent Options) string {
	controls, _ := arguments["_flint"].(map[string]any)
	profile, hasProfile := controls["profile"].(string)
	merchant, hasMerchant := controls["merchant"].(string)
	if !hasProfile && !hasMerchant {
		return ""
	}
	base, _, err := a.resolveConfig(parent)
	if err != nil {
		return "_flint.profile and _flint.merchant are not accepted because this MCP server's own Flint configuration does not resolve (" + err.Error() + "). Do not retry; ask the user to fix the server's configuration and restart it."
	}
	candidate := parent
	if hasProfile {
		candidate.Profile = profile
	}
	if hasMerchant {
		candidate.Merchant = merchant
	}
	next, _, err := a.resolveConfig(candidate)
	if err != nil {
		return "" // The command reports the invalid selection itself.
	}
	pinned := func(control, setting, value, source string) string {
		return "_flint." + control + " is not accepted: this MCP server uses " + setting + " " + value + ", set by " + mcpSelectionSources[source] + ", and agents cannot replace it. Do not retry with a different value; omit _flint." + control + ". If the user wants another " + setting + ", they must restart the server with it."
	}
	switch source := base.Sources["profile"]; {
	case hasProfile && next.ProfileName != base.ProfileName && (source == "flag" || source == "environment" || source == "project"):
		return pinned("profile", "profile", base.ProfileName, source)
	case base.MerchantGuard != "" && next.MerchantGuard != base.MerchantGuard:
		control := "merchant"
		if !hasMerchant {
			control = "profile"
		}
		return pinned(control, "merchant guard", base.MerchantGuard, base.Sources["merchant_guard"])
	case base.ContextID != "" && next.ContextID != base.ContextID:
		return pinned("profile", "context", base.ContextID, base.Sources["context"])
	}
	return ""
}

func validateMCPArguments(cmd *Command, arguments map[string]any) error {
	return validateMCPToolArguments(cmd, arguments, mcpPolicy{allowLive: true, allowDestructive: true})
}

func validateMCPToolArguments(cmd *Command, arguments map[string]any, policy mcpPolicy) error {
	schema, schemaErr := schemaForMCPTool(cmd, policy)
	if schemaErr != nil {
		return fmt.Errorf("generate input schema: %s", schemaErr.Message)
	}
	rawSchema, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("encode input schema: %w", err)
	}
	var normalizedSchema any
	if err := json.Unmarshal(rawSchema, &normalizedSchema); err != nil {
		return fmt.Errorf("normalize input schema: %w", err)
	}
	const resourceURL = "https://cli.withflintpay.com/mcp/tool-input.json"
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(resourceURL, normalizedSchema); err != nil {
		return fmt.Errorf("load input schema: %w", err)
	}
	compiled, err := compiler.Compile(resourceURL)
	if err != nil {
		return fmt.Errorf("compile input schema: %w", err)
	}
	if err := compiled.Validate(arguments); err != nil {
		return err
	}
	if err := validateMCPStreamBounds(cmd, arguments); err != nil {
		return err
	}
	return nil
}

// schemaForMCPTool is the input schema advertised under policy: controls the
// human withheld are absent so agents are not invited to use them.
func schemaForMCPTool(cmd *Command, policy mcpPolicy) (map[string]any, *CLIError) {
	schema, err := schemaForMCPCommand(cmd)
	if err != nil {
		return schema, err
	}
	properties, _ := schema["properties"].(map[string]any)
	controlsProperty, _ := properties["_flint"].(map[string]any)
	controls, _ := controlsProperty["properties"].(map[string]any)
	if cmd.CanonicalName == "history" {
		delete(controls, "confirm") // Clearing is the separate history.clear tool.
	}
	if !policy.allowLive {
		delete(controls, "live")
	}
	if !policy.allowDestructive {
		delete(controls, "confirm")
	}
	if controlsProperty != nil && len(controls) == 0 {
		delete(properties, "_flint")
	}
	switch cmd.CanonicalName {
	case "help":
		if topic, ok := properties["topic"].(map[string]any); ok {
			topic["description"] = "A tool name such as customers_create, a group such as customers, or a topic: controls, events, test-cards, exit-codes, errors. Omit for a short index."
		}
	case "schema.input", "schema.output", "schema.command":
		if command, ok := properties["command"].(map[string]any); ok {
			command["description"] = "Tool name, such as customers_create."
		}
	case "schema.commands":
		if prefix, ok := properties["prefix"].(map[string]any); ok {
			prefix["description"] = "Tool group, such as customers or customers_addresses."
		}
	case "schema.events":
		properties["event_type"] = map[string]any{"type": "string", "minLength": 1, "description": "Event type such as payment_intent.succeeded, or a prefix such as payment_intent, to return payload schemas. Omit for a compact list of event types."}
	}
	delete(schema, "$id") // A per-tool URL that no MCP client resolves.
	mcpCompactSchema(schema)
	return schema, nil
}

// mcpCompactSchema drops annotations that cost tokens without informing an
// agent: enum descriptions that only restate their value ("Goods." for goods)
// and Flint's enum bookkeeping keywords.
func mcpCompactSchema(schema map[string]any) {
	restates := func(value any, description any) bool {
		normalize := func(text string) string {
			return strings.Map(func(r rune) rune {
				if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
					return r
				}
				return -1
			}, strings.ToLower(text))
		}
		text, _ := description.(string)
		return normalize(text) == normalize(fmt.Sprint(value))
	}
	walkJSONSchemas(schema, func(node map[string]any) {
		delete(node, "x-flint-enum-kind")
		delete(node, "x-flint-enum-vocabulary")
		descriptions, ok := node["x-enum-descriptions"].([]any)
		if !ok {
			return
		}
		values, _ := node["enum"].([]any)
		informative := false
		kept := make([]any, len(descriptions))
		for i, description := range descriptions {
			if i < len(values) && restates(values[i], description) {
				kept[i] = ""
				continue
			}
			kept[i] = description
			informative = true
		}
		if informative {
			node["x-enum-descriptions"] = kept
		} else {
			delete(node, "x-enum-descriptions")
		}
	})
}

func schemaForMCPCommand(cmd *Command) (map[string]any, *CLIError) {
	schema, schemaErr := schemaForCommand(cmd, true)
	if schemaErr != nil {
		return schema, schemaErr
	}
	// MCP requires an object root even when the public schema is a union.
	schema["type"] = "object"
	if cmd == nil || !cmd.Stream {
		return schema, nil
	}
	properties, _ := schema["properties"].(map[string]any)
	controlsProperty, _ := properties["_flint"].(map[string]any)
	controls, _ := controlsProperty["properties"].(map[string]any)
	controls["max_events"] = map[string]any{
		"type":        "integer",
		"minimum":     1,
		"maximum":     mcpMaxStreamEvents,
		"description": fmt.Sprintf("Stop after at most %d stream records.", mcpMaxStreamEvents),
	}
	controls["for"] = map[string]any{
		"type":        "string",
		"description": "Positive Go duration no longer than 2m.",
	}
	controlsProperty["description"] = "Set max_events (1-100) or for (up to 2m)."
	bound := map[string]any{
		"required": []string{"_flint"},
		"properties": map[string]any{
			"_flint": map[string]any{
				"anyOf": []any{
					map[string]any{"required": []string{"max_events"}},
					map[string]any{"required": []string{"for"}},
				},
			},
		},
	}
	constraint := bound
	if cmd.CanonicalName == "listen" {
		controlsProperty["description"] = "Set max_events (1-100) or for (up to 2m); or print_secret."
		constraint = map[string]any{"anyOf": []any{bound, map[string]any{
			"required":   []string{"print_secret"},
			"properties": map[string]any{"print_secret": map[string]any{"const": true}},
		}}}
	}
	schema["allOf"] = appendSchemaConstraint(schema["allOf"], constraint)
	return schema, nil
}

func appendSchemaConstraint(existing any, constraint map[string]any) []any {
	switch values := existing.(type) {
	case []any:
		return append(values, constraint)
	case nil:
		return []any{constraint}
	default:
		return []any{values, constraint}
	}
}

func validateMCPStreamBounds(cmd *Command, arguments map[string]any) error {
	if cmd == nil || !cmd.Stream {
		return nil
	}
	controls, _ := arguments["_flint"].(map[string]any)
	if raw, ok := controls["for"]; ok {
		duration, err := time.ParseDuration(fmt.Sprint(raw))
		if err != nil || duration <= 0 {
			return fmt.Errorf("_flint.for must be a positive Go duration")
		}
		if duration > mcpMaxStreamDuration {
			return fmt.Errorf("_flint.for must not exceed %s", mcpMaxStreamDuration)
		}
	}
	return nil
}

func mcpToolAnnotations(cmd *Command) map[string]any {
	readOnly := !cmd.Mutation && !cmd.Destructive && !cmd.Sensitive
	// Sensitive operations need confirmation in live mode (refunds, secret
	// rotation, session revocation), so clients should treat them as destructive.
	destructive := cmd.Destructive || cmd.Sensitive
	// Replacing (PUT) or deleting a resource again leaves it in the same state.
	idempotent := readOnly || cmd.Method == "PUT" || cmd.Method == "DELETE"
	openWorld := !cmd.Local
	switch cmd.CanonicalName {
	case "api":
		// The raw tool's method is selected at call time and can be DELETE.
		readOnly = false
		destructive = true
		idempotent = false
	case "history":
		readOnly, destructive, idempotent = true, false, true
	case "history.clear":
		readOnly, destructive, idempotent = false, true, true
	case "listen":
		// Forwards each event with a POST to the local endpoint.
		readOnly, destructive, idempotent, openWorld = false, false, false, true
	case "signup":
		readOnly, idempotent, openWorld = false, false, true
	case "doctor", "config.validate", "init", "help.search":
		// MCP's doctor has no --fix, so these only read.
		openWorld = true
	}
	return map[string]any{
		"readOnlyHint":    readOnly,
		"destructiveHint": destructive,
		"idempotentHint":  idempotent,
		"openWorldHint":   openWorld,
	}
}

// mcpLocalTools describes local tools in MCP terms: their CLI descriptions
// mention flags and terminal behavior that do not apply to agents.
var mcpLocalTools = map[string]struct{ title, description string }{
	"help":            {"Flint MCP help", "Explain one tool (arguments, required fields, controls), list a group's tools, or show a topic: controls, events, test-cards, exit-codes. Omit topic for an index of enabled tools."},
	"help.search":     {"Search Flint Help", "Search Flint Help community answers and support threads with a question."},
	"schema.input":    {"Tool input schema", "JSON Schema of a tool's arguments, the same schema tools/call validates against, including tools this server does not enable."},
	"schema.output":   {"Tool result schema", "JSON Schema of a tool's successful result (structuredContent). Some are large; use _flint.jq to extract parts."},
	"schema.command":  {"Describe a command", "Describe the Flint CLI command behind a tool: API method and path, operation ID, and safety classification."},
	"schema.commands": {"List commands", "List Flint CLI commands, optionally only those in a group such as customers, with their API routes."},
	"schema.errors":   {"Error catalog", "Flint exit codes and error types."},
	"schema.events":   {"Webhook event catalog", "List webhook event types with one-line descriptions. Pass event_type (an exact type, or a prefix such as payment_intent) for their payload schemas."},
	"history":         {"Recent resource IDs", "List Flint resource IDs this CLI recently created or changed with the active credential."},
	"history.clear":   {"Clear resource history", "Delete this CLI's saved list of recent resource IDs. Needs the user's confirmation."},
	"version":         {"CLI version", "Flint CLI version and build, and the public API version and schema hash it targets."},
	"doctor":          {"Check the Flint setup", "Check this server's Flint credential, connectivity, scopes, and API version compatibility."},
	"init":            {"Flint setup status", "Report whether a Flint credential is configured, with setup checks and suggested first steps."},
	"config.get":      {"Resolved configuration", "Show the Flint CLI configuration this server resolves: profile, merchant guard, context, and their sources."},
	"config.validate": {"Validate configuration", "Validate the global and project Flint configuration and the credential they select."},
	"signup":          {"Sign up for Flint", "Create a Flint account and sandbox API key in two calls: first email, first_name, and last_name (Flint emails a code), then verification_code. Only for a profile without a saved credential."},
	"api":             {"Call a Flint API route", "Call any documented Flint public API route by method and /v1 path, with a JSON body for writes. Prefer a dedicated tool when one exists."},
	"listen":          {"Stream webhook events", "Print Flint webhook events or forward them locally with the HMAC listen secret, regardless of endpoint signing methods. JWT handlers need an HMAC path or a local test bypass. Set _flint.max_events or _flint.for (up to 100 events or 2m). print_secret returns the local secret without connecting."},
}

// mcpToolTitle prefers the description's first sentence when it is short and
// free of CLI flags; otherwise it spells out the canonical name.
func mcpToolTitle(cmd *Command) string {
	if local, ok := mcpLocalTools[cmd.CanonicalName]; ok {
		return local.title
	}
	sentence := strings.TrimSpace(cmd.Description)
	if index := strings.IndexAny(sentence, ";\n"); index >= 0 {
		sentence = sentence[:index]
	}
	if index := strings.Index(sentence, ". "); index >= 0 {
		sentence = sentence[:index]
	}
	sentence = strings.TrimSuffix(strings.TrimSpace(sentence), ".")
	if sentence != "" && len(sentence) <= 64 && !strings.Contains(sentence, "--") {
		return sentence
	}
	var words []string
	for _, part := range strings.FieldsFunc(cmd.CanonicalName, func(r rune) bool { return r == '.' || r == '-' }) {
		words = append(words, strings.ToUpper(part[:1])+part[1:])
	}
	return strings.Join(words, " ")
}

// mcpToolDescription is the compact tools/list description: what the tool
// does, then the facts agents otherwise guess at: requirements stated only in
// schema combinators, pagination, and money units.
func mcpToolDescription(cmd *Command, schema map[string]any) string {
	parts := []string{mcpBaseDescription(cmd)}
	if summary := mcpRequirementSummary(schema); summary != "" {
		parts = append(parts, summary)
	}
	if cmd.Supports.Pagination && !cmd.Stream {
		parts = append(parts, "Returns one page: pass next_page_token as page_token for the next (empty on the last page; there is no has_more), or set _flint.all.")
	}
	if mcpUsesMoney(cmd, schema) {
		parts = append(parts, "Money amounts are integer minor units (2500 is 25.00 USD).")
	}
	return strings.Join(parts, " ")
}

// mcpBaseDescription uses the public API's operation description, trimmed to
// its first sentences, when it says more than the CLI's one-line summary.
func mcpBaseDescription(cmd *Command) string {
	switch cmd.CanonicalName {
	case "webhook-endpoints.create":
		return "Create a webhook endpoint. signing_methods defaults to [jwt]; verify Authorization: Bearer <token> with Flint's JWKS. A shared secret is returned only when hmac is included; save it if present."
	case "webhook-endpoints.update":
		return "Update a webhook endpoint. signing_methods replaces the set; omit to leave unchanged. Adding hmac without an issued secret returns 409 WEBHOOK_SECRET_NOT_ISSUED; use rotate-secret to issue one and enable hmac. Removing hmac deletes shared secrets."
	case "webhook-endpoints.rotate-secret":
		return "Rotate a webhook endpoint secret, or issue the first one and enable hmac. Save the returned secret."
	}
	if local, ok := mcpLocalTools[cmd.CanonicalName]; ok {
		return local.description
	}
	description := strings.TrimSpace(cmd.Description)
	if before, after, found := strings.Cut(description, ";"); found && strings.Contains(after, "--") {
		description = strings.TrimSpace(before)
	}
	if operation, ok := openAPIOperationByID(cmd.OperationID); ok {
		if text := mcpLeadingSentences(operation.Description, 200); len(text) > len(description)+1 {
			description = text
		}
	}
	if description != "" && !strings.HasSuffix(description, ".") {
		description += "."
	}
	return description
}

// mcpLeadingSentences returns up to two leading sentences within limit bytes,
// shortening a first sentence that alone is too long.
func mcpLeadingSentences(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	var out string
	for sentences := 0; text != "" && sentences < 2; sentences++ {
		end := len(text)
		for i := 0; i+1 < len(text); i++ {
			if text[i] != '.' || text[i+1] != ' ' {
				continue
			}
			word := text[strings.LastIndexByte(text[:i], ' ')+1 : i]
			if word == "e.g" || word == "i.e" || word == "etc" || word == "vs" {
				continue
			}
			end = i + 1
			break
		}
		sentence := strings.TrimSpace(text[:end])
		if out != "" && len(out)+1+len(sentence) > limit {
			break
		}
		if out == "" && len(sentence) > limit {
			cut := strings.LastIndexByte(sentence[:limit], ' ')
			if cut <= 0 {
				for cut = limit; cut > 0 && !utf8.RuneStart(sentence[cut]); cut-- {
				}
			}
			return strings.TrimRight(sentence[:cut], " ,;:") + "..."
		}
		out = strings.TrimSpace(out + " " + sentence)
		text = strings.TrimSpace(text[end:])
	}
	return out
}

func mcpStrings(value any) []string {
	var out []string
	switch values := value.(type) {
	case []string:
		out = append(out, values...)
	case []any:
		for _, item := range values {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
	}
	return out
}

// mcpRequirementSummary spells out requirements a schema states only through
// oneOf, anyOf, allOf, if/then, not, or dependentRequired, which clients and
// models often ignore. It returns "" when there are none.
func mcpRequirementSummary(schema map[string]any) string {
	defs, _ := schema["$defs"].(map[string]any)
	resolve := func(node any) map[string]any {
		m, _ := node.(map[string]any)
		if ref, ok := m["$ref"].(string); ok {
			m, _ = defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
		}
		return m
	}
	rootRequired := map[string]bool{}
	for _, name := range mcpStrings(schema["required"]) {
		rootRequired[name] = true
	}
	var sentences []string
	needs := map[string][]string{} // "expected_version" -> fields that need it
	var needOrder []string
	need := func(triggers, needed []string) {
		key := strings.Join(needed, " and ")
		if _, seen := needs[key]; !seen {
			needOrder = append(needOrder, key)
		}
		needs[key] = append(needs[key], triggers...)
	}
	// conditional recognizes {anyOf: [{not: {required: X}}, {required: Y}]}
	// and {if: {required: X}, then: {required: Y}}: sending X needs Y.
	conditional := func(node map[string]any) bool {
		if branches, ok := node["anyOf"].([]any); ok && len(branches) == 2 {
			first, second := resolve(branches[0]), resolve(branches[1])
			not, _ := first["not"].(map[string]any)
			triggers := mcpStrings(not["required"])
			var needed []string
			for _, name := range mcpStrings(second["required"]) {
				if !containsString(triggers, name) {
					needed = append(needed, name)
				}
			}
			if len(first) == 1 && len(not) == 1 && len(triggers) > 0 && len(needed) > 0 {
				need(triggers, needed)
				return true
			}
		}
		condition, _ := node["if"].(map[string]any)
		then, _ := node["then"].(map[string]any)
		if triggers, needed := mcpStrings(condition["required"]), mcpStrings(then["required"]); len(triggers) > 0 && len(needed) > 0 {
			need(triggers, needed)
			return true
		}
		return false
	}
	alternatives := func(branches []any, exclusive bool) string {
		type variant struct {
			required []string
			values   map[string][]string // property -> const or enum values
		}
		var variants []variant
		for _, raw := range branches {
			branch := resolve(raw)
			v := variant{values: map[string][]string{}}
			members := []map[string]any{branch}
			for _, member := range mcpAnySlice(branch["allOf"]) {
				members = append(members, resolve(member))
			}
			for _, member := range members {
				for _, name := range mcpStrings(member["required"]) {
					if !rootRequired[name] && !containsString(v.required, name) {
						v.required = append(v.required, name)
					}
				}
			}
			properties, _ := branch["properties"].(map[string]any)
			for name, rawProperty := range properties {
				property, _ := rawProperty.(map[string]any)
				if value, ok := property["const"]; ok {
					v.values[name] = []string{fmt.Sprint(value)}
				} else if enum, ok := property["enum"].([]any); ok && len(enum) > 0 {
					for _, value := range enum {
						v.values[name] = append(v.values[name], fmt.Sprint(value))
					}
				}
			}
			variants = append(variants, v)
		}
		if len(variants) < 2 {
			return ""
		}
		// A property every variant fixes to its own values selects the variant.
		var discriminators []string
		for name := range variants[0].values {
			shared := true
			for _, v := range variants[1:] {
				if _, ok := v.values[name]; !ok {
					shared = false
				}
			}
			// The values must tell the variants apart.
			seen := map[string]bool{}
			for _, v := range variants {
				for _, value := range v.values[name] {
					if seen[value] {
						shared = false
					}
					seen[value] = true
				}
			}
			if shared {
				discriminators = append(discriminators, name)
			}
		}
		sort.Strings(discriminators)
		if len(discriminators) > 0 {
			name := discriminators[0]
			var values, details []string
			for _, v := range variants {
				values = append(values, v.values[name]...)
				var rest []string
				for _, field := range v.required {
					if field != name {
						rest = append(rest, field)
					}
				}
				if len(rest) > 0 {
					details = append(details, name+" "+strings.Join(v.values[name], "/")+", send "+strings.Join(rest, ", "))
				}
			}
			sentence := "Set " + name + " to one of: " + strings.Join(values, ", ") + "."
			if len(details) > 0 && len(variants) <= 4 {
				sentence += " With " + strings.Join(details, "; with ") + "."
			} else if len(details) > 0 {
				sentence += " Each has its own required fields; see help."
			}
			return sentence
		}
		var groups []string
		empty := 0
		// Variants that one required set cannot tell apart (equal or nested
		// sets) differ in ways a one-line summary cannot state.
		for i, v := range variants {
			for j, other := range variants {
				if i == j || len(v.required) == 0 {
					continue
				}
				nested := true
				for _, field := range v.required {
					if !containsString(other.required, field) {
						nested = false
					}
				}
				if nested {
					return fmt.Sprintf("Arguments must match one of %d variants; see schema_input.", len(variants))
				}
			}
		}
		for _, v := range variants {
			switch len(v.required) {
			case 0:
				empty++
			case 1:
				groups = append(groups, v.required[0])
			default:
				groups = append(groups, "{"+strings.Join(v.required, ", ")+"}")
			}
		}
		switch {
		case empty == 0 && exclusive:
			return "Provide exactly one of: " + strings.Join(groups, "; ") + "."
		case empty == 0:
			return "Provide at least one of: " + strings.Join(groups, "; ") + "."
		case empty == 1 && exclusive && len(groups) > 0:
			return "Provide at most one of: " + strings.Join(groups, "; ") + "."
		case exclusive:
			return fmt.Sprintf("Arguments must match one of %d variants; see schema_input.", len(variants))
		}
		return ""
	}
	if branches, ok := schema["oneOf"].([]any); ok {
		if sentence := alternatives(branches, true); sentence != "" {
			sentences = append(sentences, sentence)
		}
	}
	if branches, ok := schema["anyOf"].([]any); ok && !conditional(schema) {
		if sentence := alternatives(branches, false); sentence != "" {
			sentences = append(sentences, sentence)
		}
	}
	conditional(map[string]any{"if": schema["if"], "then": schema["then"]})
	for _, raw := range mcpAnySlice(schema["allOf"]) {
		member := resolve(raw)
		if conditional(member) {
			continue
		}
		// Streaming tools require one of several _flint bounds.
		properties, _ := member["properties"].(map[string]any)
		for name, rawProperty := range properties {
			property, _ := rawProperty.(map[string]any)
			var options []string
			for _, branch := range mcpAnySlice(property["anyOf"]) {
				if required := mcpStrings(resolve(branch)["required"]); len(required) == 1 {
					options = append(options, name+"."+required[0])
				}
			}
			if len(options) > 1 && containsString(mcpStrings(member["required"]), name) {
				sentences = append(sentences, "Set "+strings.Join(options, " or ")+".")
			}
		}
	}
	if dependent, ok := schema["dependentRequired"].(map[string]any); ok {
		var triggers []string
		for trigger := range dependent {
			triggers = append(triggers, trigger)
		}
		sort.Strings(triggers)
		for _, trigger := range triggers {
			need([]string{trigger}, mcpStrings(dependent[trigger]))
		}
	}
	for _, key := range needOrder {
		triggers := needs[key]
		list := strings.Join(triggers, ", ")
		if len(triggers) > 1 {
			list = strings.Join(triggers[:len(triggers)-1], ", ") + ", or " + triggers[len(triggers)-1]
		}
		verb := " is "
		if strings.Contains(key, " and ") {
			verb = " are "
		}
		sentences = append(sentences, key+verb+"required when sending "+list+".")
	}
	// {not: {required: [S], anyOf: [...]}} means S must be sent alone.
	if not, ok := schema["not"].(map[string]any); ok {
		if alone := mcpStrings(not["required"]); len(alone) == 1 {
			if _, combined := not["anyOf"]; combined {
				sentences = append(sentences, "Send "+alone[0]+" without other fields.")
			} else {
				sentences = append(sentences, "Do not send "+alone[0]+".")
			}
		}
	}
	summary := strings.Join(sentences, " ")
	if len(summary) > 200 {
		cut := strings.LastIndex(summary[:190], ", ")
		if cut <= 0 {
			cut = 190
		}
		summary = summary[:cut] + "... (see help)"
	}
	return summary
}

func mcpAnySlice(value any) []any {
	values, _ := value.([]any)
	return values
}

var (
	mcpComponentGraphOnce sync.Once
	mcpComponentGraph     map[string]map[string]bool
)

// mcpComponentReferences maps each public schema component to the components
// it references directly.
func mcpComponentReferences() map[string]map[string]bool {
	mcpComponentGraphOnce.Do(func() {
		mcpComponentGraph = map[string]map[string]bool{}
		doc, err := loadOpenAPI()
		if err != nil {
			return
		}
		for name, schema := range doc.Components.Schemas {
			mcpComponentGraph[name] = map[string]bool{}
			collectComponentRefs(schema, mcpComponentGraph[name])
		}
	})
	return mcpComponentGraph
}

// mcpReachesMoney reports whether Money is within depth references of the
// named components. A shallow bound keeps the money note to tools whose own
// fields are amounts, not ones that merely link to a priced resource.
func mcpReachesMoney(names []string, depth int) bool {
	graph := mcpComponentReferences()
	seen := map[string]bool{}
	for level := 0; level <= depth && len(names) > 0; level++ {
		var next []string
		for _, name := range names {
			if name == "Money" {
				return true
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			for target := range graph[name] {
				next = append(next, target)
			}
		}
		names = next
	}
	return false
}

// mcpUsesMoney reports whether a tool's arguments or result carry Money
// amounts within two levels of nesting.
func mcpUsesMoney(cmd *Command, schema map[string]any) bool {
	root := map[string]any{}
	for key, value := range schema {
		if key != "$defs" {
			root[key] = value
		}
	}
	var direct []string
	walkJSONSchemas(root, func(node map[string]any) {
		if ref, ok := node["$ref"].(string); ok {
			direct = append(direct, strings.TrimPrefix(ref, "#/$defs/"))
		}
	})
	if mcpReachesMoney(direct, 1) {
		return true
	}
	return cmd.OutputSchema != "" && mcpReachesMoney([]string{cmd.OutputSchema}, 2)
}

func mcpHelpIndex(all []*Command, policy mcpPolicy) map[string]any {
	groups := map[string]int{}
	var disabled []string
	for _, tool := range all {
		group, _, _ := strings.Cut(tool.CanonicalName, ".")
		if policy.includes(tool.CanonicalName) {
			groups[group]++
		}
	}
	for _, group := range mcpToolGroups(all) {
		if groups[group] == 0 {
			disabled = append(disabled, group)
		}
	}
	findingTools := []string{
		"Call help with topic set to a tool name for its arguments and required fields, or to a group such as customers for its tools.",
		"Call schema_input or schema_output with command set to a tool name for its full JSON input or result schema.",
	}
	if policy.includes("help.search") {
		findingTools = append(findingTools, "Call help_search with a question to search Flint Help community answers.")
	}
	findingTools = append(findingTools, "tools/list returns every enabled tool. Tools in other_groups need the user to restart the server with --tools all, or --tools default,<group>.")
	data := map[string]any{
		"summary":    "Index of Flint MCP help. Tools are named <resource>_<action>; resource_groups lists each enabled group with its number of tools.",
		"tool_set":   policy.toolSetDescription(),
		"tool_count": len(mcpFilterTools(all, policy)),
		"topics": []map[string]string{
			{"topic": "<tool name>", "description": "Arguments, required fields, and controls for one tool, for example customers_create."},
			{"topic": "<group>", "description": "The tools in one group, for example customers."},
			{"topic": "controls", "description": "What each _flint execution control does."},
			{"topic": "events", "description": "Webhook event types with one-line descriptions; schema_events with event_type returns payload schemas."},
			{"topic": "test-cards", "description": "Sandbox payment method tokens and card numbers for test scenarios."},
			{"topic": "exit-codes", "description": "Exit codes and error types returned by Flint commands."},
			{"topic": "errors", "description": "Same catalog as exit-codes."},
		},
		"finding_tools":   findingTools,
		"resource_groups": groups,
	}
	if len(disabled) > 0 {
		data["other_groups"] = disabled
	}
	return map[string]any{"data": data}
}

func mcpFilterTools(all []*Command, policy mcpPolicy) []*Command {
	var tools []*Command
	for _, tool := range all {
		if policy.includes(tool.CanonicalName) {
			tools = append(tools, tool)
		}
	}
	return tools
}

// mcpSchemaType is a short type label for a property in help output.
func mcpSchemaType(property map[string]any) string {
	switch value := property["type"].(type) {
	case string:
		if items, ok := property["items"].(map[string]any); ok && value == "array" {
			return "array of " + mcpSchemaType(items)
		}
		return value
	case []any:
		return strings.Join(mcpStrings(value), " or ")
	}
	if ref, ok := property["$ref"].(string); ok {
		return strings.TrimPrefix(ref, "#/$defs/")
	}
	if _, ok := property["const"]; ok {
		return "const"
	}
	for _, keyword := range []string{"anyOf", "oneOf", "allOf"} {
		var labels []string
		for _, branch := range mcpAnySlice(property[keyword]) {
			if branchMap, ok := branch.(map[string]any); ok {
				if label := mcpSchemaType(branchMap); label != "any" && !containsString(labels, label) {
					labels = append(labels, label)
				}
			}
		}
		if len(labels) > 0 {
			return strings.Join(labels, " or ")
		}
	}
	return "any"
}

// mcpToolHelp is help for one tool in MCP terms, consistent with its input
// schema: argument names, types, and required fields come from that schema.
func mcpToolHelp(cmd *Command, policy mcpPolicy) (map[string]any, *CLIError) {
	schema, e := schemaForMCPTool(cmd, policy)
	if e != nil {
		return nil, e
	}
	name := mcpToolName(cmd.CanonicalName)
	properties, _ := schema["properties"].(map[string]any)
	required := mcpStrings(schema["required"])
	var names []string
	for property := range properties {
		if property != "_flint" {
			names = append(names, property)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		left, right := containsString(required, names[i]), containsString(required, names[j])
		if left != right {
			return left
		}
		return names[i] < names[j]
	})
	arguments := []map[string]any{}
	for _, property := range names {
		definition, _ := properties[property].(map[string]any)
		entry := map[string]any{"name": property, "type": mcpSchemaType(definition), "required": containsString(required, property)}
		for _, keyword := range []string{"description", "enum", "default"} {
			if value, ok := definition[keyword]; ok {
				entry[keyword] = value
			}
		}
		arguments = append(arguments, entry)
	}
	controls := []map[string]any{}
	controlSchemas, _ := lookupPath(schema, "properties._flint.properties")
	controlMap, _ := controlSchemas.(map[string]any)
	var controlNames []string
	for control := range controlMap {
		controlNames = append(controlNames, control)
	}
	sort.Strings(controlNames)
	for _, control := range controlNames {
		definition, _ := controlMap[control].(map[string]any)
		controls = append(controls, map[string]any{"name": "_flint." + control, "description": definition["description"]})
	}
	data := map[string]any{
		"name":           name,
		"canonical_name": cmd.CanonicalName,
		"title":          mcpToolTitle(cmd),
		"description":    mcpToolDescription(cmd, schema),
		"enabled":        policy.includes(cmd.CanonicalName),
		"annotations":    mcpToolAnnotations(cmd),
		"required":       required,
		"arguments":      arguments,
		"controls":       controls,
		"schemas":        "Call schema_input with command " + name + " for the full input JSON Schema, including nested fields, and schema_output for the result schema.",
	}
	if data["required"] == nil {
		data["required"] = []string{}
	}
	if summary := mcpRequirementSummary(schema); summary != "" {
		data["requirements"] = summary
	}
	if !policy.includes(cmd.CanonicalName) {
		group, _, _ := strings.Cut(cmd.CanonicalName, ".")
		data["note"] = "This tool is not enabled on this server. The user can enable it by restarting the server with --tools all, or with " + group + " added to --tools."
	}
	return map[string]any{"data": data}, nil
}

// mcpControlHelp documents each _flint control in full; tool schemas carry
// only a one-line description.
var mcpControlHelp = []map[string]string{
	{"name": "_flint.dry_run", "applies_to": "writes", "description": "\"client\" validates the arguments and returns the prepared request (method, path, headers, body) without sending it. Nothing changes in Flint."},
	{"name": "_flint.preview", "applies_to": "destructive writes", "description": "Returns the request and its effect (permission check, reversibility, affected resources) without making the change."},
	{"name": "_flint.idempotency_key", "applies_to": "writes", "description": "Sent as the Idempotency-Key header. Flint returns the original result when a key is reused, so reuse the same key when retrying a write that timed out or failed on the network; a new key could run it twice. The CLI generates a key only when the route makes it optional. Required routes need a key saved before the first call; operations that take no key send none. The API never generates keys. Every error from a keyed write includes its key."},
	{"name": "_flint.confirm", "applies_to": "destructive and live-sensitive operations, only when the server was started with --allow-destructive", "description": "Confirms the operation. Without --allow-destructive the server asks the user through elicitation, or refuses when the client cannot ask."},
	{"name": "_flint.live", "applies_to": "API tools, only when the server was started with --allow-live", "description": "Acknowledges a live (production) credential or context. Without --allow-live the server refuses live credentials."},
	{"name": "_flint.merchant", "applies_to": "API tools", "description": "Fails the call unless the credential belongs to this mer_ merchant. An agent may add a guard but cannot replace one the server already uses."},
	{"name": "_flint.profile", "applies_to": "API tools", "description": "Selects a saved Flint CLI profile, unless the server pins one with --profile, FLINT_PROFILE, or project configuration, or the profile would change the server's merchant guard or context."},
	{"name": "_flint.timeout", "applies_to": "tools that call Flint", "description": "Per-request timeout as a Go duration such as 30s."},
	{"name": "_flint.all", "applies_to": "paginated lists", "description": "Fetches every page and returns them merged as one list with an empty next_page_token. Large lists can exceed the result limit; combine with _flint.select."},
	{"name": "_flint.wait_for", "applies_to": "get tools", "description": "Polls until every FIELD=VALUE condition holds, for example status=succeeded, bounded by _flint.for (default 2m)."},
	{"name": "_flint.for", "applies_to": "waits and streams", "description": "Maximum duration of a wait or stream as a Go duration; streams allow at most 2m."},
	{"name": "_flint.max_events", "applies_to": "streams", "description": "Stops a stream after this many records (at most 100)."},
	{"name": "_flint.expand", "applies_to": "tools with documented expansions", "description": "Includes related resources inline; the schema lists the allowed values."},
	{"name": "_flint.field", "applies_to": "most tools", "description": "Returns only the value at a dot path such as data.id, as {\"value\": ...}."},
	{"name": "_flint.select", "applies_to": "most tools", "description": "Keeps only the listed fields of each resource, keeping the response envelope."},
	{"name": "_flint.jq", "applies_to": "most tools", "description": "Applies a jq expression to the result, returned as {\"value\": ...}. Cannot be combined with _flint.field."},
}

// mcpEventList is the compact webhook catalog: every event type with its
// one-line summary, plus how deliveries are signed and retried.
func mcpEventList() (map[string]any, *CLIError) {
	summaries, err := eventTypeSummaries()
	doc, docErr := loadOpenAPI()
	if err != nil || docErr != nil {
		return nil, cliError(ExitUsage, "internal_error", "SCHEMA_SNAPSHOT_INVALID", "The embedded webhook catalog could not be read.")
	}
	data := map[string]any{
		"event_types": summaries,
		"count":       len(summaries),
		"next":        "Call schema_events with event_type set to an event type, or a prefix such as payment_intent, for payload schemas.",
	}
	for _, key := range []string{"signing", "retry_policy", "secret_rotation"} {
		if value, ok := doc.WebhookMetadata[key]; ok {
			data[key] = value
		}
	}
	return map[string]any{"data": data}, nil
}

// mcpEventSchemas is the webhook catalog for one event type or prefix.
func mcpEventSchemas(filter string) (any, *CLIError) {
	filter = strings.TrimSuffix(strings.TrimSpace(filter), ".")
	match := func(eventType string) bool { return eventType == filter || strings.HasPrefix(eventType, filter+".") }
	catalog, err := eventCatalogFor(match)
	if err != nil {
		return nil, cliError(ExitUsage, "internal_error", "SCHEMA_SNAPSHOT_INVALID", err.Error())
	}
	if webhooks, _ := catalog["webhooks"].(map[string]any); len(webhooks) > 0 {
		return catalog, nil
	}
	summaries, _ := eventTypeSummaries()
	type candidate struct {
		name     string
		distance int
	}
	var candidates []candidate
	for _, summary := range summaries {
		name := summary["event_type"]
		group, _, _ := strings.Cut(name, ".")
		distance := min(levenshteinDistance(filter, name), levenshteinDistance(filter, group))
		if strings.Contains(name, filter) {
			distance = 0
		}
		if distance <= max(2, len(filter)/3) {
			candidates = append(candidates, candidate{name, distance})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].distance < candidates[j].distance })
	var suggestions []string
	for _, c := range candidates[:min(3, len(candidates))] {
		suggestions = append(suggestions, c.name)
	}
	message := "No webhook event type matches " + filter + "."
	if len(suggestions) > 0 {
		message += " Did you mean " + strings.Join(suggestions, ", ") + "?"
	}
	e := usageError("UNKNOWN_EVENT_TYPE", message+" Call schema_events without event_type to list event types.", "event_type")
	e.Details = []map[string]any{{"input": filter, "suggestions": suggestions}}
	return nil, e
}

// mcpCanonicalArguments maps tool names given to help and the schema tools,
// such as customers_create, to the command names the CLI resolves.
func mcpCanonicalArguments(cmd *Command, arguments map[string]any) map[string]any {
	key := map[string]string{"help": "topic", "schema.input": "command", "schema.output": "command", "schema.command": "command", "schema.commands": "prefix"}[cmd.CanonicalName]
	value, ok := arguments[key].(string)
	if !ok || !strings.Contains(value, "_") {
		return arguments
	}
	out := copyMap(arguments)
	out[key] = mcpCanonicalToolName(value)
	return out
}

// mcpTopicTool resolves a help or schema topic to an MCP tool.
func (a *App) mcpTopicTool(topic string) (*Command, bool) {
	canonical := strings.TrimSpace(topic)
	if alias, ok := a.Registry.ByName(canonical); ok {
		canonical = alias.CanonicalName
	}
	for _, tool := range a.mcpAllTools() {
		if tool.CanonicalName == canonical {
			return tool, true
		}
	}
	return nil, false
}

// mcpLocalToolResult answers the help and schema tools in MCP terms where the
// CLI's answer would describe flags and files, and gates signup. It reports
// false when the CLI command should run as usual.
func (a *App) mcpLocalToolResult(ctx context.Context, cmd *Command, arguments map[string]any, opts Options, policy mcpPolicy, structured bool) (map[string]any, bool) {
	finish := func(value any, e *CLIError) (map[string]any, bool) {
		if e != nil {
			return mcpCommandError(cmd, mcpCallResult{value: mcpCLIErrorEnvelope(e), exit: e.ExitCode}, policy, ""), true
		}
		transformOptions, transformed, e := mcpOutputTransformOptions(cmd, arguments)
		if e == nil && transformed {
			// Transforms run on decoded JSON, as they do for CLI output.
			raw, _ := json.Marshal(value)
			var decoded any
			_ = decodeJSONNumbers(raw, &decoded)
			transformOptions.outputLimit = mcpMaxOutputBytes
			value, e = applyOutputTransforms(ctx, decoded, transformOptions)
		}
		if e != nil {
			return mcpCommandError(cmd, mcpCallResult{value: mcpCLIErrorEnvelope(e), exit: e.ExitCode}, policy, ""), true
		}
		if refused := mcpResultTooLarge(cmd, arguments, value); refused != nil {
			return refused, true
		}
		return mcpToolResult(value, structured, transformed, ""), true
	}
	switch cmd.CanonicalName {
	case "help":
		topic, _ := arguments["topic"].(string)
		topic = strings.TrimSpace(topic)
		all := a.mcpAllTools()
		switch topic {
		case "":
			return finish(mcpHelpIndex(all, policy), nil)
		case "controls":
			return finish(map[string]any{"data": map[string]any{"controls": mcpControlHelp, "usage": "Pass controls inside the _flint object argument, for example {\"_flint\": {\"dry_run\": \"client\"}}. Each tool's schema lists the controls it accepts."}}, nil)
		case "events":
			return finish(mcpEventList())
		}
		if tool, ok := a.mcpTopicTool(topic); ok {
			return finish(mcpToolHelp(tool, policy))
		}
		var group []map[string]any
		for _, tool := range all {
			if strings.HasPrefix(tool.CanonicalName, topic+".") {
				group = append(group, map[string]any{"name": mcpToolName(tool.CanonicalName), "title": mcpToolTitle(tool), "enabled": policy.includes(tool.CanonicalName)})
			}
		}
		if len(group) > 0 {
			return finish(map[string]any{"data": map[string]any{"group": mcpToolName(topic), "tools": group, "next": "Call help with a tool name for its arguments."}}, nil)
		}
	case "schema.input":
		command, _ := arguments["command"].(string)
		if tool, ok := a.mcpTopicTool(command); ok {
			schema, e := schemaForMCPTool(tool, policy)
			return finish(schema, e)
		}
	case "schema.events":
		if filter, ok := arguments["event_type"].(string); ok {
			return finish(mcpEventSchemas(filter))
		}
		return finish(mcpEventList())
	case "signup":
		if message := a.mcpSignupViolation(opts); message != "" {
			return mcpToolError(message, nil), true
		}
	}
	return nil, false
}

// mcpSignupViolation refuses signup when it would replace the credential the
// human already saved for the server's profile.
func (a *App) mcpSignupViolation(opts Options) string {
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		return "" // Signup reports the configuration problem itself.
	}
	existing, err := a.LoadCredential(resolved.ProfileName)
	if err != nil || strings.TrimSpace(existing) == "" {
		return ""
	}
	return "signup is not available here: profile " + resolved.ProfileName + " already has a saved Flint credential, and signup would replace it with a new account's key. Do not retry. The user already has a Flint login; if they want a new account, they can run flint signup in a terminal."
}

type mcpCallResult struct {
	value       any
	transformed bool
	exit        int
	environment string
	liveBlocked bool
	warnings    string // stderr of a successful command, minus progress and debug lines
}

// mcpResultTooLarge refuses an oversized read result with advice to narrow
// it. Writes are never refused here: their result reports a completed change.
func mcpResultTooLarge(cmd *Command, args map[string]any, value any) map[string]any {
	method, _ := args["method"].(string)
	if cmd.Mutation || (cmd.CanonicalName == "api" && !strings.EqualFold(method, http.MethodGet)) {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil || len(raw) <= mcpMaxResultBytes {
		return nil
	}
	message := fmt.Sprintf("The result is %d bytes, over this server's %d byte limit for one tool result, so it was not returned. Narrow it and call again: keep only the fields you need with _flint.select or _flint.jq, use page_size and page_token instead of _flint.all, or add filter arguments (schema_events takes event_type).", len(raw), mcpMaxResultBytes)
	return mcpToolError(message, map[string]any{"error": map[string]any{"type": "usage_error", "code": "RESULT_TOO_LARGE", "message": message, "details": map[string]any{"bytes": len(raw), "limit": mcpMaxResultBytes}}})
}

// mcpWithWarnings adds CLI warnings to a successful result as their own text
// block, after the result and the environment line.
func mcpWithWarnings(result map[string]any, warnings string) map[string]any {
	if warnings = strings.TrimSpace(warnings); warnings != "" {
		content, _ := result["content"].([]map[string]any)
		result["content"] = append(content, map[string]any{"type": "text", "text": "Warnings:\n" + warnings})
	}
	return result
}

// runMCPTool runs a tool, applies the live and confirmation gates, and turns
// the outcome into an MCP tool result.
func (a *App) runMCPTool(ctx context.Context, cmd *Command, args map[string]any, opts Options, policy mcpPolicy, structured bool) map[string]any {
	var extra []string
	if cmd.CanonicalName == "history.clear" {
		extra = append(extra, "--clear=true")
	}
	outcome := a.runMCPCommand(ctx, cmd, args, opts, policy, extra)
	if outcome.exit != ExitOK && !outcome.liveBlocked && !policy.allowDestructive && mcpConfirmationRequired(outcome.value) {
		session := mcpSessionFrom(ctx)
		if !session.canElicit() {
			return mcpCommandError(cmd, outcome, policy, mcpConfirmationUnavailable(cmd, mcpErrorText(outcome.value)))
		}
		approved, message := a.mcpAskConfirmation(ctx, session, cmd, args, outcome)
		if !approved {
			return mcpToolError(message, map[string]any{"error": map[string]any{"type": "confirmation_required", "code": "CONFIRMATION_DECLINED", "message": message}})
		}
		outcome = a.runMCPCommand(ctx, cmd, args, opts, policy, append(extra, "--confirm=true"))
	}
	if outcome.exit != ExitOK {
		return mcpCommandError(cmd, outcome, policy, "")
	}
	if refused := mcpResultTooLarge(cmd, args, outcome.value); refused != nil {
		return refused
	}
	return mcpWithWarnings(mcpToolResult(outcome.value, structured, outcome.transformed, outcome.environment), outcome.warnings)
}

func mcpErrorField(value any, field string) string {
	envelope, _ := value.(map[string]any)
	switch e := envelope["error"].(type) {
	case map[string]any:
		text, _ := e[field].(string)
		return text
	case string:
		if field == "message" {
			return e
		}
	}
	if field == "message" {
		if stderr, _ := envelope["stderr"].(string); stderr != "" {
			return stderr
		}
	}
	return ""
}

func mcpErrorText(value any) string { return mcpErrorField(value, "message") }

func mcpConfirmationRequired(value any) bool {
	code := mcpErrorField(value, "code")
	return code == "CONFIRMATION_REQUIRED" || code == "PRODUCTION_CONFIRMATION_REQUIRED" || code == "UNVERIFIED_ENVIRONMENT_CONFIRMATION_REQUIRED"
}

func mcpConfirmationUnavailable(cmd *Command, cliMessage string) string {
	message := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(cliMessage), "Re-run with --confirm."))
	if message == "" {
		message = "This operation requires confirmation."
	}
	return message + " Only the user can confirm it on this MCP server, and this MCP client does not support elicitation, so the server cannot ask them. Do not retry. Ask the user to run it themselves with the flint CLI (flint " + strings.Join(cmd.Path[1:], " ") + " ... --confirm) or to restart the server with flint mcp serve --allow-destructive. Use _flint.preview or _flint.dry_run, where available, to show them the effect first."
}

func (a *App) mcpAskConfirmation(ctx context.Context, session *mcpSession, cmd *Command, args map[string]any, outcome mcpCallResult) (bool, string) {
	environment := outcome.environment
	if environment == "" {
		environment = "unknown"
	}
	cliMessage := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(mcpErrorText(outcome.value)), "Re-run with --confirm."))
	rawArgs, _ := json.Marshal(args)
	if len(rawArgs) > 2000 {
		rawArgs = append(rawArgs[:2000], []byte("...")...)
	}
	prompt := fmt.Sprintf("An AI agent wants to run the Flint tool %s (environment: %s). %s\n\nArguments: %s\n\nAllow this operation?", mcpToolName(cmd.CanonicalName), environment, cliMessage, rawArgs)
	requestCtx, cancel := context.WithTimeout(ctx, mcpConfirmationDeadline)
	defer cancel()
	response, err := session.request(requestCtx, "elicitation/create", map[string]any{
		"message": prompt,
		"requestedSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"confirm": map[string]any{"type": "boolean", "title": "Allow " + mcpToolName(cmd.CanonicalName), "description": "Approve this " + environment + " operation.", "default": false},
			},
			"required": []string{"confirm"},
		},
	})
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return false, "The user did not answer the confirmation request in time; the operation was not performed."
		}
		return false, "Could not ask the user to confirm this operation (" + err.Error() + "); it was not performed."
	}
	if rpcErr, ok := response["error"].(map[string]any); ok {
		message, _ := rpcErr["message"].(string)
		return false, "The MCP client could not ask the user to confirm this operation (" + message + "); it was not performed. The user can run it with the flint CLI or restart the server with --allow-destructive."
	}
	result, _ := response["result"].(map[string]any)
	action, _ := result["action"].(string)
	content, _ := result["content"].(map[string]any)
	if action == "accept" && content["confirm"] == true {
		return true, ""
	}
	return false, "The user declined to confirm " + mcpToolName(cmd.CanonicalName) + "; the operation was not performed. Do not retry unless the user asks."
}

// mcpEnvironmentObserver sees the child command's HTTP traffic. It records the
// credential environment from the auth-context check and, unless live access
// was allowed, refuses every later authenticated request on a live credential.
type mcpEnvironmentObserver struct {
	base        http.RoundTripper
	allowLive   bool
	cancel      context.CancelFunc
	mu          sync.Mutex
	environment string
	blocked     bool
}

var errMCPLiveBlocked = errors.New("live access is disabled on this MCP server")

type mcpReplayBody struct {
	io.Reader
	io.Closer
}

func (o *mcpEnvironmentObserver) RoundTrip(req *http.Request) (*http.Response, error) {
	authContext := req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/v1/developer/auth-context")
	o.mu.Lock()
	if o.environment == "live" && !o.allowLive && !authContext && req.Header.Get("Authorization") != "" {
		o.blocked = true
		o.mu.Unlock()
		if req.Body != nil {
			_ = req.Body.Close()
		}
		if o.cancel != nil {
			o.cancel()
		}
		return nil, errMCPLiveBlocked
	}
	o.mu.Unlock()
	resp, err := o.base.RoundTrip(req)
	if err != nil || !authContext || resp.StatusCode != http.StatusOK {
		return resp, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		_ = resp.Body.Close()
		return nil, readErr
	}
	resp.Body = mcpReplayBody{Reader: io.MultiReader(bytes.NewReader(raw), resp.Body), Closer: resp.Body}
	var envelope struct {
		Data struct {
			Environment string `json:"environment"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Data.Environment != "" {
		o.mu.Lock()
		o.environment = normalizeEnvironment(envelope.Data.Environment)
		o.mu.Unlock()
	}
	return resp, nil
}

func (o *mcpEnvironmentObserver) state() (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.environment, o.blocked
}

func (a *App) mcpObservedHTTPClient(observer *mcpEnvironmentObserver) *http.Client {
	var client http.Client
	if a.HTTPClient != nil {
		client = *a.HTTPClient
	} else {
		// Keep the shared pool. A nil redirect policy lets each caller apply
		// its own: API requests reject redirects, help searches follow them.
		client = *defaultAPIHTTPClient
		client.CheckRedirect = nil
	}
	observer.base = client.Transport
	if observer.base == nil {
		observer.base = http.DefaultTransport
	}
	client.Transport = observer
	return &client
}

type mcpProgressKey struct{}

// mcpProgress sends notifications/progress for a tools/call whose request
// carried a progressToken.
type mcpProgress struct {
	session  *mcpSession
	token    any
	messages bool // Progress messages arrived in protocol 2025-03-26.
	mu       sync.Mutex
	count    int
}

func mcpProgressFrom(ctx context.Context) *mcpProgress {
	progress, _ := ctx.Value(mcpProgressKey{}).(*mcpProgress)
	return progress
}

func (p *mcpProgress) notify(message string) {
	if p == nil {
		return
	}
	// Holding the lock while writing keeps progress values in order.
	p.mu.Lock()
	defer p.mu.Unlock()
	p.count++
	params := map[string]any{"progressToken": p.token, "progress": p.count}
	if p.messages && message != "" {
		params["message"] = message
	}
	_ = p.session.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": params})
}

// mcpProgressMessage describes one of the CLI's --progress=json records.
func mcpProgressMessage(record map[string]any) (string, bool) {
	switch record["type"] {
	case "pagination":
		if resources, ok := record["resources"]; ok {
			return fmt.Sprintf("Fetched page %v (%v resources)", record["pages"], resources), true
		}
		return fmt.Sprintf("Fetched page %v", record["pages"]), true
	case "wait":
		return "Waiting for " + strings.Join(mcpStrings(record["conditions"]), ", "), true
	case "upgrade":
		return fmt.Sprint(record["stage"]), true
	}
	return "", false
}

// mcpStderr captures a child command's stderr line by line. With progress,
// the CLI's JSON progress records become notifications; debug lines go to the
// server's own stderr; everything else is kept as warnings for the agent.
type mcpStderr struct {
	mu       sync.Mutex
	kept     mcpCappedBuffer
	partial  []byte
	progress *mcpProgress
	debug    io.Writer
}

func (w *mcpStderr) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.partial = append(w.partial, p...)
	for {
		index := bytes.IndexByte(w.partial, '\n')
		if index < 0 {
			break
		}
		w.line(w.partial[:index])
		w.partial = w.partial[index+1:]
	}
	return len(p), nil
}

func (w *mcpStderr) line(line []byte) {
	if w.progress != nil && bytes.HasPrefix(line, []byte("{")) {
		var record map[string]any
		if json.Unmarshal(line, &record) == nil {
			if message, ok := mcpProgressMessage(record); ok {
				w.progress.notify(message)
				return
			}
		}
	}
	if bytes.HasPrefix(line, []byte("debug: ")) {
		if w.debug != nil {
			_, _ = w.debug.Write(append(append([]byte(nil), line...), '\n'))
		}
		return
	}
	_, _ = w.kept.Write(append(append([]byte(nil), line...), '\n'))
}

func (w *mcpStderr) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.partial) > 0 {
		w.line(w.partial)
		w.partial = nil
	}
	return strings.TrimSpace(w.kept.String())
}

// mcpLineNotifier reports each stream record a command writes as progress.
type mcpLineNotifier struct {
	io.Writer
	progress *mcpProgress
	records  int
}

func (w *mcpLineNotifier) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	for _, b := range p[:n] {
		if b == '\n' {
			w.records++
			w.progress.notify(fmt.Sprintf("Received %d stream records", w.records))
		}
	}
	return n, err
}

func (a *App) callMCPCommand(ctx context.Context, cmd *Command, args map[string]any, parent Options) (any, bool, int) {
	outcome := a.runMCPCommand(ctx, cmd, args, parent, mcpPolicyFromOptions(parent), nil)
	return outcome.value, outcome.transformed, outcome.exit
}

func (a *App) runMCPCommand(ctx context.Context, cmd *Command, args map[string]any, parent Options, policy mcpPolicy, extra []string) mcpCallResult {
	if cmd.Stream {
		bound := mcpMaxStreamDuration
		if controls, ok := args["_flint"].(map[string]any); ok {
			if raw, exists := controls["for"]; exists {
				if requested, err := time.ParseDuration(fmt.Sprint(raw)); err == nil && requested > 0 {
					bound = requested
				}
			}
		}
		var cancel context.CancelFunc
		// Let the CLI finish its --for checkpoint and report unmet counts
		// before the outer MCP safety deadline cancels it.
		ctx, cancel = context.WithTimeout(ctx, bound+time.Second)
		defer cancel()
	}
	argv := append([]string(nil), cmd.Path[1:]...)
	argv = appendInheritedMCPOptions(argv, parent)
	transformOptions, transformed, transformErr := mcpOutputTransformOptions(cmd, args)
	if transformErr != nil {
		return mcpCallResult{value: mcpCLIErrorEnvelope(transformErr), exit: transformErr.ExitCode}
	}
	body := map[string]any{}
	known := map[string]bool{}
	maxPositional := 0
	for _, arg := range cmd.Arguments {
		maxPositional = max(maxPositional, arg.Positional)
	}
	positionals := make([]string, maxPositional)
	for _, arg := range cmd.Arguments {
		known[arg.Name] = true
		value, ok := args[arg.Name]
		if !ok {
			continue
		}
		if arg.Positional > 0 {
			positionals[arg.Positional-1] = fmt.Sprint(value)
			continue
		}
		if arg.Flag != "" {
			argv = appendMCPFlag(argv, arg.Flag, value)
		}
	}
	for key, value := range args {
		if !known[key] && !strings.HasPrefix(key, "_") {
			body[key] = value
		}
	}
	if explicit, ok := body["body"].(map[string]any); ok && cmd.CanonicalName == "api" {
		delete(body, "body")
		if len(body) > 0 {
			e := usageError("CONFLICTING_ARGUMENTS", "Put every request field inside body; do not also pass fields as top-level arguments.", "body")
			return mcpCallResult{value: mcpCLIErrorEnvelope(e), exit: e.ExitCode}
		}
		body = explicit
	}
	progress := mcpProgressFrom(ctx)
	if progress != nil && !cmd.Stream {
		// The CLI reports progress only for these; --progress elsewhere is a
		// usage error.
		controls, _ := args["_flint"].(map[string]any)
		all, _ := controls["all"].(bool)
		paginate, _ := controls["paginate"].(bool)
		if _, waits := controls["wait_for"]; all || paginate || waits || cmd.CanonicalName == "upgrade" {
			argv = append(argv, "--progress=json")
		}
	}
	if globals, ok := args["_flint"].(map[string]any); ok {
		for key, value := range globals {
			if key == "field" || key == "select" || key == "jq" {
				continue
			}
			flag := "--" + strings.ReplaceAll(key, "_", "-")
			argv = appendMCPFlag(argv, flag, value)
		}
	}
	if cmd.Stream {
		globals, _ := args["_flint"].(map[string]any)
		_, hasDuration := globals["for"]
		if _, exists := globals["max_events"]; !exists && !hasDuration {
			argv = append(argv, "--max-events", fmt.Sprint(mcpMaxStreamEvents))
		}
		if _, exists := globals["for"]; !exists {
			argv = append(argv, "--for", mcpMaxStreamDuration.String())
		}
	}
	argv = append(argv, extra...)
	var stdin io.Reader = strings.NewReader("")
	if len(body) > 0 {
		raw, err := json.Marshal(body)
		if err != nil {
			return mcpCallResult{value: map[string]any{"error": "Could not encode MCP tool input: " + err.Error()}, exit: ExitUsage}
		}
		stdin = bytes.NewReader(raw)
		argv = append(argv, "--input", "-")
	}
	argv = append(argv, "--output", "json", "--no-input")
	// Positionals must follow all options and the terminator so data such as
	// a help-search query beginning with -- is never interpreted as a flag.
	if len(positionals) > 0 {
		argv = append(argv, "--")
		for _, value := range positionals {
			if value != "" {
				argv = append(argv, value)
			}
		}
	}
	childCtx, cancelChild := context.WithCancel(ctx)
	defer cancelChild()
	observer := &mcpEnvironmentObserver{allowLive: policy.allowLive, cancel: cancelChild}
	stdout := &mcpCappedBuffer{limit: mcpMaxOutputBytes}
	stderr := &mcpStderr{kept: mcpCappedBuffer{limit: mcpMaxStderrBytes}, progress: progress, debug: a.Stderr}
	child := *a
	child.Stdout = stdout
	if progress != nil && cmd.Stream {
		child.Stdout = &mcpLineNotifier{Writer: stdout, progress: progress}
	}
	child.Stderr = stderr
	child.Stdin = stdin
	child.IsTTY = func() bool { return false }
	child.Context = childCtx
	child.HTTPClient = a.mcpObservedHTTPClient(observer)
	exit := child.Run(argv)
	environment, blocked := observer.state()
	outcome := mcpCallResult{exit: exit, environment: environment}
	if exit == ExitOK {
		outcome.warnings = stderr.String()
	}
	if !policy.allowLive && (blocked || exit != ExitOK) {
		var probe any
		_ = decodeJSONNumbers(bytes.TrimSpace(stdout.Bytes()), &probe)
		if blocked || mcpErrorField(probe, "code") == "LIVE_ACKNOWLEDGEMENT_REQUIRED" {
			outcome.liveBlocked = true
			outcome.environment = "live"
			outcome.value = map[string]any{"error": map[string]any{"type": "configuration_error", "code": "LIVE_ACCESS_DISABLED", "message": mcpLiveDisabledMessage}}
			outcome.exit = ExitAuth
			return outcome
		}
	}
	if stdout.exceeded {
		outcome.value = map[string]any{"error": fmt.Sprintf("Flint command output exceeded the %d byte MCP safety limit.", mcpMaxOutputBytes)}
		outcome.exit = ExitNetwork
		return outcome
	}
	trimmed := bytes.TrimSpace(stdout.Bytes())
	if len(trimmed) == 0 {
		outcome.value, outcome.warnings = map[string]any{"stderr": stderr.String()}, ""
		return outcome
	}
	var result any
	if err := decodeJSONNumbers(trimmed, &result); err == nil {
		if exit == ExitOK && transformed {
			transformOptions.outputLimit = mcpMaxOutputBytes
			result, transformErr = applyCommandOutputTransforms(childCtx, result, cmd, transformOptions)
			if transformErr != nil {
				outcome.value, outcome.exit = mcpCLIErrorEnvelope(transformErr), transformErr.ExitCode
				return outcome
			}
		}
		outcome.value, outcome.transformed = result, transformed
		return outcome
	}
	var lines []any
	scanner := bufio.NewScanner(bytes.NewReader(trimmed))
	scanner.Buffer(make([]byte, 64<<10), 20<<20)
	for scanner.Scan() {
		var line any
		if err := decodeJSONNumbers(scanner.Bytes(), &line); err != nil {
			outcome.value = map[string]any{"error": "Could not decode Flint NDJSON output: " + err.Error(), "stderr": strings.TrimSpace(stderr.String())}
			outcome.exit = ExitNetwork
			return outcome
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		outcome.value = map[string]any{"error": "Could not read Flint NDJSON output: " + err.Error(), "stderr": strings.TrimSpace(stderr.String())}
		outcome.exit = ExitNetwork
		return outcome
	}
	if len(lines) > 0 {
		outcome.value = map[string]any{"records": lines}
		return outcome
	}
	outcome.value = map[string]any{"stdout": string(trimmed), "stderr": strings.TrimSpace(stderr.String())}
	return outcome
}

const mcpLiveDisabledMessage = "The selected Flint credential or context is LIVE (production), and this MCP server was started without --allow-live, so it will not act on live data. Do not retry. Tell the user: to allow live access, restart the server with flint mcp serve --allow-live; otherwise switch to a sandbox credential (for example FLINT_API_KEY=flint_test_... or flint context switch to a sandbox context)."

func mcpOutputTransformOptions(cmd *Command, args map[string]any) (Options, bool, *CLIError) {
	opts := defaultOptions()
	controls, _ := args["_flint"].(map[string]any)
	transformed := false
	if raw, exists := controls["field"]; exists {
		value, ok := raw.(string)
		if !ok || strings.TrimSpace(value) == "" {
			return opts, false, usageError("INVALID_FLAG_VALUE", "_flint.field must be a non-empty string.", "field")
		}
		opts.Field = value
		transformed = true
	}
	if raw, exists := controls["jq"]; exists {
		value, ok := raw.(string)
		if !ok || strings.TrimSpace(value) == "" {
			return opts, false, usageError("INVALID_FLAG_VALUE", "_flint.jq must be a non-empty string.", "jq")
		}
		opts.JQ = value
		transformed = true
	}
	if raw, exists := controls["select"]; exists {
		values, ok := raw.([]any)
		if !ok || len(values) == 0 {
			return opts, false, usageError("INVALID_FLAG_VALUE", "_flint.select must be a non-empty string array.", "select")
		}
		for _, rawValue := range values {
			value, ok := rawValue.(string)
			if !ok || strings.TrimSpace(value) == "" {
				return opts, false, usageError("INVALID_FLAG_VALUE", "_flint.select must be a non-empty string array.", "select")
			}
			opts.Select = append(opts.Select, value)
		}
		transformed = true
	}
	if !transformed {
		return opts, false, nil
	}
	if !cmd.Supports.Field || !cmd.Supports.Select || !cmd.Supports.JQ {
		return opts, false, usageError("UNSUPPORTED_FLAG", "Output transforms do not apply to this command.", "field")
	}
	if opts.Field != "" && opts.JQ != "" {
		return opts, false, usageError("CONFLICTING_FLAGS", "_flint.jq and _flint.field cannot be used together.", "field")
	}
	if paginate, _ := controls["paginate"].(bool); paginate {
		return opts, false, usageError("UNSUPPORTED_FLAG", "Output transforms do not apply to paginated NDJSON output.", "paginate")
	}
	return opts, true, nil
}

func mcpCLIErrorEnvelope(err *CLIError) map[string]any {
	var stdout bytes.Buffer
	(&App{Stdout: &stdout}).writeError(err, Options{Output: "json"})
	var envelope map[string]any
	_ = json.Unmarshal(stdout.Bytes(), &envelope)
	return envelope
}

func appendMCPFlag(argv []string, flag string, value any) []string {
	switch values := value.(type) {
	case []any:
		for _, item := range values {
			argv = appendMCPFlag(argv, flag, item)
		}
	case bool:
		argv = append(argv, flag+"="+fmt.Sprint(values))
	default:
		argv = append(argv, flag+"="+fmt.Sprint(value))
	}
	return argv
}

func appendInheritedMCPOptions(argv []string, opts Options) []string {
	for _, item := range []struct {
		name  string
		value string
	}{
		{"profile", opts.Profile},
		{"merchant", opts.Merchant},
		{"color", opts.Color},
	} {
		if len(opts.Raw[item.name]) > 0 && item.value != "" {
			argv = appendMCPFlag(argv, "--"+item.name, item.value)
		}
	}
	for _, item := range []struct {
		name  string
		value bool
	}{
		{"live", opts.Live},
		{"debug", opts.Debug},
		{"quiet", opts.Quiet},
	} {
		if len(opts.Raw[item.name]) > 0 {
			argv = append(argv, "--"+item.name+"="+fmt.Sprint(item.value))
		}
	}
	if len(opts.Raw["timeout"]) > 0 {
		argv = append(argv, "--timeout", opts.Timeout.String())
	}
	return argv
}

// mcpControlFlags maps CLI flags in error remediation to MCP tool controls.
var mcpControlFlags = map[string]string{
	"live": "live", "confirm": "confirm", "idempotency-key": "idempotency_key", "dry-run": "dry_run",
	"preview": "preview", "page-size": "page_size", "page-token": "page_token", "all": "all",
	"expand": "expand", "field": "field", "select": "select", "jq": "jq", "for": "for",
	"max-events": "max_events", "wait-for": "wait_for",
	"timeout": "timeout", "merchant": "merchant", "profile": "profile", "paginate": "paginate",
}

var mcpFlagPattern = regexp.MustCompile(`--([a-z][a-z0-9-]*)(=client)?`)

// mcpTranslateRemediation rewrites CLI flag advice into the tool's own terms:
// _flint controls, tool arguments, or guidance for the human running the server.
func mcpTranslateRemediation(message string, cmd *Command, policy mcpPolicy) string {
	if strings.Contains(message, "Re-run with --confirm.") {
		replacement := "Re-run with _flint.confirm set to true, after checking the effect with _flint.preview or _flint.dry_run where available."
		if !policy.allowDestructive {
			replacement = "Only the user can confirm it on this MCP server."
		}
		message = strings.ReplaceAll(message, "Re-run with --confirm.", replacement)
	}
	arguments := map[string]string{}
	if cmd != nil {
		for _, arg := range cmd.Arguments {
			if arg.Flag != "" {
				arguments[strings.TrimPrefix(arg.Flag, "--")] = arg.Name
			}
		}
	}
	return mcpFlagPattern.ReplaceAllStringFunc(message, func(match string) string {
		parts := mcpFlagPattern.FindStringSubmatch(match)
		name := parts[1]
		if argument, ok := arguments[name]; ok {
			return "the " + argument + " argument"
		}
		control, ok := mcpControlFlags[name]
		if !ok || (name == "live" && !policy.allowLive) || (name == "confirm" && !policy.allowDestructive) {
			return match
		}
		if parts[2] != "" {
			return `_flint.` + control + `="client"`
		}
		return "_flint." + control
	})
}

// mcpCredentialMessages replace CLI sign-in advice, which an MCP client
// configuration cannot follow, for errors about a missing credential.
var mcpCredentialMessages = map[string]string{
	"API_KEY_REQUIRED":         "No Flint credential is configured for this MCP server.",
	"CREDENTIAL_LOOKUP_FAILED": "This MCP server could not read its Flint credential from the OS keychain, which may be locked.",
}

const mcpCredentialRemedy = " Do not retry. Tell the user to set FLINT_API_KEY (a sandbox flint_test_ key from " + dashboardAPIKeysURL + ") or FLINT_PROFILE (a profile saved with flint login) in this server's env in their MCP client configuration, then restart the server."

// mcpTranslateDetails rewrites CLI flag advice anywhere in error details,
// leaving commands meant for the human and identifiers unchanged.
func mcpTranslateDetails(value any, cmd *Command, policy mcpPolicy) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			if key == "command" || key == "url" || key == "idempotency_key" {
				out[key] = item
				continue
			}
			out[key] = mcpTranslateDetails(item, cmd, policy)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = mcpTranslateDetails(item, cmd, policy)
		}
		return out
	case []map[string]any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = mcpTranslateDetails(item, cmd, policy)
		}
		return out
	case string:
		return mcpTranslateRemediation(typed, cmd, policy)
	}
	return value
}

func mcpCommandError(cmd *Command, outcome mcpCallResult, policy mcpPolicy, override string) map[string]any {
	value := outcome.value
	message := override
	if message == "" {
		message = mcpTranslateRemediation(mcpErrorText(value), cmd, policy)
	}
	code := mcpErrorField(value, "code")
	if replacement, ok := mcpCredentialMessages[code]; ok && override == "" {
		message = replacement + mcpCredentialRemedy
	}
	if envelope, ok := value.(map[string]any); ok {
		if e, ok := envelope["error"].(map[string]any); ok {
			copyEnvelope := copyMap(envelope)
			copyError := copyMap(e)
			if remediation, has := copyError["remediation"]; has {
				copyError["remediation"] = mcpTranslateDetails(remediation, cmd, policy)
			}
			if _, credential := mcpCredentialMessages[code]; credential {
				copyError["remediation"] = map[string]any{"next_actions": []any{map[string]any{"reason_message": strings.TrimSpace(mcpCredentialRemedy)}}}
			}
			if details, has := copyError["details"]; has {
				translated := mcpTranslateDetails(details, cmd, policy)
				if _, credential := mcpCredentialMessages[code]; credential {
					if detailMap, ok := translated.(map[string]any); ok {
						detailMap["remediation"] = map[string]any{"next_actions": []any{map[string]any{"reason_message": strings.TrimSpace(mcpCredentialRemedy)}}}
					}
				}
				copyError["details"] = translated
				// A retry must reuse the key, or a write that already reached
				// Flint could run twice.
				if detailMap, ok := translated.(map[string]any); ok && !strings.Contains(message, "_flint.idempotency_key") {
					if key, ok := detailMap["idempotency_key"].(string); ok && key != "" {
						message = strings.TrimSpace(message + ` To retry, call the tool again with the same arguments and _flint.idempotency_key set to "` + key + `"; a different key could make the write run twice.`)
					}
				}
			}
			if _, has := copyError["message"]; has {
				copyError["message"] = message
			}
			copyEnvelope["error"] = copyError
			value = copyEnvelope
		}
	}
	if strings.TrimSpace(message) == "" {
		message = fmt.Sprintf("Flint command failed with exit code %d.", outcome.exit)
	}
	details := map[string]any{"exit_code": outcome.exit}
	if outcome.environment != "" {
		details["environment"] = outcome.environment
	}
	if value != nil {
		details["result"] = value
	}
	return mcpToolError(message, details)
}

// mcpEnvironmentMetaKey names the credential environment in result _meta.
const mcpEnvironmentMetaKey = "com.withflintpay/environment"

// mcpToolResult keeps structuredContent identical to the command's JSON output
// so it still matches the public output schema. The credential environment is
// reported beside it, in a second text block and in _meta.
func mcpToolResult(value any, structured, transformed bool, environment string) map[string]any {
	structuredValue := value
	if transformed {
		structuredValue = map[string]any{"value": value}
	}
	raw, _ := json.Marshal(structuredValue)
	content := []map[string]any{{"type": "text", "text": string(raw)}}
	result := map[string]any{"content": content}
	if environment != "" {
		result["content"] = append(content, map[string]any{"type": "text", "text": "Flint environment: " + environment})
		result["_meta"] = map[string]any{mcpEnvironmentMetaKey: environment}
	}
	if structured {
		result["structuredContent"] = structuredValue
	}
	return result
}

// mcpOutputSchema is the public output contract of a tool's structured result.
// tools/list does not advertise it (every tool would embed its full component
// closure); agents fetch it on demand with the schema.output tool, and tests
// check results against it.
func mcpOutputSchema(cmd *Command) (map[string]any, *CLIError) {
	outputSchema, outputErr := schemaForCommand(cmd, false)
	if outputErr != nil {
		return nil, outputErr
	}
	if cmd.Stream {
		stream := mcpStreamOutputSchema()
		if cmd.CanonicalName == "listen" {
			return map[string]any{"type": "object", "anyOf": []any{stream, map[string]any{
				"type": "object", "additionalProperties": false, "required": []string{"data"},
				"properties": map[string]any{"data": map[string]any{
					"type": "object", "additionalProperties": false, "required": []string{"signing_secret", "source"},
					"properties": map[string]any{"signing_secret": map[string]any{"type": "string"}, "source": map[string]any{"type": "string"}},
				}},
			}}}, nil
		}
		return stream, nil
	}
	variants := []any{outputSchema}
	if cmd.Supports.DryRunClient || cmd.Destructive {
		variants = append(variants, mcpPreviewOutputSchema())
	}
	if cmd.Supports.Field && cmd.Supports.Select && cmd.Supports.JQ {
		variants = append(variants, map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties":           map[string]any{"value": map[string]any{}},
			"required":             []string{"value"},
		})
	}
	return map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type":    "object",
		"anyOf":   variants,
	}, nil
}

// Client dry runs and destructive previews return the prepared request instead
// of the API resource. Keep that envelope explicit in the tool's output union.
func mcpPreviewOutputSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"data"},
		"properties": map[string]any{
			"data": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"method", "path", "headers", "body", "persistent_side_effects"},
				"properties": map[string]any{
					"method": map[string]any{"type": "string"},
					"path":   map[string]any{"type": "string"},
					"headers": map[string]any{
						"type":       "object",
						"properties": map[string]any{"Idempotency-Key": map[string]any{"type": "string"}},
					},
					"body":                    map[string]any{},
					"persistent_side_effects": map[string]any{"const": false},
					"warnings":                map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
					"preview": map[string]any{
						"type":     "object",
						"required": []string{"permission_check", "reversibility", "affected_resources"},
						"properties": map[string]any{
							"permission_check":   map[string]any{"type": "object"},
							"reversibility":      map[string]any{"type": "object"},
							"affected_resources": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
						},
					},
				},
			},
		},
	}
}

func mcpStreamOutputSchema() map[string]any {
	return map[string]any{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"records": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": true,
					"required":             []string{"type"},
					"properties": map[string]any{
						"type": map[string]any{"type": "string", "enum": []string{"version", "event", "listener", "ready", "gap", "withheld", "forward", "reconnect", "disconnect", "checkpoint"}},
					},
				},
			},
		},
		"required": []string{"records"},
	}
}

// mcpToolError leads with the human-readable message; details follow as JSON.
func mcpToolError(message string, details any) map[string]any {
	text := message
	if details != nil {
		raw, _ := json.Marshal(details)
		text += "\n\n" + string(raw)
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": true}
}
func rpcResult(id, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}
func rpcError(id any, code int, message string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
}
