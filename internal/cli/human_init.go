package cli

import (
	"fmt"
	"io"
	"time"
)

func renderInitHuman(w io.Writer, value any, now time.Time) bool {
	normalized, err := normalizeOutput(value)
	if err != nil {
		return false
	}
	envelope, _ := normalized.(map[string]any)
	data, _ := envelope["data"].(map[string]any)
	items, ok := data["checks"].([]any)
	if !ok {
		return false
	}
	checks := make([]map[string]any, 0, len(items))
	for _, item := range items {
		check, ok := item.(map[string]any)
		if !ok {
			return false
		}
		checks = append(checks, check)
	}
	renderDoctorHuman(w, map[string]any{"data": checks})
	remaining := make(map[string]any, len(data))
	for key, value := range data {
		if key != "checks" && key != "next_steps" {
			remaining[key] = value
		}
	}
	if len(remaining) > 0 {
		fmt.Fprintln(w)
		renderHumanValue(w, remaining, "", now, 0)
	}
	if steps, ok := data["next_steps"].([]any); ok && len(steps) > 0 {
		fmt.Fprintln(w, "\nNext steps")
		rows := make([][]string, 0, len(steps))
		for _, item := range steps {
			step, _ := item.(map[string]any)
			rows = append(rows, []string{fmt.Sprint(step["workflow"]), fmt.Sprint(step["command"])})
		}
		renderTextTable(w, []string{"Workflow", "Command"}, rows)
	}
	return true
}
