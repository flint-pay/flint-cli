package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Flint Help is the community and support site. It is a separate origin from
// the public API, so it has its own base URL rather than reusing FLINT_BASE_URL.
const helpStationURL = "https://help.withflintpay.com"

// The search API is public, so these commands never send a credential. The
// composer link carries context only; HelpService is first-party, and there is
// no public support-threads route to post through yet.
const helpSearchPath = "/api/search"
const helpSearchLimit = 8

// Product areas the composer accepts, mirroring HELP_PRODUCT_AREAS in
// web/libs/utils/src/helpLink.ts, which is the link schema every other Flint
// surface builds from. The composer drops an unknown area silently, so the CLI
// rejects it with the list instead.
var supportProductAreas = []string{
	"checkout",
	"payments",
	"payouts",
	"invoices",
	"subscriptions",
	"payment_links",
	"api",
	"webhooks",
	"dashboard",
	"documentation",
	"flint_billing",
	"account",
	"sales",
}

type helpSearchResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Slug        string `json:"slug"`
	Status      string `json:"status"`
	Kind        string `json:"kind"`
	ProductArea string `json:"productArea"`
	Excerpt     string `json:"excerpt"`
	ReplyCount  int    `json:"replyCount"`
	Answered    bool   `json:"answered"`
	// Passed through untouched: Flint Help serves an epoch number, and the CLI
	// has no reason to reinterpret it.
	UpdatedAt any `json:"updatedAt"`
}

func helpStationBaseURL() (string, *CLIError) {
	value := strings.TrimSpace(os.Getenv("FLINT_HELP_URL"))
	if value == "" {
		return helpStationURL, nil
	}
	normalized, err := validateBaseURL(value)
	if err != nil {
		return "", configError(
			"INVALID_HELP_URL",
			"FLINT_HELP_URL must be an absolute HTTP or HTTPS URL without credentials, a query, or a fragment.",
			err,
		)
	}
	return normalized, nil
}

func helpAskURL(base string) string {
	return base + "/new?kind=question&source=cli.help"
}

func (a *App) localHelpSearch(cmd *Command, opts Options) int {
	query := strings.TrimSpace(strings.Join(opts.Positionals, " "))
	if query == "" {
		return a.fail(usageError("MISSING_REQUIRED_ARGUMENT", "Missing required argument: query", "query"), opts)
	}
	base, baseErr := helpStationBaseURL()
	if baseErr != nil {
		return a.fail(baseErr, opts)
	}
	results, searchErr := a.searchFlintHelp(base, query, opts)
	if searchErr != nil {
		return a.fail(searchErr, opts)
	}
	items := make([]any, 0, len(results))
	for _, result := range results {
		items = append(items, map[string]any{
			"title":        result.Title,
			"url":          result.URL,
			"slug":         result.Slug,
			"status":       result.Status,
			"kind":         result.Kind,
			"product_area": result.ProductArea,
			"excerpt":      result.Excerpt,
			"reply_count":  result.ReplyCount,
			"answered":     result.Answered,
			"updated_at":   result.UpdatedAt,
		})
	}
	return a.outputLocal(map[string]any{"data": map[string]any{
		"query":   query,
		"results": items,
		"ask_url": helpAskURL(base),
	}}, cmd, opts)
}

func (a *App) searchFlintHelp(base, query string, opts Options) ([]helpSearchResult, *CLIError) {
	target := base + helpSearchPath + "?q=" + url.QueryEscape(query) + "&limit=" + strconv.Itoa(helpSearchLimit)
	ctx := a.Context
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, usageError("INVALID_REQUEST_URL", err.Error(), "query")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "flintpay-cli/"+a.Info.Version)
	request.Header.Set("X-Flint-CLI-Version", a.Info.Version)
	if opts.Debug {
		fmt.Fprintf(a.Stderr, "debug: GET %s\n", target)
	}
	response, err := a.helpHTTPClient().Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &CLIError{ExitCode: ExitNetwork, Type: "timeout_error", Code: "HELP_SEARCH_TIMEOUT", Message: "Flint Help did not respond before the timeout.", Cause: err}
		}
		return nil, networkError("HELP_SEARCH_UNREACHABLE", "Flint Help could not be reached: "+err.Error(), err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, networkError("HELP_SEARCH_READ_FAILED", "Could not read the Flint Help search response.", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &CLIError{
			ExitCode: ExitAPI,
			Type:     "unavailable_error",
			Code:     "HELP_SEARCH_FAILED",
			Message:  fmt.Sprintf("Flint Help search returned HTTP %d.", response.StatusCode),
			Details:  map[string]any{"status": response.StatusCode, "url": target},
		}
	}
	var results []helpSearchResult
	if err := json.Unmarshal(body, &results); err != nil {
		return nil, invalidResponseError("INVALID_HELP_SEARCH_RESPONSE", "Flint Help returned a search response the CLI could not read.", err)
	}
	return results, nil
}

func (a *App) helpHTTPClient() *http.Client {
	if a.HTTPClient != nil {
		return a.HTTPClient
	}
	// Public help searches retain normal redirect handling while sharing the
	// bounded connection pool. API requests keep their stricter redirect policy.
	client := *defaultAPIHTTPClient
	client.CheckRedirect = nil
	return &client
}

func renderHelpSearchHuman(w io.Writer, value any, _ time.Time) {
	data, _ := lookupPath(value, "data.results")
	results, _ := data.([]any)
	if len(results) == 0 {
		fmt.Fprintln(w, "No Flint Help threads matched that search.")
	}
	for index, raw := range results {
		result, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		title, _ := result["title"].(string)
		line := fmt.Sprintf("%d. %s", index+1, title)
		if answered, _ := result["answered"].(bool); answered {
			line += "  ✓ answered"
		}
		if area, _ := result["product_area"].(string); area != "" {
			line += "  " + area
		}
		line += "  " + pluralizeReplies(result["reply_count"])
		fmt.Fprintln(w, line)
		if target, _ := result["url"].(string); target != "" {
			fmt.Fprintln(w, "   "+target)
		}
	}
	if ask, ok := firstStringAt(value, "data.ask_url"); ok {
		fmt.Fprintln(w, "Ask your own: "+ask)
	}
}

func pluralizeReplies(value any) string {
	count := 0
	switch typed := value.(type) {
	case int:
		count = typed
	case float64:
		count = int(typed)
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			count = int(parsed)
		}
	}
	if count == 1 {
		return "1 reply"
	}
	return strconv.Itoa(count) + " replies"
}

// supportComposerContext is the terminal-side context the composer prefills.
// Nothing here is posted: the CLI only builds and opens the link.
type supportComposerContext struct {
	Title       string
	Body        string
	AIAgent     bool
	Area        string
	Private     bool
	RequestID   string
	Resource    string
	Environment string
}

// buildSupportComposerURL preserves the context parameter order from
// web/libs/utils/src/helpLink.ts, with optional title, body, and AI status before source.
func buildSupportComposerURL(base string, composer supportComposerContext) string {
	pairs := [][2]string{{"kind", "question"}}
	if composer.Area != "" {
		pairs = append(pairs, [2]string{"area", composer.Area})
	}
	if composer.Private {
		pairs = append(pairs, [2]string{"visibility", "private"})
	}
	if composer.Environment != "" {
		pairs = append(pairs, [2]string{"env", composer.Environment})
	}
	if composer.Resource != "" {
		pairs = append(pairs, [2]string{"resource", composer.Resource})
	}
	if composer.RequestID != "" {
		pairs = append(pairs, [2]string{"request_id", composer.RequestID})
	}
	if composer.Title != "" {
		pairs = append(pairs, [2]string{"title", composer.Title})
	}
	if composer.Body != "" {
		pairs = append(pairs, [2]string{"body", composer.Body})
	}
	if composer.AIAgent {
		pairs = append(pairs, [2]string{"ai_agent", "true"})
	}
	pairs = append(pairs, [2]string{"source", "cli.support"})
	encoded := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		encoded = append(encoded, url.QueryEscape(pair[0])+"="+url.QueryEscape(pair[1]))
	}
	return base + "/new?" + strings.Join(encoded, "&")
}

func (a *App) localSupportOpen(cmd *Command, opts Options) int {
	base, baseErr := helpStationBaseURL()
	if baseErr != nil {
		return a.fail(baseErr, opts)
	}
	area := lastRawOption(opts, "area")
	if area != "" && !slices.Contains(supportProductAreas, area) {
		return a.fail(usageError(
			"INVALID_PRODUCT_AREA",
			"--area must be one of: "+strings.Join(supportProductAreas, ", ")+".",
			"area",
		), opts)
	}
	// Preserve whitespace in prose, including Markdown indentation and newlines.
	textOption := func(name string) string {
		values := opts.Raw[name]
		if len(values) == 0 {
			return ""
		}
		return values[len(values)-1]
	}
	composer := supportComposerContext{
		Title:       textOption("title"),
		Body:        textOption("body"),
		AIAgent:     booleanRawOption(opts, "ai-agent"),
		Area:        area,
		Private:     booleanRawOption(opts, "private"),
		RequestID:   lastRawOption(opts, "request-id"),
		Resource:    lastRawOption(opts, "resource"),
		Environment: a.supportEnvironmentHint(opts),
	}
	target := buildSupportComposerURL(base, composer)
	opened := false
	// A browser is only useful for a human at a terminal. Machine modes and
	// --no-open print the link so the caller can hand it on.
	if !booleanRawOption(opts, "no-open") && !opts.NoInput && a.IsTTY() {
		if err := openBrowser(target); err != nil {
			if !opts.Quiet {
				fmt.Fprintln(a.Stderr, "warning: could not open browser: "+err.Error())
			}
		} else {
			opened = true
		}
	}
	return a.outputLocal(map[string]any{"data": map[string]any{
		"url":         target,
		"opened":      opened,
		"kind":        "question",
		"title":       composer.Title,
		"body":        composer.Body,
		"ai_agent":    composer.AIAgent,
		"area":        composer.Area,
		"visibility":  supportVisibility(composer.Private),
		"request_id":  composer.RequestID,
		"resource":    composer.Resource,
		"environment": composer.Environment,
	}}, cmd, opts)
}

// supportEnvironmentHint labels the composer with the environment the caller is
// working in. It is a hint, not an assertion: a profile without a readable
// credential still gets a working composer link, just without the label.
func (a *App) supportEnvironmentHint(opts Options) string {
	resolved, _, err := a.resolveConfig(opts)
	if err != nil {
		return ""
	}
	key, _, err := a.resolveCredential(resolved.ProfileName)
	if err != nil || key == "" {
		return ""
	}
	if isOAuthCredential(key) {
		c, e := decodeOAuthCredential(key)
		if e != nil {
			return ""
		}
		return c.Auth.Environment
	}
	environment, err := credentialEnvironment(key)
	if err != nil {
		return ""
	}
	return environment
}

func supportVisibility(private bool) string {
	if private {
		return "private"
	}
	return "public"
}

func renderSupportOpenHuman(w io.Writer, value any) {
	if target, ok := firstStringAt(value, "data.url"); ok {
		fmt.Fprintln(w, target)
	}
	if opened, _ := lookupPath(value, "data.opened"); opened == true {
		fmt.Fprintln(w, "Opened in your browser. Post from there.")
		return
	}
	fmt.Fprintln(w, "Open the link to post. The CLI does not create the thread.")
}

func booleanRawOption(opts Options, name string) bool {
	return lastRawOption(opts, name) == "true"
}
