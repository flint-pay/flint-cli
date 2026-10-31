package cli

// Keep these fixed test aliases in sync with the Flint testing reference.
// Card numbers belong in hosted checkout forms, never in API token fields.
func sandboxTestTokens() []map[string]string {
	return []map[string]string{
		{"scenario": "success", "token": "pm_card_visa", "behavior": "Payment succeeds."},
		{"scenario": "3d_secure", "token": "pm_card_threeDSecure2Required", "behavior": "Requires a 3D Secure 2 browser challenge."},
		{"scenario": "authentication_required", "token": "pm_card_authenticationRequired", "behavior": "Requires browser authentication."},
		{"scenario": "insufficient_funds", "token": "pm_card_chargeDeclinedInsufficientFunds", "behavior": "Payment fails with insufficient_funds."},
		{"scenario": "generic_decline", "token": "pm_card_chargeDeclined", "behavior": "Payment fails with card_declined."},
		{"scenario": "dispute_fraudulent", "token": "pm_card_createDispute", "behavior": "Payment succeeds, then receives a fraudulent dispute."},
		{"scenario": "dispute_inquiry", "token": "pm_card_createDisputeInquiry", "behavior": "Payment succeeds, then receives an inquiry."},
		{"scenario": "dispute_product_not_received", "token": "pm_card_createDisputeProductNotReceived", "behavior": "Payment succeeds, then receives a product-not-received dispute."},
		{"scenario": "risk_elevated", "token": "pm_card_riskLevelElevated", "behavior": "Elevated processor risk level; merchant risk rules still apply."},
		{"scenario": "risk_highest", "token": "pm_card_riskLevelHighest", "behavior": "Highest processor risk level; merchant risk rules still apply."},
		{"scenario": "risk_block", "token": "pm_card_radarBlock", "behavior": "Blocked by processor fraud screening."},
		{"scenario": "available_balance", "token": "pm_card_bypassPending", "behavior": "Test funds bypass the pending balance."},
	}
}
