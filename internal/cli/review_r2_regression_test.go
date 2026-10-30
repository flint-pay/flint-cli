package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func TestOAuthCommandLockAndRefreshBudgets(t *testing.T) {
	previousRequest, previousRefresh, previousLock := defaultRequestTimeout, oauthRefreshTimeout, localFileLockTimeout
	defaultRequestTimeout = 50 * time.Millisecond
	oauthRefreshTimeout = time.Second
	localFileLockTimeout = 2 * time.Second
	t.Cleanup(func() {
		defaultRequestTimeout, oauthRefreshTimeout, localFileLockTimeout = previousRequest, previousRefresh, previousLock
	})

	app, out, _ := testApp(t, "https://api.example.test")
	t.Setenv("FLINT_API_KEY", "")
	installTestOAuth(t, app, testOAuthCredential(t, "https://api.example.test", time.Now().Add(-time.Minute)))
	refreshes := 0
	app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case loginTokenPath:
			refreshes++
			// Both the lock wait and refresh exceed the injected default
			// command timeout. The refresh must get its own budget.
			select {
			case <-time.After(2 * defaultRequestTimeout):
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			writeTestTokens(w, "new-access", "new-refresh")
		case "/v1/auth-context":
			fmt.Fprint(w, oauthAuthJSON("sandbox"))
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
		return w.Result(), nil
	})}
	path, err := app.configPath()
	if err != nil {
		t.Fatal(err)
	}
	lock := flock.New(path + ".lock")
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	unlocked := make(chan error, 1)
	go func() {
		time.Sleep(2 * defaultRequestTimeout)
		unlocked <- lock.Unlock()
	}()
	// Exercise parsing and authenticateCommand with the default timeout,
	// rather than calling oauthAccess with an unbounded context.
	exit := app.Run([]string{"auth", "status", "--output", "json"})
	if err := <-unlocked; err != nil {
		t.Fatal(err)
	}
	if exit != ExitOK || refreshes != 1 || !strings.Contains(out.String(), `"auth_type":"oauth"`) {
		t.Fatalf("exit=%d refreshes=%d output=%s", exit, refreshes, out)
	}
}

func TestOAuthCommandLockWaitIsCancelable(t *testing.T) {
	app, _, _ := testApp(t, "https://api.example.test")
	t.Setenv("FLINT_API_KEY", "")
	installTestOAuth(t, app, testOAuthCredential(t, "https://api.example.test", time.Now().Add(time.Hour)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app.Context = ctx
	path, err := app.configPath()
	if err != nil {
		t.Fatal(err)
	}
	lock := flock.New(path + ".lock")
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	finished := make(chan int, 1)
	go func() { finished <- app.Run([]string{"auth", "status", "--output", "json"}) }()
	// Leave the lock held throughout: cancellation must wake the waiter.
	time.AfterFunc(50*time.Millisecond, cancel)
	select {
	case exit := <-finished:
		if exit == ExitOK {
			t.Fatal("canceled authentication succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not interrupt the lock wait")
	}
}

func TestOAuthCommandRetainsAPIRequestTimeout(t *testing.T) {
	app, out, _ := testApp(t, "https://api.example.test")
	t.Setenv("FLINT_API_KEY", "")
	installTestOAuth(t, app, testOAuthCredential(t, "https://api.example.test", time.Now().Add(time.Hour)))
	requestCanceled := false
	app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1/auth-context" {
			w := httptest.NewRecorder()
			fmt.Fprint(w, oauthAuthJSON("sandbox"))
			return w.Result(), nil
		}
		select {
		case <-r.Context().Done():
			requestCanceled = true
			return nil, r.Context().Err()
		case <-time.After(5 * time.Second):
			t.Error("API request did not time out")
			return nil, context.DeadlineExceeded
		}
	})}
	exit := app.Run([]string{"api", "get", "/v1/payment-intents", "--timeout", "50ms", "--output", "json"})
	if exit != ExitNetwork || !requestCanceled || !strings.Contains(out.String(), "CLIENT_TIMEOUT") {
		t.Fatalf("exit=%d requestCanceled=%t output=%s", exit, requestCanceled, out)
	}
}

func TestOAuthCommandRetainsAuthRequestTimeout(t *testing.T) {
	app, out, _ := testApp(t, "https://api.example.test")
	t.Setenv("FLINT_API_KEY", "")
	installTestOAuth(t, app, testOAuthCredential(t, "https://api.example.test", time.Now().Add(time.Hour)))
	requestCanceled := false
	app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		select {
		case <-r.Context().Done():
			requestCanceled = true
			return nil, r.Context().Err()
		case <-time.After(5 * time.Second):
			t.Error("auth context request did not time out")
			return nil, context.DeadlineExceeded
		}
	})}
	exit := app.Run([]string{"auth", "status", "--timeout", "50ms", "--output", "json"})
	if exit != ExitNetwork || !requestCanceled || !strings.Contains(out.String(), "OAUTH_CONTEXT_UNAVAILABLE") {
		t.Fatalf("exit=%d requestCanceled=%t output=%s", exit, requestCanceled, out)
	}
}

func TestHumanSchemaOutputTransforms(t *testing.T) {
	for _, command := range []string{"input", "output", "command"} {
		t.Run(command, func(t *testing.T) {
			field, scalar := "type", "object"
			label := "Type"
			if command == "command" {
				field, scalar = "data.name", "customers.get"
				label = "Name"
			}
			for _, test := range []struct {
				name, want string
				flags      []string
			}{
				{"jq", "selected-marker", []string{"--jq", `"selected-marker"`}},
				{"field", scalar, []string{"--field", field}},
				{"select human", label + " " + scalar, []string{"--select", strings.TrimPrefix(field, "data.")}},
				{"select", scalar, []string{"--select", strings.TrimPrefix(field, "data."), "--jq", "." + field}},
				{"invalid field", "Field path does not exist", []string{"--field", "missing_field"}},
			} {
				t.Run(test.name, func(t *testing.T) {
					app, out, stderr := testApp(t, "")
					args := append([]string{"schema", command, "customers.get"}, test.flags...)
					exit := app.Run(args)
					if test.name == "invalid field" {
						if exit != ExitUsage || !strings.Contains(stderr.String(), test.want) {
							t.Fatalf("exit=%d output=%s stderr=%s", exit, out, stderr)
						}
					} else if exit != ExitOK || strings.Join(strings.Fields(out.String()), " ") != test.want {
						t.Fatalf("exit=%d output=%s stderr=%s", exit, out, stderr)
					}
				})
			}
		})
	}
}
