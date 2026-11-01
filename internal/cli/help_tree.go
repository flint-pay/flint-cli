package cli

import (
	"fmt"
	"sort"
	"strings"
)

var helpStartHere = []string{"flint login", "flint signup", "flint init", "flint payment-links create", "flint help test-cards"}

func rootHelpIndex(reg *Registry) map[string]any {
	tree := reg.commandTree()
	groups := 0
	for _, group := range tree.Groups {
		if len(group.Words) == 1 {
			groups++
		}
	}
	return map[string]any{
		"usage": "flint <command> [flags]", "start_here": helpStartHere,
		"sections":       rootHelpSections(tree, false),
		"shortcuts":      map[string]string{"login": "auth.login", "logout": "auth.logout", "reauth": "auth.reauth", "whoami": "auth.status"},
		"topics":         helpTopicNames,
		"hosted_account": "Flint-hosted account operations work only with Flint's hosted account sign-in and are excluded from discovery", "counts": map[string]int{"commands": len(tree.Commands), "top_level_groups": groups},
		"all_commands": "flint help --all --output json", "catalog": "flint schema commands --output json", "search": "flint help search <words> --output json",
	}
}

func rootHelpSections(tree *CommandTree, all bool) []map[string]any {
	sections := []map[string]any{}
	for _, definition := range []struct{ id, title string }{{sectionDeveloper, "Developer tools"}, {sectionMerchant, "Merchant resources"}, {sectionBuyer, "Buyer session"}} {
		entries := []map[string]any{}
		seen := map[string]bool{}
		for _, cmd := range tree.Commands {
			if cmd.Section != definition.id {
				continue
			}
			if all {
				entries = append(entries, map[string]any{"name": cmd.Name, "command": cmd.Path, "usage": commandUsage(cmd), "description": cmd.Description})
				continue
			}
			top := cmd.Path[1]
			if seen[top] {
				continue
			}
			seen[top] = true
			entry := map[string]any{"name": top, "type": "command", "description": cmd.Description}
			if group := tree.Groups[top]; group != nil {
				entry["type"] = "group"
				entry["description"] = group.Description
				entry["command_count"] = len(group.Commands)
			}
			entries = append(entries, entry)
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i]["name"].(string) < entries[j]["name"].(string) })
		section := map[string]any{"id": definition.id, "title": definition.title, "entries": entries}
		if definition.id == sectionMerchant {
			section["credential"] = "merchant"
		}
		if definition.id == sectionBuyer {
			section["credential"] = "customer_session"
			section["credential_hint"] = "Set FLINT_ACCESS_TOKEN to a customer session secret (flint_cses_...)."
		}
		sections = append(sections, section)
	}
	return sections
}

func (a *App) commandHelpDocument(cmd *Command) map[string]any {
	if !cmd.Group {
		return commandDocument(cmd, cmd.Name)
	}
	group := a.Registry.commandTree().Groups[cmd.Name]
	commands := []map[string]any{}
	for _, child := range group.Commands {
		commands = append(commands, map[string]any{"name": child.Name, "command": child.Path, "usage": commandUsage(child), "description": child.Description, "method": child.Method, "api_path": child.APIPath})
	}
	return map[string]any{"name": group.Name, "type": "group", "section": group.Section, "description": group.Description, "commands": commands}
}

func (a *App) showCommandHelp(cmd *Command, opts Options) int {
	if opts.Output == "json" {
		return a.outputLocal(map[string]any{"data": a.commandHelpDocument(cmd)}, cmd, opts)
	}
	a.printCommandHelp(cmd)
	return ExitOK
}

func (a *App) printTreeRootHelp(all bool) {
	tree := a.Registry.commandTree()
	if !all {
		fmt.Fprintln(a.Stdout, "Flint Pay command-line interface\n\nUsage:")
		for _, line := range []string{"flint <command> [flags]", "flint help <command>          Help for a command or group", "flint help --all              Every command, one per line", "flint help search <words>     Find commands and topics offline"} {
			fmt.Fprintln(a.Stdout, "  "+line)
		}
		fmt.Fprintln(a.Stdout, "\nFlint-hosted account operations require Flint's hosted account sign-in and are excluded from discovery.")
		fmt.Fprintln(a.Stdout, "\nStart here:")
		for _, line := range helpStartHere {
			fmt.Fprintln(a.Stdout, "  "+line)
		}
	}
	for _, section := range rootHelpSections(tree, all) {
		title := section["title"].(string)
		if section["id"] == sectionBuyer {
			title += " (set FLINT_ACCESS_TOKEN to a customer session secret, flint_cses_...)"
		}
		fmt.Fprintln(a.Stdout, "\n"+title+":")
		entries := section["entries"].([]map[string]any)
		if all {
			for _, entry := range entries {
				fmt.Fprintf(a.Stdout, "  %-65s %s\n", entry["usage"], entry["description"])
			}
			continue
		}
		if section["id"] == sectionMerchant {
			width := 0
			for _, entry := range entries {
				width = max(width, len(entry["name"].(string)))
			}
			for i := 0; i < len(entries); i += 3 {
				var line strings.Builder
				for j := i; j < min(i+3, len(entries)); j++ {
					fmt.Fprintf(&line, "  %-*s", width, entries[j]["name"])
				}
				fmt.Fprintln(a.Stdout, strings.TrimRight(line.String(), " "))
			}
		} else {
			repeats := map[string]int{}
			for _, entry := range entries {
				repeats[entry["description"].(string)]++
			}
			for _, entry := range entries {
				description := entry["description"].(string)
				if repeats[description] > 1 {
					if group := tree.Groups[entry["name"].(string)]; group != nil {
						verbs := commandGroupVerbs(group.Commands)
						if len(verbs) > 4 {
							description = fmt.Sprintf("%d commands", len(group.Commands))
						} else {
							description = strings.Join(verbs, ", ")
						}
					}
				}
				fmt.Fprintf(a.Stdout, "  %-32s %s\n", entry["name"], description)
			}
		}
		if section["id"] == sectionDeveloper {
			fmt.Fprintln(a.Stdout, "  Shortcuts: login, logout, reauth (auth login, logout, reauth), whoami (auth status)")
		}
	}
	if all {
		return
	}
	fmt.Fprintln(a.Stdout, "\nScripts and AI agents:")
	for _, line := range []string{
		"--output json (or FLINT_OUTPUT=json) prints JSON results and errors on stdout and never prompts.",
		"Exit codes: 0 ok, 1 API error, 2 usage, 3 auth or config, 4 confirmation required, 5 network, 6 wait unmet, 70 internal error.",
		"--field, --select, and --wait-for take paths inside the resource; --jq sees the whole response.",
		"Lists return one page: pass --page-token, or use --all or --limit N.",
		"Reuse --idempotency-key when retrying a write that timed out.",
	} {
		fmt.Fprintln(a.Stdout, "  "+line)
	}
	fmt.Fprintln(a.Stdout, "\nEnvironment:")
	for _, line := range [][2]string{
		{"FLINT_API_KEY", "API key; overrides saved credentials"}, {"FLINT_OUTPUT", "human, json, or ndjson (streams and lists)"}, {"FLINT_NO_INPUT", "1, true, yes, or on to disable prompts"},
		{"FLINT_PROFILE", "Profile to use, like --profile"}, {"FLINT_MERCHANT", "Merchant guard, like --merchant"}, {"FLINT_WEBHOOK_SECRET", "Signing secret for flint listen"},
		{"FLINT_ACCESS_TOKEN", "Access token; takes precedence over FLINT_API_KEY"}, {"FLINT_WORKLOAD_IDENTITY_ID", "Workload identity; overrides saved login and contexts"},
		{"FLINT_OIDC_SOURCE", "auto, none, token, token_file, github_actions, gitlab_ci, kubernetes, gcp, azure, aws"}, {"FLINT_OIDC_AUDIENCE", "OIDC audience; required Entra app ID URI for Azure, otherwise defaults to the API origin"},
		{"FLINT_AZURE_CLIENT_ID", "User-assigned Azure managed identity client ID"},
		{"FLINT_OIDC_TOKEN", "OIDC JWT for workload identity exchange"}, {"FLINT_OIDC_TOKEN_FILE", "Flint OIDC token file; re-read on exchange"}, {"FLINT_OIDC_SCOPE", "Optional narrowing scopes for workload tokens"},
		{"FLINT_BASE_URL", "API server for development; sign in with a separate profile"},
		{"FLINT_COLOR", "auto, always, or never; overridden by --color"},
	} {
		fmt.Fprintf(a.Stdout, "  %-27s %s\n", line[0], line[1])
	}
	fmt.Fprintln(a.Stdout, "\nRun flint help <command> for one command. Run flint schema commands --output json for the full catalog.")
}
