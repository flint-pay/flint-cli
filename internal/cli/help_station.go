package cli

import (
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/flint-pay/flint-cli/internal/opindex"
)

// Support only builds a composer link; search uses the local operation index.
const helpStationURL = "https://help.withflintpay.com"
const helpSearchLimit = 8

// Product areas the composer accepts, mirroring HELP_PRODUCT_AREAS in
// web/libs/utils/src/helpLink.ts, which is the link schema every other Flint
// surface builds from. The composer drops an unknown area silently, so the CLI
// rejects it with the list instead.
var supportProductAreas = []string{
	"checkout",
	"payments",
	"payouts",
	"disputes",
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

// cliSearchFilter includes the terminal's full catalog, independent of MCP
// exposure and credentials. Synthetic MCP operations have no terminal command.
func (a *App) cliSearchFilter() opindex.Filter {
	return opindex.Filter{Limit: helpSearchLimit, Exposed: func(entry opindex.Entry) bool {
		switch entry.Type {
		case "operation":
			cmd, ok := a.Registry.ByName(entry.ID)
			return ok && cmd.CanonicalName == entry.ID && entry.ID != "__complete"
		case "event":
			return true
		case "topic":
			return slices.Contains(helpTopicNames, entry.ID)
		}
		return false
	}}
}

func (a *App) localHelpSearch(cmd *Command, opts Options) int {
	query := strings.TrimSpace(strings.Join(opts.Positionals, " "))
	if query == "" {
		return a.fail(usageError("MISSING_REQUIRED_ARGUMENT", "Missing required argument: query", "query"), opts)
	}
	index, err := a.operationIndex()
	if err != nil {
		return a.fail(cliError(ExitSoftware, errorTypeInternal, "SCHEMA_SNAPSHOT_INVALID", err.Error()), opts)
	}
	a.Registry.commandTree()
	hits, _ := index.Search(query, a.cliSearchFilter())
	items := make([]any, 0, len(hits))
	for _, hit := range hits {
		item := map[string]any{"name": hit.ID, "type": hit.Type, "description": hit.Title}
		switch hit.Type {
		case "operation":
			command, _ := a.Registry.ByName(hit.ID)
			item["type"] = "command"
			item["command"] = strings.Join(command.Path, " ")
			item["description"] = command.Description
			item["section"] = command.Section
			item["method"] = command.Method
			item["api_path"] = command.APIPath
			item["operation_id"] = command.OperationID
		case "event":
			item["command"] = "flint help events " + hit.ID
		case "topic":
			item["command"] = "flint help " + hit.ID
		}
		items = append(items, item)
	}
	data := map[string]any{"query": query, "results": items}
	if len(items) == 0 {
		data["hint"] = "Try fewer words or run flint help --all."
	}
	return a.outputLocal(map[string]any{"data": data}, cmd, opts)
}

func renderHelpSearchHuman(w io.Writer, value any, _ time.Time) {
	data, _ := lookupPath(value, "data.results")
	results, _ := data.([]any)
	if len(results) == 0 {
		fmt.Fprintln(w, "No commands, events, or topics matched. Try fewer words or run flint help --all.")
	}
	for _, raw := range results {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		command, _ := item["command"].(string)
		description, _ := item["description"].(string)
		kind, _ := item["type"].(string)
		route := kind
		if kind == "command" {
			method, _ := item["method"].(string)
			path, _ := item["api_path"].(string)
			route = strings.TrimSpace(method + " " + path)
		}
		fmt.Fprintf(w, "%-44s %-45s %s\n", strings.TrimPrefix(command, "flint "), description, route)
	}
	fmt.Fprintln(w, "Run flint <command> --help for flags. To ask Flint, run flint support open.")
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
	target := buildSupportComposerURL(helpStationURL, composer)
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
