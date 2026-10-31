package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

func renderHelpTopicHuman(w io.Writer, topic string, value map[string]any) {
	fmt.Fprintln(w, strings.ToUpper(strings.ReplaceAll(topic, "-", " ")))
	normalized, err := normalizeOutput(value)
	if err != nil {
		return
	}
	data, _ := normalized.(map[string]any)
	table := func(title, key string, columns ...string) {
		items, _ := data[key].([]any)
		if len(items) == 0 {
			return
		}
		fmt.Fprintln(w, "\n"+title)
		headers := make([]string, len(columns))
		for i, column := range columns {
			headers[i] = humanLabel(column)
		}
		rows := make([][]string, 0, len(items))
		for _, item := range items {
			fields, _ := item.(map[string]any)
			row := make([]string, len(columns))
			for i, column := range columns {
				if v := fields[column]; v != nil {
					row[i] = fmt.Sprint(v)
				}
			}
			rows = append(rows, row)
		}
		renderTextTable(w, headers, rows)
	}
	switch topic {
	case "test-cards":
		table("Sandbox API tokens", "sandbox_tokens", "scenario", "token", "behavior")
		table("Card numbers (hosted checkout forms only)", "provider_test_cards", "scenario", "number")
	case "exit-codes":
		table("Process exit codes", "exit_codes", "code", "meaning")
		if types, ok := data["types"].([]any); ok {
			fmt.Fprintln(w, "\nError types")
			rows := make([][]string, 0, len(types))
			for _, typ := range types {
				rows = append(rows, []string{fmt.Sprint(typ)})
			}
			renderTextTable(w, []string{"Type"}, rows)
		}
		table("Listener error codes", "listen_codes", "code", "exit_code")
	case "events":
		table("Webhook event types", "events", "type", "description")
	case "errors":
		fmt.Fprintln(w, "\nEnvelope: "+fmt.Sprint(data["envelope"]))
		fields, _ := data["fields"].(map[string]any)
		keys := make([]string, 0, len(fields))
		for key := range fields {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		rows := make([][]string, 0, len(keys))
		for _, key := range keys {
			rows = append(rows, []string{key, fmt.Sprint(fields[key])})
		}
		renderTextTable(w, []string{"Field", "Description"}, rows)
	}
}
