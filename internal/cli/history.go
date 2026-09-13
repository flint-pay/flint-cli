package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type History struct {
	Entries []HistoryEntry `json:"entries"`
}
type HistoryEntry struct {
	CredentialScope string    `json:"credential_scope,omitempty"`
	ID              string    `json:"id"`
	ResourceType    string    `json:"resource_type"`
	Command         string    `json:"command"`
	Profile         string    `json:"profile"`
	Environment     string    `json:"environment"`
	MerchantID      string    `json:"merchant_id,omitempty"`
	SandboxID       string    `json:"sandbox_id,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

var resourcePrefixes = buildResourcePrefixes()

func buildResourcePrefixes() map[string]string {
	prefixes := map[string]string{
		"pi_": "payment_intent", "ord_": "order", "cus_": "customer", "ref_": "refund", "cs_": "checkout_session",
		"whep_": "webhook_endpoint", "whev_": "webhook_event", "wdel_": "webhook_delivery", "rlog_": "request_log", "inv_": "invoice", "org_": "organization",
		"fbr_": "feedback_report",
		"mer_": "merchant", "test_": "sandbox", "key_": "api_key", "pl_": "payment_link", "pm_": "payment_method",
		"dls_": "delivery_location_set", "dlsr_": "delivery_location_set_revision", "dmet_": "delivery_method", "dmetr_": "delivery_method_revision",
		"dprof_": "delivery_profile", "dprofr_": "delivery_profile_revision", "dqt_": "delivery_quote", "dcb_": "delivery_rate_callback",
		"dcbr_": "delivery_rate_callback_revision", "drate_": "delivery_rate", "drev_": "delivery_revocation", "dsel_": "delivery_selection",
		"dzone_": "delivery_zone", "dzoner_": "delivery_zone_revision", "fev_": "fulfillment_event", "ful_": "fulfillment",
		"fnt_": "fulfillment_notification", "loc_": "location", "pkg_": "shipment_package", "pki_": "shipment_package_item",
		"ret_": "return", "shp_": "fulfillment_shipment",
	}
	names := make([]string, 0, len(publicResourceIDPrefixes))
	for name := range publicResourceIDPrefixes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		prefix := publicResourceIDPrefixes[name]
		if _, exists := prefixes[prefix]; exists {
			continue
		}
		if strings.HasPrefix(name, "expected_") || strings.HasPrefix(name, "corrects_") || strings.HasPrefix(name, "replaces_") || strings.HasPrefix(name, "after_") || name == "resolution_id" {
			continue
		}
		prefixes[prefix] = strings.TrimSuffix(name, "_id")
	}
	return prefixes
}

var historyQualifiers = func() map[string]string {
	qualifiers := map[string]string{}
	for prefix := range resourcePrefixes {
		qualifiers[strings.TrimSuffix(prefix, "_")] = prefix
	}
	return qualifiers
}()

type historyScope struct {
	CredentialScope string
	Profile         string
	Environment     string
	MerchantID      string
	SandboxID       string
}

func historyScopeFromResolved(resolved ResolvedConfig) historyScope {
	return historyScope{
		CredentialScope: resolved.CredentialScope,
		Profile:         resolved.ProfileName,
		Environment:     resolved.Environment,
		MerchantID:      resolved.MerchantID,
		SandboxID:       resolved.SandboxID,
	}
}

func (a *App) historyPath() (string, error) {
	dir, err := a.configDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "history.json"), nil
}
func (a *App) loadHistory() (History, error) {
	var h History
	path, err := a.historyPath()
	if err != nil {
		return h, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return h, err
	}
	if err := strictJSON(b, &h); err != nil {
		return h, fmt.Errorf("parse %s: %w", path, err)
	}
	return h, nil
}
func (a *App) saveHistory(h History) error {
	path, err := a.historyPath()
	if err != nil {
		return err
	}
	return withLocalFileLock(path, func() error {
		return a.saveHistoryUnlocked(h)
	})
}

func (a *App) saveHistoryUnlocked(h History) error {
	path, err := a.historyPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), "history-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (a *App) updateHistory(update func(*History) error) error {
	path, err := a.historyPath()
	if err != nil {
		return err
	}
	return withLocalFileLock(path, func() error {
		history, err := a.loadHistory()
		if err != nil {
			return err
		}
		if err := update(&history); err != nil {
			return err
		}
		return a.saveHistoryUnlocked(history)
	})
}

func (a *App) recordHistory(value any, cmd string, scope historyScope) error {
	ids := map[string]bool{}
	collectIDs(value, ids)
	if len(ids) == 0 {
		return nil
	}
	now := a.Now()
	orderedIDs := make([]string, 0, len(ids))
	primaryPrefix := historyPrefixForCommand(cmd)
	primaryID := primaryHistoryID(value, primaryPrefix)
	if primaryID != "" && ids[primaryID] {
		orderedIDs = append(orderedIDs, primaryID)
		delete(ids, primaryID)
	}
	remainingIDs := make([]string, 0, len(ids))
	for id := range ids {
		remainingIDs = append(remainingIDs, id)
	}
	sort.Slice(remainingIDs, func(i, j int) bool {
		iPrimary := primaryPrefix != "" && strings.HasPrefix(remainingIDs[i], primaryPrefix)
		jPrimary := primaryPrefix != "" && strings.HasPrefix(remainingIDs[j], primaryPrefix)
		if iPrimary != jPrimary {
			return iPrimary
		}
		return remainingIDs[i] < remainingIDs[j]
	})
	orderedIDs = append(orderedIDs, remainingIDs...)
	return a.updateHistory(func(h *History) error {
		for _, id := range orderedIDs {
			prefix, typ := classifyID(id)
			if prefix == "" {
				continue
			}
			_ = prefix
			filtered := h.Entries[:0]
			for _, existing := range h.Entries {
				if existing.ID == id && historyEntryMatchesScope(existing, scope) {
					continue
				}
				filtered = append(filtered, existing)
			}
			h.Entries = append(filtered, HistoryEntry{
				CredentialScope: scope.CredentialScope,
				ID:              id,
				ResourceType:    typ,
				Command:         cmd,
				Profile:         scope.Profile,
				Environment:     scope.Environment,
				MerchantID:      scope.MerchantID,
				SandboxID:       scope.SandboxID,
				CreatedAt:       now,
			})
		}
		sort.SliceStable(h.Entries, func(i, j int) bool { return h.Entries[i].CreatedAt.After(h.Entries[j].CreatedAt) })
		if len(h.Entries) > 200 {
			h.Entries = h.Entries[:200]
		}
		return nil
	})
}

func historyPrefixForCommand(command string) string {
	namespace := strings.SplitN(strings.TrimSpace(command), ".", 2)[0]
	return map[string]string{
		"payment-intents": "pi_", "orders": "ord_", "customers": "cus_", "refunds": "ref_",
		"checkout-sessions": "cs_", "webhook-endpoints": "whep_", "webhook-events": "whev_", "webhook-deliveries": "wdel_",
		"request-logs": "rlog_", "invoices": "inv_", "organizations": "org_", "merchants": "mer_",
		"feedback-reports": "fbr_",
		"sandboxes":        "test_", "api-keys": "key_", "payment-links": "pl_",
		"packages": "pkg_", "shipments": "shp_",
	}[namespace]
}

func primaryHistoryID(value any, prefix string) string {
	if prefix == "" {
		return ""
	}
	resourceType := resourcePrefixes[prefix]
	data, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	if nested, exists := data["data"]; exists {
		data, _ = nested.(map[string]any)
	}
	if data == nil {
		return ""
	}
	for _, key := range []string{resourceType + "_id", "id"} {
		if id, ok := data[key].(string); ok && strings.HasPrefix(id, prefix) {
			return id
		}
	}
	if nested, ok := data[resourceType].(map[string]any); ok {
		for _, key := range []string{resourceType + "_id", "id"} {
			if id, ok := nested[key].(string); ok && strings.HasPrefix(id, prefix) {
				return id
			}
		}
	}
	return ""
}

func collectIDs(value any, out map[string]bool) {
	switch v := value.(type) {
	case map[string]any:
		for key, x := range v {
			// Credentials and caller-authored metadata can resemble resource IDs.
			// Only ID fields and nested resource objects belong in local history.
			if key == "metadata" || key == "request" || key == "response" || key == "headers" || key == "body" || strings.Contains(key, "secret") || strings.Contains(key, "token") {
				continue
			}
			if key == "id" || strings.HasSuffix(key, "_id") || strings.HasSuffix(key, "_ids") {
				collectIDs(x, out)
				continue
			}
			switch x.(type) {
			case map[string]any, []any:
				collectIDs(x, out)
			}
		}
	case []any:
		for _, x := range v {
			collectIDs(x, out)
		}
	case string:
		if prefix, _ := classifyID(v); prefix != "" {
			out[v] = true
		}
	}
}
func classifyID(id string) (string, string) {
	for p, t := range resourcePrefixes {
		if strings.HasPrefix(id, p) && len(id) > len(p) {
			return p, t
		}
	}
	return "", ""
}

func (a *App) resolveHistoryRef(ref, expectedPrefix string, scope historyScope) (string, *CLIError) {
	if !strings.HasPrefix(ref, "@last") {
		return ref, nil
	}
	prefix := expectedPrefix
	if qualifier, ok := strings.CutPrefix(ref, "@last."); ok {
		var exists bool
		prefix, exists = historyQualifiers[qualifier]
		if !exists {
			return "", usageError("INVALID_HISTORY_REFERENCE", "Unknown history qualifier: "+qualifier+".", "history")
		}
	}
	if prefix == "" {
		return "", usageError("AMBIGUOUS_HISTORY_REFERENCE", "This argument requires a prefix-qualified history reference such as @last.pi.", "history")
	}
	h, err := a.loadHistory()
	if err != nil {
		return "", &CLIError{ExitCode: ExitUsage, Type: "usage_error", Code: "HISTORY_READ_FAILED", Message: err.Error(), Cause: err}
	}
	for _, e := range h.Entries {
		if historyEntryMatchesScope(e, scope) && strings.HasPrefix(e.ID, prefix) {
			return e.ID, nil
		}
	}
	e := usageError("HISTORY_REFERENCE_NOT_FOUND", fmt.Sprintf("No recent %s resource exists for profile %s in %s. Run flint history.", prefix, scope.Profile, scope.Environment), "history")
	return "", e
}

func historyEntryMatchesScope(entry HistoryEntry, scope historyScope) bool {
	if entry.CredentialScope != scope.CredentialScope || entry.Profile != scope.Profile || entry.Environment != scope.Environment {
		return false
	}
	if scope.MerchantID != "" && entry.MerchantID != scope.MerchantID {
		return false
	}
	if scope.SandboxID != "" && entry.SandboxID != scope.SandboxID {
		return false
	}
	return true
}
