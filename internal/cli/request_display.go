package cli

import "strings"

// Display copies must never change the credential sent in the real request.
func displayRequestBody(value any) (any, bool) {
	sensitive := false
	switch node := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(node))
		for key, item := range node {
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
			masked, secret := displayRequestBody(item)
			out[key], sensitive = masked, sensitive || secret
		}
		return out, sensitive
	case []any:
		out := make([]any, len(node))
		for i, item := range node {
			masked, secret := displayRequestBody(item)
			out[i], sensitive = masked, sensitive || secret
		}
		return out, sensitive
	default:
		return value, false
	}
}
