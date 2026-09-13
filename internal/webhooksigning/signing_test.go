package webhooksigning

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestStandardSignatureConformance(t *testing.T) {
	const secret = "whsec_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	const eventID = "whev_test"
	const timestamp int64 = 1700000000
	payload := []byte(`{"id":"pi_123","status":"succeeded"}`)

	if got, want := StandardWebhookSignature(secret, eventID, timestamp, payload), "/hiJOnBg47YJCiwbXhOB2ygLlyQXGu6CHw8D3A/KW5o="; got != want {
		t.Fatalf("signature = %q, want %q", got, want)
	}
	if got, want := StandardWebhookSignatureHeader(eventID, timestamp, payload, secret), "v1,/hiJOnBg47YJCiwbXhOB2ygLlyQXGu6CHw8D3A/KW5o="; got != want {
		t.Fatalf("signature header = %q, want %q", got, want)
	}
}

func TestVerifyStandardWebhook(t *testing.T) {
	const secret = "whsec_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	const eventID = "whev_test"
	now := time.Unix(1700000000, 0).UTC()
	payload := []byte(`{"id":"pi_123","status":"succeeded"}`)
	signature := StandardWebhookSignatureHeader(eventID, now.Unix(), payload, secret)

	if err := VerifyStandardWebhook(secret, eventID, "1700000000", signature, payload, now, 5*time.Minute); err != nil {
		t.Fatalf("VerifyStandardWebhook: %v", err)
	}
	if err := VerifyStandardWebhook(secret, eventID, "1700000000", "v1,bad "+signature, payload, now, 5*time.Minute); err != nil {
		t.Fatalf("VerifyStandardWebhook with overlapping signatures: %v", err)
	}
	if err := VerifyStandardWebhook(secret, eventID, "1700000000", signature, []byte(`{}`), now, 5*time.Minute); !errors.Is(err, ErrInvalidStandardWebhookSignature) {
		t.Fatalf("tampered payload error = %v", err)
	}
	if err := VerifyStandardWebhook(secret, eventID, "1700000000", signature, payload, now.Add(6*time.Minute), 5*time.Minute); !errors.Is(err, ErrStaleStandardWebhookTimestamp) {
		t.Fatalf("stale timestamp error = %v", err)
	}
}

func TestGenerateSecretProducesCanonicalKey(t *testing.T) {
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	if !strings.HasPrefix(secret, SecretPrefix) {
		t.Fatalf("secret = %q", secret)
	}
	key, err := StandardWebhookSigningKey(secret)
	if err != nil {
		t.Fatalf("StandardWebhookSigningKey: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("key length = %d, want 32", len(key))
	}
}

func TestSigningKeyRejectsMalformedSecrets(t *testing.T) {
	for _, secret := range []string{"secret", "whsec_not-base64", "whsec_YQ=="} {
		if _, err := StandardWebhookSigningKey(secret); err == nil {
			t.Fatalf("StandardWebhookSigningKey(%q) succeeded", secret)
		}
	}
}
