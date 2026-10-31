package cli

import (
	"reflect"
	"testing"
)

func TestSandboxTestTokensPinned(t *testing.T) {
	want := []string{"pm_card_visa", "pm_card_threeDSecure2Required", "pm_card_authenticationRequired", "pm_card_chargeDeclinedInsufficientFunds", "pm_card_chargeDeclined", "pm_card_createDispute", "pm_card_createDisputeInquiry", "pm_card_createDisputeProductNotReceived", "pm_card_riskLevelElevated", "pm_card_riskLevelHighest", "pm_card_radarBlock", "pm_card_bypassPending"}
	got := []string{}
	for _, token := range sandboxTestTokens() {
		got = append(got, token["token"])
		if token["scenario"] == "" || token["behavior"] == "" {
			t.Fatalf("missing scenario/behavior for %s", token["token"])
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("test tokens changed: got %v, want %v", got, want)
	}
}
