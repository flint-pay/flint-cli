package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigSafetyUnknownFields(t *testing.T) {
	for _, tc := range []struct {
		project   bool
		raw, want string
	}{
		{true, `{"merchant":"mer_old"}`, `renamed to "merchant_id"`},
		{true, `{"sandbox":"test_old"}`, `renamed to "sandbox_id"`},
		{true, `{"context":"ctx_old"}`, `renamed to "context_id"`},
		{true, `{"merchat_id":"mer_old"}`, `did you mean "merchant_id"`},
		{true, `{"future":true}`, `unknown field "future"`},
		{false, `{"future":true}`, `unknown field "future"`},
		{false, `{"profiles":{"default":{"merchant_gurad":"mer_old"}}}`, `unknown field "merchant_gurad"`},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			app, out, _ := testApp(t, "http://127.0.0.1:9")
			path, _ := app.configPath()
			if tc.project {
				path = filepath.Join(app.WorkingDir, ".flint", "config.json")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(path, []byte(tc.raw), 0600); err != nil {
				t.Fatal(err)
			}
			for _, argv := range [][]string{{"config", "get"}, {"config", "validate"}, {"customers", "list"}, {"init"}} {
				out.Reset()
				exit, result := runJSON(t, app, out, argv...)
				if exit != ExitAuth || !strings.Contains(fmt.Sprint(result), path) || !strings.Contains(fmt.Sprint(result), tc.want) {
					t.Fatalf("argv=%v exit=%d result=%v", argv, exit, result)
				}
			}
		})
	}
}

func TestConfigSafetyUnknownProfileWithEnvironment(t *testing.T) {
	for _, env := range []string{"FLINT_API_KEY", "FLINT_ACCESS_TOKEN", "FLINT_WORKLOAD_IDENTITY_ID"} {
		for _, selector := range []string{"flag", "environment", "project"} {
			t.Run(env+"/"+selector, func(t *testing.T) {
				app, out, _ := testApp(t, "http://127.0.0.1:9")
				t.Setenv("FLINT_API_KEY", "")
				t.Setenv("FLINT_ACCESS_TOKEN", "")
				t.Setenv("FLINT_WORKLOAD_IDENTITY_ID", "")
				t.Setenv(env, "synthetic")
				argv := []string{"config", "get"}
				switch selector {
				case "flag":
					argv = append(argv, "--profile", "typo")
				case "environment":
					t.Setenv("FLINT_PROFILE", "typo")
				case "project":
					dir := filepath.Join(app.WorkingDir, ".flint")
					if err := os.MkdirAll(dir, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"profile":"typo"}`), 0600); err != nil {
						t.Fatal(err)
					}
				}
				exit, result := runJSON(t, app, out, argv...)
				if exit != ExitAuth || !strings.Contains(fmt.Sprint(result), `unknown profile "typo"`) {
					t.Fatalf("exit=%d result=%v", exit, result)
				}
			})
		}
	}
}

func TestConfigSafetyProfileSelectionAndListing(t *testing.T) {
	app, out, _ := testApp(t, "http://127.0.0.1:9")
	if err := app.updateConfig(func(cfg *Config) error { cfg.Profiles["qa"] = profileWithoutCredential(); return nil }); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"qa", "typo"} {
		out.Reset()
		exit, _ := runJSON(t, app, out, "config", "set", "profile", name)
		cfg, _ := app.loadConfig()
		if name == "qa" && (exit != ExitOK || cfg.DefaultProfile != "qa") {
			t.Fatal(out)
		}
		if name == "typo" && (exit != ExitAuth || cfg.DefaultProfile != "qa") {
			t.Fatal(out)
		}
		if _, ok := cfg.Profiles["typo"]; ok {
			t.Fatal("unknown profile created")
		}
	}
	out.Reset()
	exit, result := runJSON(t, app, out, "config", "get")
	profiles, ok := lookupPath(result, "data.profiles")
	if exit != ExitOK || !ok || len(profiles.([]any)) != 2 || strings.Contains(out.String(), "flint_test_") || strings.Contains(out.String(), `"credential_store"`) {
		t.Fatal(result)
	}
	out.Reset()
	if exit := app.Run([]string{"config", "unset", "profile"}); exit != ExitOK {
		t.Fatal(out)
	}
	cfg, _ := app.loadConfig()
	if cfg.DefaultProfile != "default" || len(cfg.Profiles) != 2 {
		t.Fatal(cfg)
	}
}

func TestConfigSafetyLogoutDeletesProfileAndRollsBack(t *testing.T) {
	for _, store := range []string{"file", "keyring"} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprint(store, fail), func(t *testing.T) {
				app, out, _ := testApp(t, "http://127.0.0.1:9")
				t.Setenv("FLINT_API_KEY", "")
				app.credentialStore = store
				if e := app.saveAuthenticatedCredential("qa", "flint_test_synthetic", AuthContext{Environment: "sandbox", MerchantID: "mer_123"}); e != nil {
					t.Fatal(e)
				}
				if err := app.updateConfig(func(cfg *Config) error {
					cfg.DefaultProfile = "qa"
					p := cfg.Profiles["qa"]
					p.MerchantGuard = "mer_guard"
					cfg.Profiles["qa"] = p
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				app.credentialStore = ""
				if fail {
					if store == "keyring" {
						app.DeleteCredential = func(string) error { return errors.New("delete failed") }
					} else {
						path, _ := app.credentialFilePath()
						if err := os.Remove(path); err != nil {
							t.Fatal(err)
						}
						if err := os.Mkdir(path, 0700); err != nil {
							t.Fatal(err)
						}
					}
				}
				out.Reset()
				exit, _ := runJSON(t, app, out, "logout", "--confirm")
				cfg, _ := app.loadConfig()
				_, exists := cfg.Profiles["qa"]
				if fail {
					if exit != ExitAuth || !exists || cfg.DefaultProfile != "qa" {
						t.Fatal(out, cfg)
					}
					return
				}
				if exit != ExitOK || exists || cfg.DefaultProfile != "default" {
					t.Fatal(out, cfg)
				}
				if store == "file" {
					raw, err := app.fileCredential("qa", nil)
					if err != nil || raw != "" {
						t.Fatal("file credential retained", err)
					}
				}
			})
		}
	}
}

func TestConfigSafetyInitGuards(t *testing.T) {
	merchant := "mer_123"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth-context" {
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"data":{"auth_type":"api_key","environment":"sandbox","merchant_id":%q,"sandbox_id":"test_123"}}`, merchant)
	}))
	defer server.Close()
	app, out, _ := testApp(t, server.URL)
	path := filepath.Join(app.WorkingDir, ".flint", "config.json")
	for i := 0; i < 2; i++ {
		out.Reset()
		exit, _ := runJSON(t, app, out, "init")
		if exit != ExitOK {
			t.Fatal(out)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), `"merchant_id": "mer_123"`) || !strings.Contains(string(raw), `"sandbox_id": "test_123"`) {
		t.Fatal(string(raw), err)
	}
	merchant = "mer_other"
	app.invalidateAuthContext(server.URL, "flint_test_test")
	out.Reset()
	exit, _ := runJSON(t, app, out, "init")
	if exit != ExitAuth || !strings.Contains(out.String(), "MERCHANT_GUARD_MISMATCH") {
		t.Fatal(out)
	}
	current, _ := os.ReadFile(path)
	if string(current) != string(raw) {
		t.Fatal("changed existing guard")
	}
	if err := os.WriteFile(path, []byte(`{"merchant":"mer_old"}`), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	exit, _ = runJSON(t, app, out, "init", "--force")
	if exit != ExitOK {
		t.Fatal(out)
	}
	current, _ = os.ReadFile(path)
	if !strings.Contains(string(current), `"merchant_id": "mer_other"`) {
		t.Fatal(string(current))
	}
}

func TestConfigSafetyInitIsMCPWrite(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:9")
	index, err := app.operationIndex()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := index.Lookup("init")
	if !ok || entry.Kind != "write" {
		t.Fatalf("init writes project guards: %+v", entry)
	}
}

func TestConfigSafetyMCPForceCannotReplaceGuards(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:9")
	path := filepath.Join(app.WorkingDir, ".flint", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"merchant_id":"mer_other","sandbox_id":"test_other"}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	result := mcpCall(t, app, defaultOptions(), "init", map[string]any{"force": true})
	if result["isError"] != true {
		t.Fatal(result)
	}
	current, err := os.ReadFile(path)
	if err != nil || string(current) != string(raw) {
		t.Fatal("MCP replaced guards", err)
	}
	cmd, _ := app.Registry.ByName("init")
	schema, e := schemaForMCPTool(cmd, mcpPolicy{})
	if e != nil {
		t.Fatal(e)
	}
	if _, ok := schema["properties"].(map[string]any)["force"]; ok {
		t.Fatal("MCP advertises force")
	}
}

func TestConfigSafetyInitDryRunDoesNotWriteOrRequest(t *testing.T) {
	app, out, _ := testApp(t, "http://127.0.0.1:9")
	app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("dry run sent HTTP"); return nil, nil })}
	exit, result := runJSON(t, app, out, "init", "--dry-run=client")
	if exit != ExitOK || lookupPathValue(result, "data.dry_run") != true {
		t.Fatal(out)
	}
	result = mcpCall(t, app, defaultOptions(), "init", map[string]any{"_flint": map[string]any{"dry_run": "client"}})
	if result["isError"] == true || lookupPathValue(mcpResultJSON(t, result), "data.dry_run") != true {
		t.Fatal(result)
	}
	if _, err := os.Stat(filepath.Join(app.WorkingDir, ".flint", "config.json")); !os.IsNotExist(err) {
		t.Fatal("dry run wrote project config", err)
	}
}

func TestConfigSafetyMCPInitHonorsLivePolicy(t *testing.T) {
	for _, scenario := range []struct{ allow, live bool }{{false, false}, {true, false}, {true, true}} {
		t.Run(fmt.Sprint(scenario), func(t *testing.T) {
			app, _, _, state, stored := newContextTestApp(t)
			credential, e := decodeOAuthCredential(stored())
			if e != nil {
				t.Fatal(e)
			}
			auth := contextTestAuth("ctx_live")
			credential.Auth = auth
			credential.ContextID = auth.ContextID
			installTestOAuth(t, app, credential)
			state.tokens[testOAuthAccess] = "ctx_live"
			if err := app.updateConfig(func(cfg *Config) error {
				p := cfg.Profiles["default"]
				p.ContextID = "ctx_live"
				cfg.Profiles["default"] = p
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			opts := defaultOptions()
			if scenario.allow {
				opts = mcpServeOptions(t, "--allow-live")
			}
			args := map[string]any{}
			if scenario.live {
				args["_flint"] = map[string]any{"live": true}
			}
			result := mcpCall(t, app, opts, "init", args)
			_, err := os.Stat(filepath.Join(app.WorkingDir, ".flint", "config.json"))
			want := scenario.allow && scenario.live
			if (result["isError"] != true) != want || (err == nil) != want {
				t.Fatalf("want=%v result=%v fileErr=%v", want, result, err)
			}
		})
	}
}
