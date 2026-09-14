package cli

import (
	"sort"
	"strings"
	"unicode"
)

// publicAPICommands derives commands from the same contract used by callers.
// Existing commands provide curated shortcuts; other operations receive
// route-shaped commands unless explicitly excluded below. Generated commands
// include the complete request schema and query arguments.
func publicAPICommands(existing []*Command) []*Command {
	doc, err := loadOpenAPI()
	if err != nil {
		panic("load public API commands: " + err.Error())
	}
	covered := map[string]bool{}
	for _, command := range existing {
		covered[command.OperationID] = true
	}
	var commands []*Command
	for apiPath, methods := range doc.Paths {
		// Feedback is handled through Flint Help, not dedicated CLI commands.
		if apiPath == "/v1/feedback-reports" || strings.HasPrefix(apiPath, "/v1/feedback-reports/") {
			continue
		}
		for method, operation := range methods {
			upper := strings.ToUpper(method)
			if !strings.Contains(" GET POST PUT PATCH DELETE ", " "+upper+" ") || covered[operation.OperationID] {
				continue
			}
			name := publicAPICommandName(apiPath, upper, operation)
			name = alignPublicCommandPrefix(name, apiPath, existing)
			input := requestSchemaName(operation.RequestBody)
			arguments := publicAPIArguments(apiPath, operation)
			if operation.RequestBody != nil {
				arguments = append(arguments, flag("input", "string", "", "", "JSON request body file, or - for stdin", false))
			}
			description := strings.TrimSpace(operation.Summary)
			if description == "" {
				description = humanizeOperationID(operation.OperationID)
			}
			// OAuth authorization creates a grant even though its protocol uses GET.
			authorization := operation.OperationID == "authorizePartnerInstall"
			command := apiCommand(name, description, upper, apiPath, operation.OperationID, input,
				responseSchemaName(operation.Responses), arguments, upper != "GET" || authorization, authorization,
				operation.FlintDestructive || upper == "DELETE", upper == "GET" && !authorization && !strings.HasSuffix(name, ".list"),
				publicAPIExample(name, arguments, operation.RequestBody != nil, operation.FlintDestructive || upper == "DELETE"))
			commands = append(commands, command)
		}
	}
	sort.Slice(commands, func(i, j int) bool { return commands[i].Name < commands[j].Name })
	return commands
}

func alignPublicCommandPrefix(name, apiPath string, existing []*Command) string {
	longest := 0
	aligned := name
	for _, command := range existing {
		base := command.APIPath
		if base == "" || strings.Contains(base, "{") || len(base) <= longest || (apiPath != base && !strings.HasPrefix(apiPath, base+"/")) {
			continue
		}
		parts := strings.Split(command.Name, ".")
		if len(parts) < 2 {
			continue
		}
		action := parts[len(parts)-1]
		if action != "create" && action != "list" && action != "get" {
			continue
		}
		routePrefix := strings.ReplaceAll(strings.TrimPrefix(base, "/v1/"), "/", ".")
		if name != routePrefix && !strings.HasPrefix(name, routePrefix+".") {
			continue
		}
		aligned = strings.Join(parts[:len(parts)-1], ".") + strings.TrimPrefix(name, routePrefix)
		longest = len(base)
	}
	return aligned
}

func publicAPICommandName(apiPath, method string, operation openAPIOperation) string {
	if operation.OperationID == "authorizePartnerInstall" {
		return "oauth.authorize"
	}
	if fulfillmentAPIPath(apiPath) {
		return fulfillmentCommandName(apiPath, method, operation.FlintCLIAction)
	}
	segments := strings.Split(strings.TrimPrefix(apiPath, "/v1/"), "/")
	var literals []string
	for _, segment := range segments {
		if !strings.HasPrefix(segment, "{") {
			literals = append(literals, strings.TrimSuffix(segment, ".json"))
		}
	}
	last := segments[len(segments)-1]
	if operation.FlintCLIAction != "" {
		if !strings.HasPrefix(last, "{") {
			literals = literals[:len(literals)-1]
		}
		return strings.Join(append(literals, operation.FlintCLIAction), ".")
	}
	action := map[string]string{"GET": "list", "POST": "create", "PATCH": "update", "PUT": "replace", "DELETE": "delete"}[method]
	if method == "GET" && !strings.HasPrefix(operation.OperationID, "list") {
		action = "get"
	}
	if method == "POST" && !strings.HasPrefix(last, "{") && len(segments) > 1 && !strings.HasPrefix(operation.OperationID, "create") {
		return strings.Join(literals, ".")
	}
	return strings.Join(append(literals, action), ".")
}

func fulfillmentAPIPath(apiPath string) bool {
	return strings.Contains(apiPath, "/delivery-") ||
		strings.Contains(apiPath, "/fulfillment") ||
		strings.HasPrefix(apiPath, "/v1/shipments") ||
		strings.HasPrefix(apiPath, "/v1/packages")
}

func fulfillmentCommandName(apiPath, method, explicitAction string) string {
	segments := strings.Split(strings.TrimPrefix(apiPath, "/v1/"), "/")
	literals := make([]string, 0, len(segments))
	for _, segment := range segments {
		if !strings.HasPrefix(segment, "{") {
			literals = append(literals, segment)
		}
	}
	lastSegment := segments[len(segments)-1]
	if explicitAction != "" {
		if len(literals) > 0 && literals[len(literals)-1] == lastSegment {
			literals = literals[:len(literals)-1]
		}
		return strings.Join(append(literals, explicitAction), ".")
	}
	if lastSegment == "current" {
		action := map[string]string{"GET": "get", "DELETE": "delete"}[method]
		return strings.Join(append(literals, action), ".")
	}
	if _, action := fulfillmentPathActions[lastSegment]; action {
		return strings.Join(literals, ".")
	}
	action := map[string]string{
		"GET": "list", "POST": "create", "PUT": "replace",
		"PATCH": "update", "DELETE": "delete",
	}[method]
	if strings.HasPrefix(lastSegment, "{") && method == "GET" {
		action = "get"
	}
	return strings.Join(append(literals, action), ".")
}

var fulfillmentPathActions = map[string]struct{}{
	"accept": {}, "activate": {}, "apply": {}, "archive": {}, "cancel": {},
	"complete": {}, "deactivate": {}, "dispatch": {}, "fail": {},
	"hold": {}, "mark-no-show": {}, "mark-packed": {}, "mark-picked": {},
	"mark-preparing": {}, "mark-ready": {}, "preview": {},
	"query-pickup-availability": {}, "refresh": {}, "resend": {},
	"revoke": {}, "rotate-secret": {}, "schedule": {}, "start": {},
	"check-connection": {}, "test-deliveries": {},
	"void": {},
}

func publicAPIArguments(
	apiPath string,
	operation openAPIOperation,
) []Arg {
	arguments := make([]Arg, 0, len(operation.Parameters))
	pathPosition := 0
	for _, parameter := range operation.Parameters {
		name, _ := parameter["name"].(string)
		location, _ := parameter["in"].(string)
		if name == "" || (location != "path" && location != "query") ||
			name == "expand" {
			continue
		}
		description, _ := parameter["description"].(string)
		schema, _ := parameter["schema"].(map[string]any)
		typeName := publicSchemaType(schema)
		argument := Arg{
			Name:        name,
			Type:        openAPIArgumentType(typeName, schema),
			Description: description,
			IDPrefix:    publicResourceIDPrefix(name),
		}
		if argument.IDPrefix != "" || isPublicResourceID(name) {
			argument.AcceptsHistoryRef = true
		}
		if location == "path" {
			pathPosition++
			argument.Required = true
			argument.Positional = pathPosition
		} else {
			argument.Flag = "--" + strings.ReplaceAll(name, "_", "-")
			if globalFlags[strings.TrimPrefix(argument.Flag, "--")] && name != "page_size" && name != "page_token" {
				argument.Name = "filter_" + name
				argument.Flag = "--filter-" + strings.ReplaceAll(name, "_", "-")
			}
			argument.Query = name
			argument.Required, _ = parameter["required"].(bool)
			argument.Repeat = typeName == "array"
		}
		arguments = append(arguments, argument)
	}
	sort.SliceStable(arguments, func(i, j int) bool {
		leftPath := arguments[i].Positional > 0
		rightPath := arguments[j].Positional > 0
		if leftPath != rightPath {
			return leftPath
		}
		if leftPath {
			return strings.Index(apiPath, "{"+arguments[i].Name+"}") <
				strings.Index(apiPath, "{"+arguments[j].Name+"}")
		}
		return arguments[i].Name < arguments[j].Name
	})
	for index := range arguments {
		if arguments[index].Positional > 0 {
			arguments[index].Positional = index + 1
		}
	}
	return arguments
}

func openAPIArgumentType(typeName string, schema map[string]any) string {
	if typeName == "array" {
		items, _ := schema["items"].(map[string]any)
		if itemType, _ := items["type"].(string); itemType != "" {
			return openAPIArgumentType(itemType, items)
		}
		return "string"
	}
	switch typeName {
	case "integer", "number", "boolean":
		return typeName
	default:
		if format, _ := schema["format"].(string); format == "date-time" {
			return "time"
		}
		return "string"
	}
}

var publicResourceIDPrefixes = map[string]string{
	"credit_note_id": "cn_", "credit_note_allocation_id": "cna_", "invoice_payment_attempt_id": "invpa_", "email_change_request_id": "cecr_", "environment_grant_id": "egrt_", "partner_app_install_id": "pinst_", "plan_id": "plan_", "review_id": "rev_",
	"after_event_id":                     "whev_",
	"api_key_id":                         "key_",
	"api_request_log_id":                 "rlog_",
	"balance_transaction_id":             "btxn_",
	"bundle_id":                          "bun_",
	"category_id":                        "ctg_",
	"checkout_session_id":                "cs_",
	"corrects_return_resolution_id":      "retres_",
	"customer_address_id":                "caddr_",
	"customer_deletion_request_id":       "cdel_",
	"customer_id":                        "cus_",
	"customer_session_id":                "cses_",
	"delivery_location_set_id":           "dls_",
	"delivery_location_set_revision_id":  "dlsr_",
	"delivery_method_id":                 "dmet_",
	"delivery_method_revision_id":        "dmetr_",
	"delivery_profile_id":                "dprof_",
	"delivery_profile_revision_id":       "dprofr_",
	"delivery_quote_id":                  "dqt_",
	"delivery_rate_callback_id":          "dcb_",
	"delivery_rate_callback_revision_id": "dcbr_",
	"delivery_rate_id":                   "drate_",
	"delivery_revocation_id":             "drev_",
	"delivery_selection_id":              "dsel_",
	"delivery_zone_id":                   "dzone_",
	"delivery_zone_revision_id":          "dzoner_",
	"destination_location_id":            "loc_",
	"device_id":                          "dev_",
	"dispute_id":                         "du_",
	"environment_id":                     "menv_",
	"expected_delivery_selection_id":     "dsel_",
	"fraud_warning_id":                   "fw_",
	"fulfillment_event_id":               "fev_",
	"fulfillment_id":                     "ful_",
	"fulfillment_notification_id":        "fnt_",
	"inventory_allocation_policy_id":     "invp_",
	"inventory_count_id":                 "invc_",
	"inventory_item_id":                  "invi_",
	"inventory_level_id":                 "invl_",
	"inventory_location_id":              "loc_",
	"inventory_reservation_id":           "invr_",
	"inventory_transfer_id":              "invtr_",
	"invoice_id":                         "inv_",
	"invoice_payment_term_id":            "ipt_",
	"location_id":                        "loc_",
	"merchant_billing_balance_id":        "mbbal_",
	"merchant_id":                        "mer_",
	"merchant_subscription_invoice_id":   "msinv_",
	"modifier_group_id":                  "mg_",
	"modifier_set_id":                    "ms_",
	"option_id":                          "opt_",
	"order_charge_id":                    "och_",
	"order_id":                           "ord_",
	"order_line_item_id":                 "li_",
	"organization_id":                    "org_",
	"origin_location_id":                 "loc_",
	"package_id":                         "pkg_",
	"package_item_id":                    "pki_",
	"parent_organization_id":             "org_",
	"partner_app_id":                     "papp_",
	"payment_attempt_id":                 "opat_",
	"payment_intent_id":                  "pi_",
	"payment_link_id":                    "pl_",
	"payment_method_domain_id":           "pmdom_",
	"payment_method_id":                  "pm_",
	"payout_destination_id":              "pdest_",
	"payout_id":                          "po_",
	"product_id":                         "prod_",
	"promotion_code_id":                  "pcode_",
	"promotion_id":                       "promo_",
	"receiving_location_id":              "loc_",
	"refund_id":                          "ref_",
	"replaces_return_disposition_id":     "retdsp_",
	"report_download_id":                 "rdl_",
	"report_id":                          "rep_",
	"resolution_id":                      "retres_",
	"return_disposition_id":              "retdsp_",
	"return_id":                          "ret_",
	"return_inspection_id":               "retins_",
	"return_inspection_line_item_id":     "retinli_",
	"return_line_item_id":                "retli_",
	"return_policy_id":                   "rpol_",
	"return_policy_revision_id":          "rpolv_",
	"return_reason_id":                   "rrsn_",
	"return_receipt_id":                  "retrc_",
	"return_receipt_line_item_id":        "retrcli_",
	"return_resolution_id":               "retres_",
	"risk_list_id":                       "rl_",
	"risk_list_item_id":                  "rli_",
	"risk_rule_id":                       "rule_",
	"sandbox_id":                         "test_",
	"shipment_id":                        "shp_",
	"subscription_id":                    "sub_",
	"subscription_payment_retry_id":      "spr_",
	"user_id":                            "usr_",
	"variant_id":                         "var_",
	"webhook_delivery_id":                "wdel_",
	"webhook_endpoint_id":                "whep_",
	"webhook_event_id":                   "whev_",
}

func publicResourceIDPrefix(name string) string { return publicResourceIDPrefixes[name] }

func requestSchemaName(requestBody map[string]any) string {
	content, _ := requestBody["content"].(map[string]any)
	jsonContent, _ := content["application/json"].(map[string]any)
	schema, _ := jsonContent["schema"].(map[string]any)
	if name := schemaRefName(schema); name != "" {
		return name
	}
	if all, ok := schema["allOf"].([]any); ok && len(all) == 1 {
		return schemaRefName(all[0])
	}
	return ""
}

func responseSchemaName(responses map[string]map[string]any) string {
	statuses := make([]string, 0, len(responses))
	for status := range responses {
		if strings.HasPrefix(status, "2") {
			statuses = append(statuses, status)
		}
	}
	sort.Strings(statuses)
	for _, status := range statuses {
		content, _ := responses[status]["content"].(map[string]any)
		jsonContent, _ := content["application/json"].(map[string]any)
		if name := schemaRefName(jsonContent["schema"]); name != "" {
			return name
		}
	}
	return ""
}

func schemaRefName(value any) string {
	schema, _ := value.(map[string]any)
	ref, _ := schema["$ref"].(string)
	return strings.TrimPrefix(ref, "#/components/schemas/")
}

func publicAPIExample(
	name string,
	arguments []Arg,
	hasInput bool,
	destructive bool,
) string {
	parts := []string{"flint"}
	parts = append(parts, strings.Split(name, ".")...)
	for _, argument := range arguments {
		if argument.Positional > 0 {
			prefix := argument.IDPrefix
			if prefix == "" {
				prefix = argument.Name + "_"
			}
			parts = append(parts, prefix+"123")
			continue
		}
		if argument.Required && argument.Name != "input" {
			value := "value"
			switch {
			case argument.IDPrefix != "":
				value = argument.IDPrefix + "123"
			case argument.Type == "integer", argument.Type == "number":
				value = "1"
			case argument.Type == "boolean":
				value = "true"
			case argument.Type == "time":
				value = "2h"
			}
			parts = append(parts, argument.Flag, value)
		}
	}
	if hasInput {
		parts = append(parts, "--input", "request.json")
	}
	if destructive {
		parts = append(parts, "--confirm")
	}
	return strings.Join(parts, " ")
}

func humanizeOperationID(operationID string) string {
	words := splitCamelCase(operationID)
	if len(words) == 0 {
		return "Call fulfillment API"
	}
	words[0] = strings.ToUpper(words[0][:1]) + words[0][1:]
	return strings.Join(words, " ")
}

func splitCamelCase(value string) []string {
	var words []string
	var current []rune
	for index, character := range []rune(value) {
		if index > 0 && unicode.IsUpper(character) {
			words = append(words, strings.ToLower(string(current)))
			current = current[:0]
		}
		current = append(current, character)
	}
	if len(current) > 0 {
		words = append(words, strings.ToLower(string(current)))
	}
	return words
}

func publicSchemaType(schema map[string]any) string {
	if value, ok := schema["type"].(string); ok {
		return value
	}
	if types, ok := schema["type"].([]any); ok {
		for _, value := range types {
			if name, ok := value.(string); ok && name != "null" {
				return name
			}
		}
	}
	return "string"
}

func isPublicResourceID(name string) bool {
	if !strings.HasSuffix(name, "_id") {
		return false
	}
	switch name {
	case "request_id", "related_request_id", "client_id", "correlation_id", "source_reference_id":
		return false
	}
	return !strings.HasPrefix(name, "external_")
}

func applyPublicOperationMetadata(command *Command) {
	operation, ok := openAPIOperationByID(command.OperationID)
	if !ok {
		return
	}
	command.Security = operation.Security
	command.AuthRequired = len(operation.Security) > 0
	if strings.HasPrefix(command.APIPath, "/v1/developer/partner/") || strings.HasPrefix(command.APIPath, "/v1/developer/sandboxes") || strings.HasPrefix(command.APIPath, "/v1/onboarding/") || strings.HasPrefix(command.APIPath, "/v1/oauth/") {
		command.EnvironmentAffinity = EnvironmentAffinityAccount
	}
	if !command.AuthRequired {
		command.EnvironmentAffinity = EnvironmentAffinityNeutral
	}
	queries := map[string]bool{}
	for _, argument := range command.Arguments {
		queries[argument.Query] = true
	}
	for _, argument := range publicAPIArguments(command.APIPath, operation) {
		if argument.Query != "" && !queries[argument.Query] {
			command.Arguments = append(command.Arguments, argument)
		}
		if argument.Query == "page_size" {
			command.Supports.Pagination = true
		}
	}
	for status, response := range operation.Responses {
		if !strings.HasPrefix(status, "2") {
			continue
		}
		content, _ := response["content"].(map[string]any)
		if _, ok := content["application/pdf"]; ok {
			command.ResponseMediaType = "application/pdf"
		}
	}
	if command.OperationID == "getReportDownload" {
		command.ResponseMediaType = "text/csv"
	}
	if command.OperationID == "getOpenAPISpec" {
		// A contract contains example IDs, not retrieved merchant resources.
		command.Get = false
		command.Supports.WaitFor = false
	}
	if command.ResponseMediaType != "" {
		command.Arguments = append(command.Arguments, flag("save_to", "string", "", "", "Save the downloaded file to a new path", false))
		command.Get = false
		command.Supports.WaitFor = false
	}
}
