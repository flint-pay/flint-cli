package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestHumanHelpTopicTables(t *testing.T) {
	for _, topic := range helpTopicNames {
		t.Run(topic, func(t *testing.T) {
			value, ok := helpTopic(topic)
			if !ok {
				t.Fatal("missing topic")
			}
			before, _ := json.Marshal(value)
			var out bytes.Buffer
			renderHelpTopicHuman(&out, topic, value)
			want := map[string]string{"test-cards": "Scenario", "exit-codes": "Meaning", "errors": "Description", "events": "Description"}[topic]
			if !strings.Contains(out.String(), want) || strings.Contains(out.String(), `"scenario":`) || strings.Contains(out.String(), `"meaning":`) {
				t.Fatalf("missing table: %s", &out)
			}
			after, _ := json.Marshal(value)
			if !bytes.Equal(before, after) {
				t.Fatal("human rendering changed topic data")
			}
		})
	}
}
