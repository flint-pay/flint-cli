package cli

import "strings"

// Display copies must never change the credential sent in the real request.
func displayRequestBody(value any, operationID ...string) (any, bool) {
	return redactRequestBody(value, len(operationID) > 0 && hasLicenseKeyBody(operationID[0]))
}

func hasLicenseKeyBody(operationID string) bool {
	switch operationID {
	case "lookupLicenseKey", "validateLicenseKey", "createLicenseKeyActivation", "releaseLicenseKeyActivation", "createLicenseKeyPoolKeys", "removeLicenseKeyPoolKeys":
		return true
	}
	return false
}

func redactRequestBody(value any, licenseKeys bool) (any, bool) {
	sensitive := false
	switch node := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(node))
		for key, item := range node {
			if licenseKeys && (key == "key" || key == "keys") {
				out[key], sensitive = "[REDACTED]", true
				continue
			}
			if key == "admission_code" {
				sensitive = true
				out[key] = "[REDACTED]"
				if code, ok := item.(string); ok {
					code = strings.NewReplacer("-", "", " ", "").Replace(code)
					if code != "[REDACTED]" && len(code) > 4 {
						out[key] = "[REDACTED]" + code[len(code)-4:]
					}
				}
				continue
			}
			masked, secret := redactRequestBody(item, licenseKeys)
			out[key], sensitive = masked, sensitive || secret
		}
		return out, sensitive
	case []any:
		out := make([]any, len(node))
		for i, item := range node {
			masked, secret := redactRequestBody(item, licenseKeys)
			out[i], sensitive = masked, sensitive || secret
		}
		return out, sensitive
	default:
		return value, false
	}
}
