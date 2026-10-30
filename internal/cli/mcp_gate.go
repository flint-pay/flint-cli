package cli

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"fmt"
	"github.com/flint-pay/flint-cli/internal/opindex"
	"strings"
)

// mcpSelectionSources says where the human set each server selection.
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

// mcpGateInput contains validated public controls and the environment returned
// by this call's authentication. Confirm and Live are per-call controls, never
// inherited CLI flags or internal confirmation granted after user approval.
type mcpGateInput struct {
	Environment string
	Local       bool
	Write       bool
	Gated       bool // opindex.Entry.ConfirmRequired(), not destructiveHint
	Confirm     bool
	DryRun      bool
	Live        bool
	Elicitation bool // form-mode elicitation, after capability negotiation
}

type mcpGateAction string

const (
	mcpGateRun    mcpGateAction = "run"
	mcpGateAsk    mcpGateAction = "ask"
	mcpGateReject mcpGateAction = "reject"
)

// A rejection is a CLI-origin error. The integration layer owns its envelope,
// next actions and messages, including terminal fallback and recovery reads.
type mcpGateDecision struct {
	Action mcpGateAction
	Code   string
	Reason string
}

// mcpDecideGate implements spec 11.1 after operation exposure and control/input
// validation. The ask decision grants no authorization: only a subsequent
// action=accept with approve=true authorizes the prepared request.
func mcpDecideGate(policy mcpPolicy, call mcpGateInput) mcpGateDecision {
	reject := func(code, reason string) mcpGateDecision {
		return mcpGateDecision{Action: mcpGateReject, Code: code, Reason: reason}
	}
	// Read-only is an exposure restriction, so a dry run cannot bypass it.
	if policy.readOnly && call.Write {
		return reject("OPERATION_NOT_AVAILABLE", "read_only_server")
	}
	if call.DryRun {
		return mcpGateDecision{Action: mcpGateRun}
	}
	environment := normalizeEnvironment(call.Environment)
	if call.Local {
		environment = "sandbox"
	}
	if environment == "live" {
		if !policy.allowLive {
			return reject("LIVE_ACCESS_DISABLED", "")
		}
		if !call.Live {
			return reject("LIVE_ACKNOWLEDGEMENT_REQUIRED", "")
		}
	}
	if !call.Write || !call.Gated {
		return mcpGateDecision{Action: mcpGateRun}
	}
	// Unknown/unverified environments use live confirmation rules, without the
	// verified-live access restriction or acknowledgement requirement above.
	if environment != "sandbox" && !policy.allowDestructive && !call.Elicitation {
		return reject("CONFIRMATION_NEEDS_USER", "")
	}
	if !call.Confirm {
		return reject("CONFIRMATION_REQUIRED", "")
	}
	if environment != "sandbox" && !policy.allowDestructive {
		return mcpGateDecision{Action: mcpGateAsk}
	}
	return mcpGateDecision{Action: mcpGateRun}
}

// One gate belongs to one child invocation. It authorizes the request prepared
// by that invocation, rather than executing a command again after approval.
type mcpCallGate struct {
	entry           opindex.Entry
	input           mcpGateInput
	policy          mcpPolicy
	session         *mcpSession
	environment     string
	merchant        string
	approvalTimeout time.Duration
}

func (a *App) newMCPCallGate(ctx context.Context, cmd *Command, args map[string]any, policy mcpPolicy) (*mcpCallGate, *CLIError) {
	index, err := a.mcpOperationIndex(policy)
	if err != nil {
		return nil, mcpDiscoverySnapshotError(err)
	}
	entry, ok := index.Lookup(cmd.CanonicalName)
	if !ok {
		return nil, usageError("UNKNOWN_OPERATION", "The operation is not in the embedded API contract.", "operation")
	}
	g := &mcpCallGate{entry: entry, policy: policy, session: mcpSessionFrom(ctx), approvalTimeout: mcpConfirmationDeadline}
	g.input = mcpGateInput{Local: cmd.CanonicalName == "history" || cmd.CanonicalName == "history.clear", Write: entry.Kind == "write", Gated: entry.ConfirmRequired(), Elicitation: g.session.canElicit()}
	controls, _ := args["_flint"].(map[string]any)
	for _, control := range []struct {
		name   string
		target *bool
	}{{"confirm", &g.input.Confirm}, {"live", &g.input.Live}} {
		if value, exists := controls[control.name]; exists {
			boolean, valid := value.(bool)
			if !valid {
				return nil, mcpInputError([]mcpInputProblem{mcpPlainProblem(mcpInputControlLocation, control.name, "INVALID_VALUE", control.name+" must be a boolean.")})
			}
			*control.target = boolean
		}
	}
	if raw, exists := controls["dry_run"]; exists {
		switch raw {
		case true, "client":
			g.input.DryRun = true
		case false:
		default:
			return nil, mcpInputError([]mcpInputProblem{mcpPlainProblem(mcpInputControlLocation, "dry_run", "INVALID_VALUE", "dry_run must be a boolean.")})
		}
	}
	if policy.readOnly && g.input.Write {
		return nil, g.failure(mcpGateDecision{Code: "OPERATION_NOT_AVAILABLE", Reason: "read_only_server"}, "")
	}
	if g.input.Local {
		g.environment = "sandbox"
	}
	return g, nil
}

func (g *mcpCallGate) afterAuth(auth AuthContext) *CLIError {
	g.environment, g.merchant = normalizeEnvironment(auth.Environment), auth.MerchantID
	call := g.input
	call.Environment, call.Write, call.Gated = g.environment, false, false
	decision := mcpDecideGate(g.policy, call)
	if decision.Action == mcpGateReject {
		return g.failure(decision, "")
	}
	return nil
}

func (g *mcpCallGate) prepared(ctx context.Context, a *App, cmd *Command, req preparedRequest, resolved ResolvedConfig) *CLIError {
	call := g.input
	call.Environment = g.environment
	decision := mcpDecideGate(g.policy, call)
	if decision.Action == mcpGateRun {
		return nil
	}
	if decision.Action == mcpGateReject {
		e := g.failure(decision, "")
		if decision.Code == "CONFIRMATION_NEEDS_USER" {
			command, err := a.cliCommandFor(cmd, req, resolved)
			if err != nil {
				failure := cliError(ExitSoftware, errorTypeInternal, "MCP_COMMAND_FAILED", "The terminal approval command could not be prepared. Ask the user to upgrade Flint CLI and restart this MCP server.")
				failure.Details = map[string]any{"reason": "fallback_unavailable"}
				failure.Cause = err
				return failure
			}
			details := e.Details.(map[string]any)
			details["command"], details["idempotency_key"] = command, req.IdempotencyKey
			details["same_project"] = resolved.SandboxGuard != ""
		}
		return e
	}
	return g.ask(ctx, req)
}

func (g *mcpCallGate) failure(decision mcpGateDecision, message string) *CLIError {
	if message == "" {
		switch decision.Code {
		case "OPERATION_NOT_AVAILABLE":
			message = "This MCP server is read-only and cannot run this write."
		case "LIVE_ACCESS_DISABLED":
			message = mcpLiveDisabledMessage
		case "LIVE_ACKNOWLEDGEMENT_REQUIRED":
			message = "This operation uses live data. Set live: true to acknowledge production access."
		case "CONFIRMATION_REQUIRED":
			message = "This operation requires confirm: true. Inspect its effect with dry_run: true first where available."
		case "CONFIRMATION_NEEDS_USER":
			message = "This live or unverified operation requires the user's approval. Ask the user to run the supplied terminal command."
		}
	}
	e := cliError(ExitConfirmation, errorTypeConfirmationRequired, decision.Code, message)
	if decision.Code == "OPERATION_NOT_AVAILABLE" {
		e.ExitCode, e.Type = ExitUsage, errorTypeUsage
	}
	if decision.Code == "LIVE_ACCESS_DISABLED" || decision.Code == "LIVE_ACKNOWLEDGEMENT_REQUIRED" {
		e.ExitCode, e.Type = ExitAuth, errorTypeConfiguration
	}
	e.Details = map[string]any{}
	if decision.Reason != "" {
		e.Details.(map[string]any)["reason"] = decision.Reason
	}
	return e
}

func (g *mcpCallGate) ask(ctx context.Context, req preparedRequest) *CLIError {
	effect := mcpPreparedEffect(g.entry, req)
	prompt := "Approve this " + g.environment + " Flint operation?\n\n" + g.entry.ID + ": " + g.entry.Title
	if g.merchant != "" {
		prompt += "\nMerchant: " + g.merchant + " (" + g.environment + ")"
	}
	for _, amount := range effect["amounts"].([]map[string]any) {
		prompt += "\nAmount: " + fmt.Sprint(amount["display"]) + " (" + fmt.Sprint(amount["field"]) + ")"
	}
	if resources := effect["resources"].([]string); len(resources) > 0 {
		prompt += "\nResources: " + strings.Join(resources, ", ")
	}
	if g.entry.Irreversible {
		prompt += "\nThis can't be undone through the API."
	} else if g.entry.Sensitive {
		prompt += "\nIt is sensitive: it moves money or affects buyers."
	}
	body := req.BodyValue
	if body == nil {
		body = map[string]any{}
	}
	raw, _ := mcpCompactJSON(body)
	if len(raw) > 2000 {
		cut := 2000
		for !utf8.Valid(raw[:cut]) {
			cut--
		}
		raw = append(raw[:cut], []byte("...")...)
	}
	prompt += "\n\nRequest: " + req.Method + " " + req.Path + "\nArguments: " + string(raw)
	if req.IdempotencyKey != "" {
		prompt += "\nIdempotency key: " + req.IdempotencyKey
	}
	requestCtx, cancel := context.WithTimeout(ctx, g.approvalTimeout)
	defer cancel()
	response, err := g.session.request(requestCtx, "elicitation/create", map[string]any{
		"message": prompt, "requestedSchema": map[string]any{"type": "object", "required": []string{"approve"}, "properties": map[string]any{"approve": map[string]any{"type": "boolean", "title": "Approve " + g.entry.ID, "default": false}}},
	})
	if err != nil {
		reason, message := "elicitation_failed", "Could not ask the user to approve this operation; it was not performed."
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			reason, message = "no_answer", "The user did not answer within 10 minutes; the operation was not performed."
		}
		return g.failure(mcpGateDecision{Code: "CONFIRMATION_DECLINED", Reason: reason}, message)
	}
	if response["error"] != nil {
		return g.failure(mcpGateDecision{Code: "CONFIRMATION_DECLINED", Reason: "elicitation_failed"}, "The MCP client could not ask the user to approve this operation; it was not performed.")
	}
	result, _ := response["result"].(map[string]any)
	content, _ := result["content"].(map[string]any)
	if result["action"] == "accept" && content["approve"] == true {
		return nil
	}
	return g.failure(mcpGateDecision{Code: "CONFIRMATION_DECLINED", Reason: "declined"}, "The user declined to approve this operation; it was not performed. Do not retry unless the user asks.")
}
