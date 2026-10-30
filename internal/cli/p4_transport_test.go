package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apispec "github.com/flint-pay/flint-cli/internal/spec"
)

type p4RepeatedByte byte

func (b p4RepeatedByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

func p4JSONBody(padding int64) io.Reader {
	return io.MultiReader(strings.NewReader(`{"padding":"`), io.LimitReader(p4RepeatedByte('x'), padding), strings.NewReader(`"}`))
}

func TestP4OpenAPIResponseLimits(t *testing.T) {
	for _, test := range []struct {
		name, method, path string
		padding            int64
		limit              int
	}{
		{"openapi over normal cap", http.MethodGet, "/v1/openapi.json", 17 << 20, 0},
		{"openapi query", http.MethodGet, "/v1/openapi.json?format=json", 17 << 20, 0},
		{"openapi over larger cap", http.MethodGet, "/v1/openapi.json", 64 << 20, 64},
		{"ordinary get", http.MethodGet, "/v1/customers", 16 << 20, 16},
		{"openapi post", http.MethodPost, "/v1/openapi.json", 16 << 20, 16},
		{"openapi suffix", http.MethodGet, "/v1/openapi.json/", 16 << 20, 16},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, _, _ := testApp(t, "http://127.0.0.1:1")
			app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Request-Id": {"req_limit"}}, Body: io.NopCloser(p4JSONBody(test.padding))}, nil
			})}
			response, e := app.doRequest(context.Background(), "http://127.0.0.1:1", "", test.method, test.path, nil, "", false)
			if test.limit == 0 {
				if e != nil || response == nil {
					t.Fatalf("response=%v error=%v", response, e)
				}
				return
			}
			if e == nil || e.Code != "RESPONSE_TOO_LARGE" || e.RequestID != "req_limit" || !strings.Contains(e.Message, fmt.Sprintf("%d MiB", test.limit)) {
				t.Fatalf("error=%+v", e)
			}
		})
	}
}

func TestP4OpenAPISnapshotResponse(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:1")
	app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(apispec.OpenAPI))}, nil
	})}
	response, e := app.doRequest(context.Background(), "http://127.0.0.1:1", "", http.MethodGet, "/v1/openapi.json", nil, "", false)
	if e != nil || response == nil || len(response.Raw) != len(apispec.OpenAPI) {
		t.Fatalf("snapshot retrieval error=%v", e)
	}
}

func TestP4OpenAPIDecompressedLimit(t *testing.T) {
	for _, test := range []struct {
		name    string
		padding int64
		code    string
	}{
		{"compressed success", 17 << 20, ""},
		{"compressed too large", 64 << 20, "RESPONSE_TOO_LARGE"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
					t.Error("transport did not request gzip")
				}
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Set("X-Request-Id", "req_gzip")
				writer := gzip.NewWriter(w)
				_, _ = io.Copy(writer, p4JSONBody(test.padding))
				_ = writer.Close()
			}))
			defer server.Close()
			app, _, _ := testApp(t, server.URL)
			_, e := app.doRequest(context.Background(), server.URL, "", http.MethodGet, "/v1/openapi.json", nil, "", false)
			if test.code == "" {
				if e != nil {
					t.Fatal(e)
				}
			} else if e == nil || e.Code != test.code || e.RequestID != "req_gzip" {
				t.Fatalf("error=%+v", e)
			}
		})
	}
}

type p4ProbeBody struct {
	t      *testing.T
	closed bool
}

func (b *p4ProbeBody) Read([]byte) (int, error) {
	b.t.Error("version discovery read the OpenAPI response body")
	return 0, errors.New("body must not be read")
}

func (b *p4ProbeBody) Close() error { b.closed = true; return nil }

func TestP4VersionProbeReadsOnlyUnpinnedHeaders(t *testing.T) {
	for _, version := range []string{"2027-02-01", "", "not-a-date"} {
		t.Run(version, func(t *testing.T) {
			app, _, _ := testApp(t, "http://127.0.0.1:1")
			body := &p4ProbeBody{t: t}
			app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.String() != "http://127.0.0.1:1/custom/v1/openapi.json" || r.Header.Get("Authorization") != "" || r.Header.Get("Flint-Version") != "" {
					t.Errorf("unexpected probe: %s %s headers=%v", r.Method, r.URL, r.Header)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Flint-Version": {version}, "X-Request-Id": {"req_probe"}}, Body: body}, nil
			})}
			got, e := app.fetchCurrentAPIVersion(context.Background(), "http://127.0.0.1:1/custom/v1", false)
			if version == "2027-02-01" {
				if e != nil || got != version {
					t.Fatalf("version=%q error=%v", got, e)
				}
			} else if e == nil || e.RequestID != "req_probe" || e.Code != "INVALID_RESPONSE" {
				t.Fatalf("error=%+v", e)
			}
			if !body.closed {
				t.Error("probe left response body open")
			}
		})
	}
}

func TestP4VersionProbeRejectsRedirectsAndInvalidBaseURL(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("X-Request-Id", "req_redirect")
		http.Redirect(w, r, "/other", http.StatusFound)
	}))
	defer server.Close()
	app, _, _ := testApp(t, server.URL)
	app.HTTPClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	_, e := app.fetchCurrentAPIVersion(context.Background(), server.URL, false)
	if e == nil || e.HTTPStatus != http.StatusFound || e.RequestID != "req_redirect" || requests != 1 {
		t.Fatalf("requests=%d error=%+v", requests, e)
	}
	for _, baseURL := range []string{"http://example.invalid", server.URL + "?token=private", server.URL + "#fragment"} {
		if _, e := app.fetchCurrentAPIVersion(context.Background(), baseURL, false); e == nil || e.Code != "INVALID_BASE_URL" {
			t.Fatalf("baseURL=%q error=%+v", baseURL, e)
		}
	}
	if requests != 1 {
		t.Fatalf("invalid base URL made a request: %d", requests)
	}
}

func TestP4ResponseFailureRetainsRequestID(t *testing.T) {
	for _, raw := range []string{"not JSON", strings.Repeat("x", 16<<20+1)} {
		app, _, _ := testApp(t, "http://127.0.0.1:1")
		app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Request-Id": {"req_failed"}}, Body: io.NopCloser(strings.NewReader(raw))}, nil
		})}
		_, e := app.doRequest(context.Background(), "http://127.0.0.1:1", "", http.MethodGet, "/v1/customers", nil, "", false)
		if e == nil || e.RequestID != "req_failed" {
			t.Fatalf("error=%+v", e)
		}
	}
}

func TestP4DoctorContinuesAfterDiscoveryWarning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/openapi.json" {
			w.Header().Set("X-Request-Id", "req_missing_version")
			return
		}
		fmt.Fprint(w, authContextJSON("sandbox"))
	}))
	defer server.Close()
	app, stdout, _ := testApp(t, server.URL)
	if exit := app.Run([]string{"doctor", "--output", "json"}); exit != ExitOK || !strings.Contains(stdout.String(), `"status":"warning"`) || !strings.Contains(stdout.String(), `"name":"cli_version"`) || !strings.Contains(stdout.String(), `"request_id":"req_missing_version"`) {
		t.Fatalf("exit=%d output=%s", exit, stdout)
	}
}

type p4FailedReadBody struct{}

func (p4FailedReadBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (p4FailedReadBody) Close() error             { return nil }

func TestP4ReadFailureRetainsRequestID(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:1")
	app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Request-Id": {"req_read_failed"}}, Body: p4FailedReadBody{}}, nil
	})}
	// A no-key POST avoids retrying the injected read failure.
	_, e := app.doRequest(context.Background(), "http://127.0.0.1:1", "", http.MethodPost, "/v1/customers", nil, "", false)
	if e == nil || e.RequestID != "req_read_failed" {
		t.Fatalf("error=%+v", e)
	}
}

func TestP4OversizedAPIFailureRetainsRequestID(t *testing.T) {
	app, _, _ := testApp(t, "http://127.0.0.1:1")
	app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"X-Request-Id": {"req_api_failed"}}, Body: io.NopCloser(p4JSONBody(64 << 20))}, nil
	})}
	_, e := app.doRequest(context.Background(), "http://127.0.0.1:1", "", http.MethodGet, "/v1/openapi.json", nil, "", false)
	if e == nil || e.Code != "RESPONSE_TOO_LARGE" || e.HTTPStatus != http.StatusBadRequest || e.RequestID != "req_api_failed" {
		t.Fatalf("error=%+v", e)
	}
}
