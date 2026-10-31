package cli

import (
	"sort"
	"strings"
	"unicode"
)

// publicAPICommands derives every API command name and route from the contract.
// CLI-owned operations are excluded; overlays carry behavior.
func publicAPICommands() []*Command {
	doc, err := loadOpenAPI()
	if err != nil {
		panic("load public API commands: " + err.Error())
	}
	var commands []*Command
	for path, methods := range doc.Paths {
		for method, operation := range methods {
			upper := strings.ToUpper(method)
			if !strings.Contains(" GET POST PUT PATCH DELETE ", " "+upper+" ") || commandExclusion(&doc, path, operation) != "" {
				continue
			}
			name, kind, err := commandName(&doc, path, upper, operation)
			if err != nil {
				panic(err)
			}
			arguments := publicAPIArguments(path, operation)
			overlay := publicOperationOverlays[operation.OperationID]
			arguments = append(arguments, publicBodyArguments(operation, arguments)...)
			arguments = append(arguments, overlay.Arguments...)
			if operation.RequestBody != nil {
				arguments = append(arguments, flag("input", "string", "", "", "JSON body: FILE.json, - for stdin, or inline JSON", false))
			}
			destructive := operation.FlintDestructive || upper == "DELETE" || overlay.Destructive
			command := apiCommand(name, strings.TrimSpace(operation.Summary), upper, path, operation.OperationID, requestSchemaName(operation.RequestBody), responseSchemaName(operation.Responses), arguments, upper != "GET", false, destructive, upper == "GET" && kind != "collection")
			command.Kind = kind
			command.Render, command.Supports.Open = overlay.Render, overlay.Open
			command.Examples = append([]string(nil), overlay.Examples...)
			if len(command.Examples) == 0 {
				command.Examples = ex(publicAPIExample(name, arguments, operation.RequestBody != nil, destructive))
			}
			// Looking up an email preference link is the existing read-only POST.
			if overlay.ReadOnly {
				command.Mutation = false
			}
			commands = append(commands, command)
		}
	}
	if err := validateCommandNames(commands); err != nil {
		panic(err)
	}
	sort.Slice(commands, func(i, j int) bool { return commands[i].Name < commands[j].Name })
	return commands
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
			IDPrefix:    publicResourceIDPrefixes[name],
		}
		if argument.IDPrefix != "" || isPublicResourceID(name) {
			argument.AcceptsHistoryRef = true
		}
		if location == "path" {
			applyQueryArgumentContract(&argument, schema)
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
			applyQueryArgumentContract(&argument, schema)
			if argument.Repeat {
				argument.Style, _ = parameter["style"].(string)
				if argument.Style == "" {
					argument.Style = "form"
				}
				explode, ok := parameter["explode"].(bool)
				if !ok {
					explode = argument.Style == "form"
				}
				argument.Explode = &explode
			}
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

func applyQueryArgumentContract(argument *Arg, schema map[string]any) {
	argument.Repeat = publicSchemaType(schema) == "array"
	argument.Type = openAPIArgumentType(publicSchemaType(schema), schema)
	argument.Enum = nil
	if argument.Repeat {
		schema, _ = schema["items"].(map[string]any)
	}
	if values, ok := schema["enum"].([]any); ok {
		for _, value := range values {
			if text, ok := value.(string); ok {
				argument.Enum = append(argument.Enum, text)
			}
		}
	}
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
		} else if format == "date" {
			return "date"
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
	"custom_domain_id":                   "cdom_",
	"customer_id":                        "cus_",
	"customer_session_id":                "cses_",
	"customer_verification_id":           "cver_",
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
	"fraud_warning_id":                   "frw_",
	"gift_card_id":                       "gc_",
	"gift_card_load_id":                  "gcl_",
	"gift_card_notification_id":          "gcn_",
	"gift_card_redemption_id":            "gcr_",
	"gift_card_transaction_id":           "gct_",
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
	"invoice_schedule_entry_id":          "invse_",
	"invoice_late_fee_id":                "inlf_",
	"invoice_payment_term_id":            "ipt_",
	"location_id":                        "loc_",
	"merchant_billing_balance_id":        "mbbal_",
	"merchant_id":                        "mer_",
	"merchant_billing_invoice_id":        "msinv_",
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

	"checkout_session_customer_verification_id": "cscv_",
	"file_id":                                 "file_",
	"gift_card_funding_disposition_id":        "gfd_",
	"inventory_adjustment_id":                 "invadj_",
	"inventory_allocation_policy_revision_id": "invpv_",
	"inventory_movement_id":                   "invm_",
	"inventory_receipt_id":                    "invrec_",
}

func requestSchemaName(requestBody map[string]any) string {
	_, schema := requestContentSchema(requestBody)
	if name := schemaRefName(schema); name != "" {
		return name
	}
	if all, ok := schema["allOf"].([]any); ok && len(all) == 1 {
		return schemaRefName(all[0])
	}
	return ""
}

func requestContentSchema(requestBody map[string]any) (string, map[string]any) {
	content, _ := requestBody["content"].(map[string]any)
	for _, mediaType := range []string{"application/json", "application/x-www-form-urlencoded"} {
		if media, ok := content[mediaType].(map[string]any); ok {
			schema, _ := media["schema"].(map[string]any)
			return mediaType, schema
		}
	}
	return "", nil
}

func responseSchema(responses map[string]map[string]any) map[string]any {
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
		if schema, ok := jsonContent["schema"].(map[string]any); ok {
			return schema
		}
	}
	return nil
}

func responseSchemaName(responses map[string]map[string]any) string {
	return schemaRefName(responseSchema(responses))
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
			case len(argument.Enum) > 0:
				value = argument.Enum[0]
			case argument.IDPrefix != "":
				value = argument.IDPrefix + "123"
			case argument.Type == "integer", argument.Type == "number":
				value = "1"
			case argument.Type == "boolean":
				value = "true"
			case argument.Type == "time":
				value = "2h"
				if argument.BodyPath != "" {
					value = "+2h"
				}
			case argument.Type == "date":
				value = "2026-10-31"
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
	for _, name := range mcpSchemaStrings(schema["type"]) {
		if name != "null" {
			return name
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
	command.OutputSchema = responseSchemaName(operation.Responses)
	command.AuthRequired = len(operation.Security) > 0
	if command.APIPath == "/v1/partner-apps" || strings.HasPrefix(command.APIPath, "/v1/partner-apps/") || command.APIPath == "/v1/sandboxes" || strings.HasPrefix(command.APIPath, "/v1/sandboxes/") || strings.HasPrefix(command.APIPath, "/v1/onboarding/") || strings.HasPrefix(command.APIPath, "/v1/oauth/") {
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
		command.Arguments = append(command.Arguments, flag("save_to", "string", "", "", "Save the downloaded file to a new path, or - for stdout", false))
		command.Get = false
		command.Supports.WaitFor = false
	}
}
