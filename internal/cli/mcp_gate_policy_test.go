package cli

import (
	"fmt"
	"testing"

	"github.com/flint-pay/flint-cli/internal/opindex"
)

func TestMCPGatePolicyMatrix(t *testing.T) {
	run := mcpGateDecision{Action: mcpGateRun}
	ask := mcpGateDecision{Action: mcpGateAsk}
	required := mcpGateDecision{Action: mcpGateReject, Code: "CONFIRMATION_REQUIRED"}
	user := mcpGateDecision{Action: mcpGateReject, Code: "CONFIRMATION_NEEDS_USER"}
	disabled := mcpGateDecision{Action: mcpGateReject, Code: "LIVE_ACCESS_DISABLED"}
	acknowledge := mcpGateDecision{Action: mcpGateReject, Code: "LIVE_ACKNOWLEDGEMENT_REQUIRED"}
	readOnly := mcpGateDecision{Action: mcpGateReject, Code: "OPERATION_NOT_AVAILABLE", Reason: "read_only_server"}

	// The four expected columns are literal cells from spec 11.1, rather than
	// calculating expected outcomes with the same branches as the policy.
	type columns struct{ read, write, noConfirm, confirm mcpGateDecision }
	sandbox := columns{run, run, required, run}
	elicitation := columns{run, run, required, ask}
	fallback := columns{run, run, user, user}
	selfConfirm := columns{run, run, required, run}
	liveDisabled := columns{disabled, disabled, disabled, disabled}
	liveUnacknowledged := columns{acknowledge, acknowledge, acknowledge, acknowledge}
	scenarios := []struct {
		name        string
		environment string
		local       bool
		policy      mcpPolicy
		live        bool
		elicitation bool
		want        columns
	}{
		{"sandbox", "sandbox", false, mcpPolicy{}, false, false, sandbox},
		{"sandbox_elicitation", "sandbox", false, mcpPolicy{}, false, true, sandbox},
		{"sandbox_allow_destructive", "sandbox", false, mcpPolicy{allowDestructive: true}, false, false, sandbox},
		{"sandbox_all_flags", "sandbox", false, mcpPolicy{allowLive: true, allowDestructive: true}, true, true, sandbox},
		{"live_elicitation", "live", false, mcpPolicy{allowLive: true}, true, true, elicitation},
		{"live_fallback", "live", false, mcpPolicy{allowLive: true}, true, false, fallback},
		{"live_allow_destructive", "live", false, mcpPolicy{allowLive: true, allowDestructive: true}, true, false, selfConfirm},
		{"live_allow_destructive_with_elicitation", "live", false, mcpPolicy{allowLive: true, allowDestructive: true}, true, true, selfConfirm},
		{"live_disabled", "live", false, mcpPolicy{}, false, false, liveDisabled},
		{"live_disabled_destructive_cannot_override", "live", false, mcpPolicy{allowDestructive: true}, true, true, liveDisabled},
		{"live_missing_per_call_ack", "live", false, mcpPolicy{allowLive: true}, false, false, liveUnacknowledged},
		{"live_missing_ack_destructive_cannot_override", "live", false, mcpPolicy{allowLive: true, allowDestructive: true}, false, true, liveUnacknowledged},
		{"unverified_elicitation", "unverified", false, mcpPolicy{}, false, true, elicitation},
		{"unverified_fallback", "unverified", false, mcpPolicy{}, false, false, fallback},
		{"unverified_allow_destructive", "unverified", false, mcpPolicy{allowDestructive: true}, false, true, selfConfirm},
		{"local_live_credential", "live", true, mcpPolicy{}, false, true, sandbox},
		{"local_unverified_credential", "unverified", true, mcpPolicy{}, false, true, sandbox},
		{"local_all_flags", "live", true, mcpPolicy{allowLive: true, allowDestructive: true}, true, true, sandbox},
		{"test_alias", "test", false, mcpPolicy{}, false, true, sandbox},
		{"production_alias", "production", false, mcpPolicy{allowLive: true}, true, true, elicitation},
		{"unclassified_environment_does_not_imply_sandbox", "", false, mcpPolicy{}, false, true, elicitation},
	}
	operations := []struct {
		name  string
		entry opindex.Entry
	}{
		{"read", opindex.Entry{Kind: "read"}},
		{"additive_write", opindex.Entry{Kind: "write"}},
		{"overwrite_only", opindex.Entry{Kind: "write", Overwrites: true}},
		{"sensitive_refund", opindex.Entry{Kind: "write", Sensitive: true}},
		{"irreversible_write", opindex.Entry{Kind: "write", Irreversible: true}},
		{"sensitive_and_irreversible", opindex.Entry{Kind: "write", Sensitive: true, Irreversible: true}},
	}
	for _, scenario := range scenarios {
		for _, operation := range operations {
			for _, confirm := range []bool{false, true} {
				for _, dryRun := range []bool{false, true} {
					for _, restricted := range []bool{false, true} {
						name := fmt.Sprintf("%s/%s/confirm=%t/dry_run=%t/read_only=%t", scenario.name, operation.name, confirm, dryRun, restricted)
						t.Run(name, func(t *testing.T) {
							write := operation.entry.Kind == "write"
							want := scenario.want.read
							if write {
								want = scenario.want.write
								if operation.entry.ConfirmRequired() {
									want = scenario.want.noConfirm
									if confirm {
										want = scenario.want.confirm
									}
								}
							}
							if dryRun {
								want = run
							}
							if restricted && write {
								want = readOnly
							}
							policy := scenario.policy
							policy.readOnly = restricted
							call := mcpGateInput{Environment: scenario.environment, Local: scenario.local, Write: write, Gated: operation.entry.ConfirmRequired(), Confirm: confirm, DryRun: dryRun, Live: scenario.live, Elicitation: scenario.elicitation}
							if got := mcpDecideGate(policy, call); got != want {
								t.Fatalf("policy=%+v call=%+v: got %+v, want %+v", policy, call, got, want)
							}
						})
					}
				}
			}
		}
	}
}

func TestMCPGatePolicySandboxAndLocalFlagsCannotAddHumanApproval(t *testing.T) {
	for _, environment := range []string{"sandbox", "live", "unverified"} {
		for mask := 0; mask < 32; mask++ {
			policy := mcpPolicy{allowLive: mask&1 != 0, allowDestructive: mask&2 != 0}
			call := mcpGateInput{Environment: environment, Local: environment != "sandbox", Write: true, Gated: true, Confirm: mask&4 != 0, Live: mask&8 != 0, Elicitation: mask&16 != 0}
			want := mcpGateDecision{Action: mcpGateReject, Code: "CONFIRMATION_REQUIRED"}
			if call.Confirm {
				want = mcpGateDecision{Action: mcpGateRun}
			}
			if got := mcpDecideGate(policy, call); got != want {
				t.Fatalf("policy=%+v call=%+v: got %+v, want %+v", policy, call, got, want)
			}
		}
	}
}

func TestMCPGatePolicyUnverifiedAccessFlagsDoNotRefuseOrAcknowledge(t *testing.T) {
	for _, gated := range []bool{false, true} {
		for _, confirm := range []bool{false, true} {
			for _, elicitation := range []bool{false, true} {
				for _, allowDestructive := range []bool{false, true} {
					policy := mcpPolicy{allowDestructive: allowDestructive}
					call := mcpGateInput{Environment: environmentUnverified, Write: true, Gated: gated, Confirm: confirm, Elicitation: elicitation}
					want := mcpDecideGate(policy, call)
					for mask := 0; mask < 4; mask++ {
						policy.allowLive, call.Live = mask&1 != 0, mask&2 != 0
						if got := mcpDecideGate(policy, call); got != want {
							t.Fatalf("unverified behavior changed with live access flags: policy=%+v call=%+v: got %+v, want %+v", policy, call, got, want)
						}
					}
				}
			}
		}
	}
}
