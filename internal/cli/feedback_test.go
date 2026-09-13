package cli

import "testing"

func TestFeedbackSecretFieldMatchesServerFixtures(t *testing.T) {
	t.Parallel()
	fixtures := []struct {
		name, value string
	}{
		{"Flint key", "flint_live_abcdefghijklmnop"},
		{"provider key", "sk_test_abcdefghijklmnop"},
		{"webhook secret", "whsec_abcdefghijklmnop"},
		{"authorization", "Authorization: Basic abcdefghijklmnop"},
		{"private key", "-----BEGIN PRIVATE KEY-----\nabcdefghijklmnop\n-----END PRIVATE KEY-----"},
		{"assignment", "access_token=abcdefghijklmnop"},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()
			path, matched := feedbackSecretField(map[string]any{"description": fixture.value}, "")
			if !matched || path != "description" {
				t.Fatalf("matched=%v path=%q", matched, path)
			}
		})
	}
	if path, matched := feedbackSecretField(map[string]any{"reproduction_steps": []any{"safe", "password=abcdefghijklmnop"}}, ""); !matched || path != "reproduction_steps[1]" {
		t.Fatalf("nested match=%v path=%q", matched, path)
	}
	if path, matched := feedbackSecretField(map[string]any{"description": "The error did not explain the required scope."}, ""); matched || path != "" {
		t.Fatalf("safe text matched=%v path=%q", matched, path)
	}
}

func TestFeedbackUnsafeIdempotencyKeyMatchesServerPolicy(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		"flint_live_abcdefghijklmnop",
		"developer@example.com",
		"line\nbreak",
		"hidden\u202eright-to-left",
	} {
		if !feedbackUnsafeIdempotencyKey(value) {
			t.Errorf("feedbackUnsafeIdempotencyKey(%q) = false, want true", value)
		}
	}
	if feedbackUnsafeIdempotencyKey("feedback-20260822-request-17") {
		t.Error("safe idempotency key was rejected")
	}
}

func TestFeedbackCreateRequestRecognizesEncodedRawAPIPaths(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"/v1/feedback-reports",
		"/v1/feedback-reports?source=cli",
		"/v1/%66eedback-reports",
		"/v1/feedback%2dreports",
	} {
		if !isFeedbackCreateRequest("POST", path) {
			t.Errorf("POST %q was not classified as feedback creation", path)
		}
		command := &Command{CanonicalName: "api", Method: "POST", APIPath: path}
		if !isFeedbackCreateCommand(command) {
			t.Errorf("command for %q was not classified as feedback creation", path)
		}
		if !mcpCommandTargetsFeedbackCreate(command, map[string]any{"method": "POST", "path": path}) {
			t.Errorf("MCP call for %q was not classified as feedback creation", path)
		}
	}
	for _, path := range []string{"/v1/feedback-reports-extra", "/v1/feedback-reports/", "https://example.com/v1/feedback-reports"} {
		if isFeedbackCreateRequest("POST", path) {
			t.Errorf("POST %q was incorrectly classified as feedback creation", path)
		}
	}
	if isFeedbackCreateRequest("GET", "/v1/feedback-reports") {
		t.Error("GET feedback list was classified as feedback creation")
	}
}
