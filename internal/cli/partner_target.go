package cli

import (
	"context"
	"net/http"
	"strings"

	"github.com/flint-pay/flint-cli/internal/opindex"
)

type partnerTargetKey struct{}
type partnerTarget struct{ merchant, sandbox string }

// Carry the target through preparation reads, pagination, retries, and MCP calls.
// Other credential classes continue to use these settings only as guards.
func withPartnerTarget(ctx context.Context, key string, resolved ResolvedConfig) context.Context {
	if !opindex.ClassifyCredential(opindex.CredentialAccessToken, key).Partner {
		return ctx
	}
	return context.WithValue(ctx, partnerTargetKey{}, partnerTarget{resolved.MerchantGuard, resolved.SandboxGuard})
}

func partnerMerchantGuidance(e *CLIError, key string) {
	if !opindex.ClassifyCredential(opindex.CredentialAccessToken, key).Partner {
		return
	}
	if (e.Param == "Flint-Merchant-Id" || strings.Contains(e.Message, "Flint-Merchant-Id")) && (e.Code == "INVALID_REQUEST" || e.Code == "VALUE_REQUIRED") && strings.Contains(strings.ToLower(e.Message), "required") {
		e.Guidance = "Pass --merchant <mer_id> (or set FLINT_MERCHANT) to select the connected merchant. For MCP, set the server's --merchant or this call's merchant control."
		e.Message += " " + e.Guidance
	}
}

func setPartnerTargetHeaders(req *http.Request) {
	if target, ok := req.Context().Value(partnerTargetKey{}).(partnerTarget); ok {
		if target.merchant != "" {
			req.Header.Set("Flint-Merchant-Id", target.merchant)
		}
		if target.sandbox != "" {
			req.Header.Set("Flint-Sandbox-Id", target.sandbox)
		}
	}
}
