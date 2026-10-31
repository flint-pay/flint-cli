package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/currency"
)

// Recognize the contract's Money shape without interpreting other amount fields.
func humanMoney(value any) (string, bool) {
	m, ok := value.(map[string]any)
	if !ok || len(m) != 2 {
		return "", false
	}
	code, ok := m["currency"].(string)
	if !ok || code == "" {
		return "", false
	}
	var amount int64
	switch n := m["amount"].(type) {
	case json.Number:
		var err error
		amount, err = n.Int64()
		if err != nil {
			return "", false
		}
	case int:
		amount = int64(n)
	case int64:
		amount = n
	default:
		return "", false
	}
	digits := strconv.FormatInt(amount, 10)
	unit, err := currency.ParseISO(code)
	if err != nil || unit == currency.XXX {
		return digits + " " + terminalSafe(code) + " (minor units)", true
	}
	scale, _ := currency.Standard.Rounding(unit)
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	if scale > 0 {
		if len(digits) <= scale {
			digits = strings.Repeat("0", scale+1-len(digits)) + digits
		}
		digits = digits[:len(digits)-scale] + "." + digits[len(digits)-scale:]
	}
	return sign + digits + " " + code, true
}

// Use public field names to select a compact summary. Detail commands retain
// every field, and JSON keeps the complete list resources.
func renderResourceTable(w io.Writer, rows []any, cmd *Command, now time.Time) bool {
	if len(rows) == 0 {
		return false
	}
	available := map[string]bool{}
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			return false
		}
		for key := range m {
			available[key] = true
		}
	}
	keys := make([]string, 0, len(available))
	for key := range available {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	columns := []string{}
	addFirst := func(candidates ...string) {
		for _, key := range candidates {
			if available[key] {
				columns = append(columns, key)
				return
			}
		}
	}
	// Resource identifiers generally precede foreign IDs in the alphabet. Prefer
	// the command's own resource name when several identifiers are present.
	if cmd != nil {
		group := strings.ReplaceAll(strings.Split(cmd.CanonicalName, ".")[0], "-", "_")
		addFirst(strings.TrimSuffix(group, "s") + "_id")
	}
	if len(columns) == 0 {
		for _, key := range keys {
			if strings.HasSuffix(key, "_id") || key == "id" {
				addFirst(key)
				break
			}
		}
	}
	statuses, money := []string{"status"}, []string{"total_money", "amount_money"}
	for _, key := range keys {
		if strings.HasSuffix(key, "_status") {
			statuses = append(statuses, key)
		}
		if strings.HasSuffix(key, "_money") {
			money = append(money, key)
		}
	}
	addFirst(statuses...)
	addFirst(money...)
	addFirst("name", "email")
	addFirst("created_at")
	if len(columns) == 0 {
		return false
	}
	cells := make([][]string, 0, len(rows))
	for _, row := range rows {
		m := row.(map[string]any)
		line := make([]string, len(columns))
		for i, key := range columns {
			v := m[key]
			if text, ok := humanMoney(v); ok {
				line[i] = text
			} else if key == "created_at" {
				if stamp, ok := v.(string); ok {
					if t, err := time.Parse(time.RFC3339, stamp); err == nil {
						line[i] = relativeAge(now.Sub(t))
					}
				}
			} else if v != nil {
				switch v.(type) {
				case map[string]any, []any:
					return false
				default:
					line[i] = formatHumanScalar(v, now)
				}
			}
			if line[i] == "" {
				line[i] = "-"
			}
		}
		cells = append(cells, line)
	}
	headers := make([]string, len(columns))
	for i, key := range columns {
		headers[i] = humanLabel(key)
	}
	renderTextTable(w, headers, cells)
	return true
}

func renderTextTable(w io.Writer, headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for i, header := range headers {
		widths[i] = utf8.RuneCountInString(terminalSafe(header))
	}
	for _, row := range rows {
		for i, cell := range row {
			widths[i] = max(widths[i], utf8.RuneCountInString(terminalSafe(cell)))
		}
	}
	printRow := func(row []string) {
		for i, cell := range row {
			if i > 0 {
				fmt.Fprint(w, "  ")
			}
			text := terminalSafe(cell)
			fmt.Fprint(w, text)
			if i < len(row)-1 {
				fmt.Fprint(w, strings.Repeat(" ", widths[i]-utf8.RuneCountInString(text)))
			}
		}
		fmt.Fprintln(w)
	}
	printRow(headers)
	for _, row := range rows {
		printRow(row)
	}
}
