package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLocalCommandsRenderStructuredHumanOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/openapi.json" {
			fmt.Fprint(w, `{"x-flint-api-releases":{"current_version":"2026-02-01"}}`)
			return
		}
		fmt.Fprint(w, authContextJSON("sandbox"))
	}))
	defer server.Close()
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"config", "get"}, []string{"Profile", "default", "Global config path", "Sources"}},
		{[]string{"auth", "import", "--stdin"}, []string{"Auth type", "api_key", "Merchant ID", "mer_123", "Scopes", "payments.payment_intents.read"}},
		{[]string{"init"}, []string{"Checks", "API accepted the credential", "Next steps", "Workflow", "commerce_order", "flint orders create --input order.json"}},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			app, out, stderr := testApp(t, server.URL)
			if tc.args[0] == "auth" {
				t.Setenv("FLINT_API_KEY", "")
				app.Stdin = strings.NewReader("flint_test_fixture\n")
			}
			if exit := app.Run(tc.args); exit != ExitOK || stderr.Len() != 0 {
				t.Fatalf("exit=%d stdout=%s stderr=%s", exit, out, stderr)
			}
			for _, want := range tc.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("missing %q in %s", want, out)
				}
			}
			for _, unwanted := range []string{"map[", "<nil>", "{", "}"} {
				if strings.Contains(out.String(), unwanted) {
					t.Errorf("Go formatting %q in %s", unwanted, out)
				}
			}
		})
	}
}

func TestGenericHumanOutputHonorsJSONFieldsAndExactNumbers(t *testing.T) {
	app, out, _ := testApp(t, "")
	value := struct {
		Internal string `json:"-"`
		Amount   int64  `json:"amount"`
		Label    string `json:"étiquette"`
	}{Internal: "internal-only", Amount: 9007199254740993, Label: "café 日本語"}
	if err := app.writeResult(map[string]any{"data": value}, nil, defaultOptions()); err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(out.String()) {
		t.Fatalf("invalid UTF-8: %q", out.String())
	}
	for _, want := range []string{"Amount", "9007199254740993", "Étiquette", "café 日本語"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
	if strings.Contains(out.String(), "internal-only") {
		t.Fatalf("internal field exposed: %s", out)
	}
}

func TestHumanLabelUnicode(t *testing.T) {
	for input, want := range map[string]string{"étiquette": "Étiquette", "日本語": "日本語", "😀_name": "😀 name", "customer_id": "Customer ID", "": "(empty key)"} {
		if got := humanLabel(input); got != want || !utf8.ValidString(got) {
			t.Errorf("humanLabel(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestHumanOutputEncodingFailure(t *testing.T) {
	app, out, _ := testApp(t, "")
	err := app.writeResult(map[string]any{"data": make(chan int)}, nil, defaultOptions())
	if err == nil || err.Code != "OUTPUT_ENCODING_FAILED" || out.Len() != 0 {
		t.Fatalf("error=%v output=%s", err, out)
	}
}
