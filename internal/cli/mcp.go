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
	"time"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

const latestMCPProtocolVersion = "2025-11-25"

const (
	mcpMaxStreamEvents      = 100
	mcpMaxStreamDuration    = 2 * time.Minute
	mcpMaxOutputBytes       = 32 << 20
	mcpMaxStderrBytes       = 1 << 20
	mcpMaxLineBytes         = 8 << 20
	mcpConfirmationDeadline = 10 * time.Minute
)

var supportedMCPProtocolVersions = map[string]bool{
	"2025-11-25": true,
	"2025-06-18": true,
	"2025-03-26": true,
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
	toolPrefixes     []string
}

func mcpBoolOption(opts Options, name string) bool {
	values := opts.Raw[name]
	return len(values) > 0 && values[len(values)-1] == "true"
}

func mcpPolicyFromOptions(opts Options) mcpPolicy {
	policy := mcpPolicy{allowLive: mcpBoolOption(opts, "allow-live"), allowDestructive: mcpBoolOption(opts, "allow-destructive")}
	for _, raw := range opts.Raw["tools"] {
		for _, prefix := range strings.Split(raw, ",") {
			if prefix = strings.TrimSuffix(strings.TrimSpace(prefix), "."); prefix != "" {
				policy.toolPrefixes = append(policy.toolPrefixes, prefix)
			}
		}
	}
	return policy
}

func mcpPrefixMatches(prefix, name string) bool {
	return name == prefix || strings.HasPrefix(name, prefix+".")
}

func (p mcpPolicy) includes(name string) bool {
	if len(p.toolPrefixes) == 0 {
		return true
	}
	for _, prefix := range p.toolPrefixes {
		if mcpPrefixMatches(prefix, name) {
			return true
		}
	}
	return false
}

func (a *App) validateMCPServeOptions(opts Options) *CLIError {
	policy := mcpPolicyFromOptions(opts)
	if len(opts.Raw["tools"]) > 0 && len(policy.toolPrefixes) == 0 {
		return usageError("INVALID_FLAG_VALUE", "--tools must name at least one tool prefix, such as customers.", "tools")
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
		e := usageError("UNKNOWN_TOOL_PREFIX", "--tools "+strings.Join(unknown, ",")+" matches no MCP tool. Use canonical names or prefixes such as customers or payment-intents.create.", "tools")
		details := []map[string]any{}
		for _, prefix := range unknown {
			details = append(details, map[string]any{"input": prefix, "suggestions": mcpNameSuggestions(prefix, mcpToolGroups(all))})
		}
		e.Details = details
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
	inFlight := map[string]context.CancelFunc{}
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
			inFlight[key] = cancel
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
		for _, cancel := range inFlight {
			cancel()
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
					cancel := inFlight[key]
					inFlightMu.Unlock()
					if cancel != nil {
						cancel()
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
				return rpcError(req.ID, -32603, "Could not generate tool input schema for "+cmd.CanonicalName)
			}
			title := mcpToolTitle(cmd)
			annotations := mcpToolAnnotations(cmd)
			annotations["title"] = title
			tool := map[string]any{"name": cmd.CanonicalName, "description": cmd.Description, "inputSchema": schema, "annotations": annotations}
			// Output schemas are optional in MCP. Each embedded its full
			// component closure (about 19 MB in total), so agents fetch one on
			// demand with the schema.output tool instead.
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
		if cmd.CanonicalName == "help" {
			if topic, _ := arguments["topic"].(string); strings.TrimSpace(topic) == "" {
				return rpcResult(req.ID, mcpToolResult(mcpHelpIndex(a.mcpTools(policy)), protocolVersion >= "2025-06-18", false, ""))
			}
		}
		return rpcResult(req.ID, a.runMCPTool(ctx, cmd, arguments, opts, policy, protocolVersion >= "2025-06-18"))
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
	return strings.Join([]string{
		"Flint Pay MCP server. Each tool runs one Flint CLI command against the Flint public API, named <resource>.<action> (for example customers.create), using the credential configured for this server (FLINT_API_KEY, FLINT_PROFILE, or a saved flint login).",
		"Results that reached the API name the credential environment, sandbox or live, in a \"Flint environment:\" text line.",
		live,
		confirm,
		"Before a write, use _flint.dry_run=\"client\" (or _flint.preview for destructive operations) to inspect the request without making it. Set _flint.idempotency_key on writes and reuse the same key when retrying after a timeout, or the operation may run twice.",
		"Call help without a topic for an index, help with a tool name for its arguments, and schema.output for a tool's response schema. The human can expose fewer tools with flint mcp serve --tools customers,payment-intents.",
	}, " ")
}

func mcpCommandExposed(cmd *Command) bool {
	return cmd != nil && cmd.CanonicalName == cmd.Name && cmd.CanonicalName != "mcp.serve" && !cmd.MCPHidden
}

// mcpAllTools lists every tool the server can expose. History is split so
// listing stays read-only while clearing is a separate destructive tool.
func (a *App) mcpAllTools() []*Command {
	tools := []*Command{}
	for _, cmd := range a.Registry.Commands {
		if !mcpCommandExposed(cmd) {
			continue
		}
		if cmd.CanonicalName == "history" {
			list, clear := mcpHistoryTools(cmd)
			tools = append(tools, list, clear)
			continue
		}
		tools = append(tools, cmd)
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

func (a *App) mcpTool(name string, policy mcpPolicy) (*Command, bool) {
	if !policy.includes(name) {
		return nil, false
	}
	for _, tool := range a.mcpAllTools() {
		if tool.CanonicalName == name {
			return tool, true
		}
	}
	return nil, false
}

func (a *App) mcpUnknownToolError(id any, name string, policy mcpPolicy) map[string]any {
	for _, tool := range a.mcpAllTools() {
		if tool.CanonicalName == name {
			response := rpcError(id, -32602, "Unknown tool: "+name+" is not enabled on this server, which was started with --tools "+strings.Join(policy.toolPrefixes, ",")+".")
			return response
		}
	}
	var names []string
	for _, tool := range a.mcpTools(policy) {
		names = append(names, tool.CanonicalName)
	}
	var suggestions []string
	if alias, ok := a.Registry.ByName(name); ok && alias.CanonicalName != name && policy.includes(alias.CanonicalName) {
		suggestions = append(suggestions, alias.CanonicalName)
	}
	for _, suggestion := range mcpNameSuggestions(name, names) {
		if len(suggestions) < 3 && !containsString(suggestions, suggestion) {
			suggestions = append(suggestions, suggestion)
		}
	}
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

// mcpNameSuggestions ranks names by edit distance, favoring shared prefixes.
func mcpNameSuggestions(input string, names []string) []string {
	input = strings.ToLower(strings.TrimSpace(input))
	type candidate struct {
		name  string
		score int
	}
	var candidates []candidate
	limit := max(2, len(input)/3)
	for _, name := range names {
		score := levenshteinDistance(input, name)
		if strings.HasPrefix(name, input) || strings.HasPrefix(input, name+".") {
			score = min(score, 1)
		}
		if score <= limit {
			candidates = append(candidates, candidate{name, score})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score < candidates[j].score
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
	if topic, ok := properties["topic"].(map[string]any); ok && cmd.CanonicalName == "help" {
		topic["description"] = "A tool name such as customers.create, or a reference topic: test-cards, exit-codes, errors, events. Omit for a short index."
	}
	return schema, nil
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
	controlsProperty["description"] = "CLI execution controls. Streaming tools require max_events or for. Flint also applies a 100-record and 2-minute safety bound."
	schema["allOf"] = appendSchemaConstraint(schema["allOf"], map[string]any{
		"required": []string{"_flint"},
		"properties": map[string]any{
			"_flint": map[string]any{
				"anyOf": []any{
					map[string]any{"required": []string{"max_events"}},
					map[string]any{"required": []string{"for"}},
				},
			},
		},
	})
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
	idempotent := readOnly
	openWorld := !cmd.Local
	switch cmd.CanonicalName {
	case "api":
		// The raw tool's method is selected at call time and can be DELETE.
		readOnly = false
		destructive = true
		idempotent = false
	case "auth.logout":
		readOnly = false
		destructive = true
		openWorld = true
	case "history":
		readOnly, destructive, idempotent = true, false, true
	case "history.clear":
		readOnly, destructive, idempotent = false, true, true
	case "listen":
		// Forwards each event with a POST to the local endpoint.
		readOnly, destructive, idempotent, openWorld = false, false, false, true
	case "config.set":
		readOnly, idempotent = false, true
	case "doctor":
		readOnly, idempotent, openWorld = false, false, true
	case "signup":
		readOnly, idempotent, openWorld = false, false, true
	case "config.validate", "init", "help.search":
		openWorld = true
	}
	return map[string]any{
		"readOnlyHint":    readOnly,
		"destructiveHint": destructive,
		"idempotentHint":  idempotent,
		"openWorldHint":   openWorld,
	}
}

// mcpToolTitle prefers the description's first sentence when it is short and
// free of CLI flags; otherwise it spells out the canonical name.
func mcpToolTitle(cmd *Command) string {
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

func mcpHelpIndex(tools []*Command) map[string]any {
	groups := map[string]int{}
	for _, tool := range tools {
		group, _, _ := strings.Cut(tool.CanonicalName, ".")
		groups[group]++
	}
	return map[string]any{"data": map[string]any{
		"summary":    "Index of Flint MCP help. Tools are named <resource>.<action>; resource_groups lists each resource with its number of tools.",
		"tool_count": len(tools),
		"topics": []map[string]string{
			{"topic": "<tool name>", "description": "Arguments, flags, and examples for one tool, for example customers.create."},
			{"topic": "test-cards", "description": "Sandbox payment method tokens and card numbers for test scenarios."},
			{"topic": "exit-codes", "description": "Exit codes and error types returned by Flint commands."},
			{"topic": "errors", "description": "Same catalog as exit-codes."},
			{"topic": "events", "description": "Full webhook event catalog with payload schemas (large)."},
		},
		"finding_tools": []string{
			"Call help with topic set to a tool name for its arguments and examples.",
			"Call schema.input or schema.output with command set to a tool name for its full JSON input or response schema.",
			"Call help.search with a question to search Flint Help community answers.",
			"tools/list returns every enabled tool; the human can limit them with flint mcp serve --tools PREFIX,...",
		},
		"resource_groups": groups,
	}}
}

type mcpCallResult struct {
	value       any
	transformed bool
	exit        int
	environment string
	liveBlocked bool
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
	return mcpToolResult(outcome.value, structured, outcome.transformed, outcome.environment)
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
	return code == "CONFIRMATION_REQUIRED" || code == "PRODUCTION_CONFIRMATION_REQUIRED"
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
	prompt := fmt.Sprintf("An AI agent wants to run the Flint tool %s (environment: %s). %s\n\nArguments: %s\n\nAllow this operation?", cmd.CanonicalName, environment, cliMessage, rawArgs)
	requestCtx, cancel := context.WithTimeout(ctx, mcpConfirmationDeadline)
	defer cancel()
	response, err := session.request(requestCtx, "elicitation/create", map[string]any{
		"message": prompt,
		"requestedSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"confirm": map[string]any{"type": "boolean", "title": "Allow " + cmd.CanonicalName, "description": "Approve this " + environment + " operation.", "default": false},
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
	return false, "The user declined to confirm " + cmd.CanonicalName + "; the operation was not performed. Do not retry unless the user asks."
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
		ctx, cancel = context.WithTimeout(ctx, bound)
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
		if _, exists := globals["max_events"]; !exists {
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
	stderr := &mcpCappedBuffer{limit: mcpMaxStderrBytes}
	child := *a
	child.Stdout = stdout
	child.Stderr = stderr
	child.Stdin = stdin
	child.IsTTY = func() bool { return false }
	child.Context = childCtx
	child.HTTPClient = a.mcpObservedHTTPClient(observer)
	exit := child.Run(argv)
	environment, blocked := observer.state()
	outcome := mcpCallResult{exit: exit, environment: environment}
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
		outcome.value = map[string]any{"stderr": strings.TrimSpace(stderr.String())}
		return outcome
	}
	var result any
	if err := decodeJSONNumbers(trimmed, &result); err == nil {
		if exit == ExitOK && transformed {
			transformOptions.outputLimit = mcpMaxOutputBytes
			result, transformErr = applyOutputTransforms(childCtx, result, transformOptions)
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
	payload := map[string]any{"type": err.Type, "code": err.Code, "message": err.Message}
	if err.Param != "" {
		payload["param"] = err.Param
	}
	if err.Details != nil {
		payload["details"] = err.Details
	}
	return map[string]any{"error": payload}
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
	"max-events": "max_events", "wait-for": "wait_for", "progress": "progress", "open": "open",
	"timeout": "timeout", "merchant": "merchant", "profile": "profile", "paginate": "paginate",
	"debug": "debug", "quiet": "quiet",
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

func mcpCommandError(cmd *Command, outcome mcpCallResult, policy mcpPolicy, override string) map[string]any {
	value := outcome.value
	message := override
	if message == "" {
		message = mcpTranslateRemediation(mcpErrorText(value), cmd, policy)
	}
	if envelope, ok := value.(map[string]any); ok {
		if e, ok := envelope["error"].(map[string]any); ok {
			copyEnvelope := copyMap(envelope)
			copyError := copyMap(e)
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
		return mcpStreamOutputSchema(), nil
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
						"required":   []string{"Idempotency-Key"},
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
						"type": map[string]any{"type": "string", "enum": []string{"version", "event", "listener", "ready", "gap", "withheld", "forward", "disconnect", "checkpoint"}},
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
