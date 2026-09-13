package webhooksigning

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const SecretPrefix = "whsec_"

var (
	ErrInvalidStandardWebhookSignature = errors.New("invalid standard webhook signature")
	ErrStaleStandardWebhookTimestamp   = errors.New("standard webhook timestamp outside tolerance")
)

func GenerateSecret() (string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("generate webhook secret: %w", err)
	}
	return SecretPrefix + base64.StdEncoding.EncodeToString(secret), nil
}

func LegacySignature(secret string, timestamp int64, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", timestamp)
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

func LegacySignatureHeader(timestamp int64, payload []byte, secrets ...string) string {
	parts := []string{fmt.Sprintf("t=%d", timestamp)}
	for _, secret := range secrets {
		if strings.TrimSpace(secret) == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("v1=%s", LegacySignature(secret, timestamp, payload)))
	}
	return strings.Join(parts, ",")
}

func StandardWebhookSignature(secret, eventID string, timestamp int64, payload []byte) string {
	key, err := StandardWebhookSigningKey(secret)
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%s.%d.", eventID, timestamp)
	mac.Write(payload)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func StandardWebhookSignatureHeader(eventID string, timestamp int64, payload []byte, secrets ...string) string {
	parts := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		if strings.TrimSpace(secret) == "" {
			continue
		}
		signature := StandardWebhookSignature(secret, eventID, timestamp, payload)
		if signature == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("v1,%s", signature))
	}
	return strings.Join(parts, " ")
}

func StandardWebhookSigningKey(secret string) ([]byte, error) {
	encoded, ok := strings.CutPrefix(secret, SecretPrefix)
	if !ok {
		return nil, fmt.Errorf("standard webhook secret must use %s prefix", SecretPrefix)
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode standard webhook secret: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("standard webhook secret must contain 32 bytes")
	}
	return key, nil
}

// VerifyStandardWebhook verifies the Standard Webhooks headers against the
// unmodified request body. At least one v1 signature must match.
func VerifyStandardWebhook(secret, eventID, timestampHeader, signatureHeader string, payload []byte, now time.Time, tolerance time.Duration) error {
	eventID = strings.TrimSpace(eventID)
	if eventID == "" {
		return fmt.Errorf("%w: webhook-id is required", ErrInvalidStandardWebhookSignature)
	}
	timestamp, err := strconv.ParseInt(strings.TrimSpace(timestampHeader), 10, 64)
	if err != nil {
		return fmt.Errorf("%w: webhook-timestamp is invalid", ErrInvalidStandardWebhookSignature)
	}
	if tolerance > 0 {
		delta := now.UTC().Sub(time.Unix(timestamp, 0).UTC())
		if delta < 0 {
			delta = -delta
		}
		if delta > tolerance {
			return ErrStaleStandardWebhookTimestamp
		}
	}
	expected := StandardWebhookSignature(secret, eventID, timestamp, payload)
	if expected == "" {
		return fmt.Errorf("%w: signing secret is invalid", ErrInvalidStandardWebhookSignature)
	}
	for _, part := range strings.Fields(signatureHeader) {
		version, candidate, ok := strings.Cut(strings.TrimSpace(part), ",")
		if !ok || version != "v1" {
			continue
		}
		if hmac.Equal([]byte(expected), []byte(candidate)) {
			return nil
		}
	}
	return ErrInvalidStandardWebhookSignature
}
