package cli

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const mcpRedacted = "[REDACTED]"

// A schema marks credentials with ambiguous names such as url and key. The
// fallback also covers secret fields in untyped data, errors and newer responses.
func mcpSecretField(name string) bool {
	name = strings.ToLower(strings.ReplaceAll(name, "-", "_"))
	switch name {
	case "page_token", "next_page_token", "previous_page_token":
		return false
	case "secret_key", "authorization", "admission_code", "device_code", "user_code",
		"test_authorization_code", "live_authorization_code", "verification_uri_complete",
		"access_url", "access_link", "password", "private_key", "secrets",
		"clientsecret", "secretkey", "accesskey", "accesstoken", "refreshtoken", "signingsecret":
		return true
	}
	return name == "secret" || name == "token" || strings.HasSuffix(name, "_secret") ||
		strings.HasSuffix(name, "_token") || strings.HasSuffix(name, "_password")
}

var mcpCredentialPattern = regexp.MustCompile(`\b(?:(?:flint_(?:test|live|cses|cref)_|fpat_|fwat_)[A-Za-z0-9_~-]+|whsec_[A-Za-z0-9_+/-]+={0,2}|ckat_v1\.[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*|eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+|(?:pi|seti)_[A-Za-z0-9]+_secret_[A-Za-z0-9_]+)`)

// APIKey.key_prefix is a display identifier containing only eight hex digits
// from the random part. Full keys in free-form fields still match the detector.
var mcpPublicKeyPrefix = regexp.MustCompile(`^flint_(?:test|live)_(?:int_)?[0-9a-fA-F]{8}$`)

const mcpInvoicePDFPath = "/data/content_base64"
const mcpGiftCardChallengeAction = "complete_gift_card_challenge"

func mcpRedactCredentialText(text string) string {
	return mcpCredentialPattern.ReplaceAllString(text, mcpRedacted)
}

func mcpCredentialURL(text string) bool {
	if !strings.HasPrefix(text, "https://") && !strings.HasPrefix(text, "http://") {
		return false
	}
	link, err := url.Parse(text)
	if err != nil {
		return false
	}
	if link.User != nil {
		return true
	}
	fragment, _ := url.ParseQuery(link.Fragment)
	for _, params := range []url.Values{link.Query(), fragment} {
		for name := range params {
			switch strings.ToLower(name) {
			case "x-amz-signature", "x-goog-signature", "signature", "sig":
				return true
			}
			if mcpSecretField(name) {
				return true
			}
		}
	}
	return false
}

// mcpSchemaSecrets finds marked values before projection can rename, filter, or
// extract them. Any matching schema branch can mark a value secret.
func mcpSchemaSecrets(value any, path string, schema map[string]any, fields map[string]any, refs map[string]bool) {
	if value == nil || schema == nil {
		return
	}
	if marked, _ := schema["x-flint-secret"].(bool); marked || schema["format"] == "password" {
		fields[path] = value
		return
	}
	if ref, _ := schema["$ref"].(string); ref != "" && !refs[ref] {
		if component, ok := resolveComponentReference(ref); ok {
			refs[ref] = true
			mcpSchemaSecrets(value, path, component, fields, refs)
			delete(refs, ref)
		}
	}
	for _, keyword := range []string{"allOf", "anyOf", "oneOf"} {
		branches, _ := schema[keyword].([]any)
		for _, branch := range branches {
			mcpSchemaSecrets(value, path, mcpObject(branch), fields, refs)
		}
	}
	switch node := value.(type) {
	case map[string]any:
		properties := mcpObject(schema["properties"])
		for name, item := range node {
			child := mcpObject(properties[name])
			if child == nil {
				child = mcpObject(schema["additionalProperties"])
			}
			// A recursive reference can be visited again after descending
			// into a child value.
			if child != nil {
				mcpSchemaSecrets(item, mcpSecretPath(path, name), child, fields, map[string]bool{})
			}
		}
	case []any:
		for i, item := range node {
			mcpSchemaSecrets(item, path+"/"+strconv.Itoa(i), mcpObject(schema["items"]), fields, map[string]bool{})
		}
	}
}

func mcpObject(value any) map[string]any {
	object, _ := value.(map[string]any)
	return object
}

var mcpPointerEscape = strings.NewReplacer("~", "~0", "/", "~1")

func mcpSecretPath(parent, name string) string {
	return parent + "/" + mcpPointerEscape.Replace(name)
}

func mcpResponseSecrets(operationID string, value any) map[string]any {
	fields := map[string]any{}
	op, _ := openAPIOperationByID(operationID)
	for status, response := range op.Responses {
		if strings.HasPrefix(status, "2") {
			jsonContent := mcpObject(mcpObject(response["content"])["application/json"])
			mcpSchemaSecrets(value, "", mcpObject(jsonContent["schema"]), fields, map[string]bool{})
		}
	}
	// These responses contain capabilities that the current spec does not mark.
	switch operationID {
	case "createGiftCard", "rotateGiftCardCode":
		if code := mcpObject(mcpObject(value)["data"])["code"]; code != nil {
			fields["/data/code"] = code
		}
	case "getInvoicePDF", "getMeInvoicePDF":
		// Open-invoice PDFs embed a bearer link in the button and QR code.
		if content := mcpObject(mcpObject(value)["data"])["content_base64"]; content != nil {
			fields[mcpInvoicePDFPath] = content
		}
	case "authorizePartnerInstall":
		if location := mcpObject(mcpObject(value)["data"])["url"]; location != nil {
			fields["/data/url"] = location
		}
	}
	return fields
}

func mcpMaskSecret(name string, value any) string {
	if text, ok := value.(string); ok {
		if name == "secret_key" {
			for _, prefix := range []string{"flint_test_", "flint_live_"} {
				if strings.HasPrefix(text, prefix) && len(text) > len(prefix) {
					return prefix + "…"
				}
			}
		}
		if strings.HasPrefix(text, "whsec_") && len(text) > len("whsec_") {
			return "whsec_…"
		}
	}
	return mcpRedacted
}

func mcpMaskResult(value any, path string, fields map[string]any) any {
	if secret, present := fields[path]; present {
		return mcpMaskSecret(filepath.Base(path), secret)
	}
	switch node := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(node))
		for name, item := range node {
			child := mcpSecretPath(path, name)
			text, isString := item.(string)
			_, hasKeyID := node["api_key_id"].(string)
			if name == "key_prefix" && hasKeyID && mcpPublicKeyPrefix.MatchString(text) {
				if _, marked := fields[child]; !marked {
					out[name] = item
					continue
				}
			}
			if item != nil && (mcpSecretField(name) || (name == "api_key" && isString) ||
				(name == "url" && (strings.HasSuffix(path, "/gift_card_challenge") || node["action_type"] == mcpGiftCardChallengeAction))) {
				fields[child] = item
			}
			out[name] = mcpMaskResult(item, child, fields)
		}
		return out
	case []any:
		out := make([]any, len(node))
		for i, item := range node {
			out[i] = mcpMaskResult(item, path+"/"+strconv.Itoa(i), fields)
		}
		return out
	case string:
		if mcpRedactCredentialText(node) != node || mcpCredentialURL(node) {
			fields[path] = node
			return mcpRedacted
		}
	}
	return value
}

// Remove exact copies of captured credentials that also appear in messages
// or untyped fields. Longest first handles credentials with a shared prefix.
func mcpSecretEchoes(fields map[string]any) *strings.Replacer {
	unique := map[string]bool{}
	var collect func(any)
	collect = func(value any) {
		switch node := value.(type) {
		case string:
			if len(node) >= 8 && node != mcpRedacted {
				unique[node] = true
			}
		case map[string]any:
			for key, item := range node {
				if mcpSecretField(key) {
					collect(item)
				} else if mcpObject(item) != nil {
					collect(item)
				}
			}
		}
	}
	for _, value := range fields {
		collect(value)
	}
	values := make([]string, 0, len(unique))
	for value := range unique {
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	pairs := make([]string, 0, len(values)*2)
	for _, value := range values {
		pairs = append(pairs, value, mcpRedacted)
	}
	return strings.NewReplacer(pairs...)
}

func mcpRemoveSecretEchoes(value any, echoes *strings.Replacer) any {
	switch node := value.(type) {
	case map[string]any:
		for key, item := range node {
			// A credential can appear as a key in a free-form JSON object.
			if echoes.Replace(mcpRedactCredentialText(key)) != key {
				delete(node, key)
				continue
			}
			node[key] = mcpRemoveSecretEchoes(item, echoes)
		}
	case []any:
		for i, item := range node {
			node[i] = mcpRemoveSecretEchoes(item, echoes)
		}
	case string:
		return echoes.Replace(node)
	}
	return value
}

func (a *App) saveMCPSecrets(fields map[string]any) (string, error) {
	configDir, err := a.configDirectory()
	if err != nil {
		return "", err
	}
	dir, err := filepath.Abs(filepath.Join(configDir, "mcp-secrets"))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	callDir, err := os.MkdirTemp(dir, "result-")
	if err != nil {
		return "", err
	}
	if content, ok := fields[mcpInvoicePDFPath].(string); ok && len(fields) == 1 {
		path := filepath.Join(callDir, "invoice.pdf")
		_, downloadErr := a.saveDownload(map[string]any{"data": map[string]any{
			"content_base64": content, "content_type": "application/pdf",
		}}, path)
		if downloadErr != nil {
			_ = os.Remove(callDir)
			return "", downloadErr
		}
		return path, nil
	}
	path := filepath.Join(callDir, "secrets.json")
	if err := writePrivateJSON(path, fields); err != nil {
		_ = os.Remove(callDir)
		return "", err
	}
	return path, nil
}

func maskMCPResult(operationID string, value any, warnings string) (any, string, map[string]any) {
	fields := mcpResponseSecrets(operationID, value)
	value = mcpMaskResult(value, "", fields)
	warnings = mcpRedactCredentialText(warnings)
	echoes := mcpSecretEchoes(fields)
	return mcpRemoveSecretEchoes(value, echoes), echoes.Replace(warnings), fields
}

func (a *App) mcpSecretNote(fields map[string]any) string {
	if len(fields) == 0 {
		return ""
	}
	path, err := a.saveMCPSecrets(fields)
	if err != nil {
		return "\nWarning: Flint could not save the redacted values. Check the result before retrying. Use your terminal or Dashboard to recover or reissue them."
	}
	return fmt.Sprintf("\nNote: Flint saved redacted values in %s. Only the user should open this file on the machine running Flint and delete it after use.", path)
}
