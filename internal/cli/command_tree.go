package cli

import (
	"sort"
	"strings"
)

const (
	sectionDeveloper = "developer_tools"
	sectionMerchant  = "merchant_resources"
	sectionBuyer     = "buyer_session"
)

type CommandTree struct {
	Commands []*Command
	Groups   map[string]*CommandGroup
	byWords  map[string]*Command
}

type CommandGroup struct {
	Name        string
	Words       []string
	Section     string
	Description string
	Commands    []*Command
}

type CommandResolution struct {
	Command     *Command
	Group       *CommandGroup
	Topic       string
	Consumed    int
	Arguments   []string
	Suggestions []string
}

func (r *Registry) treeSnapshot() *CommandTree {
	r.treeBuild.Do(func() { r.tree = newCommandTree(r.Commands) })
	return r.tree
}

func (r *Registry) commandTree() *CommandTree {
	r.ensureAPICommands()
	return r.treeSnapshot()
}

func (r *Registry) resolveWords(words []string) CommandResolution {
	resolution := r.treeSnapshot().Resolve(words)
	if resolution.Command == nil && r.localOnly {
		r.ensureAPICommands()
		resolution = r.treeSnapshot().Resolve(words)
	}
	return resolution
}

func newCommandTree(commands []*Command) *CommandTree {
	tree := &CommandTree{Commands: commands, Groups: map[string]*CommandGroup{}, byWords: map[string]*Command{}}
	tops := map[string][]*Command{}
	for _, cmd := range commands {
		words := cmd.Path[1:]
		tree.byWords[strings.Join(words, " ")] = cmd
		for _, alias := range cmd.AliasPaths {
			tree.byWords[strings.Join(alias[1:], " ")] = cmd
		}
		if commandOwned(cmd) {
			cmd.Audience = "local"
			cmd.Kind = "cli"
		} else {
			cmd.Audience = commandAudience(cmd)
		}
		tops[words[0]] = append(tops[words[0]], cmd)
		for n := 1; n < len(words); n++ {
			name := strings.Join(words[:n], ".")
			group := tree.Groups[name]
			if group == nil {
				group = &CommandGroup{Name: name, Words: append([]string{}, words[:n]...)}
				tree.Groups[name] = group
			}
			group.Commands = append(group.Commands, cmd)
		}
	}
	for _, members := range tops {
		section := commandSection(members)
		for _, cmd := range members {
			cmd.Section = section
		}
	}
	for _, group := range tree.Groups {
		group.Section = group.Commands[0].Section
		group.Description = commandGroupDescription(group.Words[0], group.Commands)
	}
	return tree
}

func commandOwned(cmd *Command) bool {
	return cmd.Local || cmd.OperationID == "" || cmd.OperationID == "getAuthContext" || cmd.OperationID == "streamWebhookEvents"
}

func commandAudience(cmd *Command) string {
	if len(cmd.Security) == 0 {
		return "keyless"
	}
	for _, requirement := range cmd.Security {
		for scheme := range requirement {
			if scheme != "CustomerSessionBearer" && scheme != "CheckoutSessionBearer" && scheme != "InvoiceAccessTokenBearer" {
				return "merchant"
			}
		}
	}
	return "buyer"
}

func commandSection(commands []*Command) string {
	buyer, developer := true, true
	for _, cmd := range commands {
		if commandOwned(cmd) {
			return sectionDeveloper
		}
		buyer = buyer && cmd.Audience == "buyer"
		operation, _ := openAPIOperationByID(cmd.OperationID)
		scoped := len(operation.FlintRequiredScopes) > 0
		for _, scope := range operation.FlintRequiredScopes {
			scoped = scoped && strings.HasPrefix(scope, "developer.")
		}
		developer = developer && (cmd.Audience == "keyless" || scoped)
	}
	if buyer {
		return sectionBuyer
	}
	if developer {
		return sectionDeveloper
	}
	return sectionMerchant
}

var localGroupDescriptions = map[string]string{
	"auth":    "Sign in and out, import keys, export tokens, and show the active credential",
	"config":  "Read, set, unset, and validate CLI configuration",
	"context": "List and switch authorized contexts",
	"help":    "Command help, reference topics, and offline search",
	"mcp":     "Serve Flint operations to AI agents over MCP",
	"schema":  "Command, input, output, error, and event schemas as JSON",
	"support": "Start a Flint Help thread with your context prefilled",
}

func commandGroupDescription(top string, commands []*Command) string {
	if text := localGroupDescriptions[top]; text != "" {
		return text
	}
	counts := map[string]int{}
	for _, cmd := range commands {
		if operation, ok := openAPIOperationByID(cmd.OperationID); ok {
			for _, tag := range operation.Tags {
				counts[tag]++
			}
		}
	}
	best := ""
	for tag, count := range counts {
		if count > counts[best] || count == counts[best] && tag < best {
			best = tag
		}
	}
	if best != "" {
		doc, _ := loadOpenAPI()
		for _, tag := range doc.Tags {
			if tag.Name == best && tag.Description != "" {
				return tag.Description
			}
		}
	}
	if len(commands) == 1 {
		return commands[0].Description
	}
	return strings.Join(commandGroupVerbs(commands), ", ")
}

func commandGroupVerbs(commands []*Command) []string {
	seen := map[string]bool{}
	verbs := []string{}
	for _, cmd := range commands {
		words := strings.Split(cmd.Name, ".")
		verb := words[len(words)-1]
		if !seen[verb] {
			seen[verb] = true
			verbs = append(verbs, verb)
		}
	}
	sort.Strings(verbs)
	return verbs
}

func commandWords(words []string) []string {
	result := []string{}
	for _, word := range words {
		result = append(result, strings.Fields(strings.ReplaceAll(word, ".", " "))...)
	}
	if len(result) > 0 && result[0] == "flint" {
		result = result[1:]
	}
	return result
}

func (tree *CommandTree) Resolve(words []string) CommandResolution {
	offset := 0
	if len(words) > 0 && words[0] == "flint" {
		words = words[1:]
		offset = 1
	}
	for n := len(words); n > 0; n-- {
		if cmd := tree.byWords[strings.Join(commandWords(words[:n]), " ")]; cmd != nil {
			return CommandResolution{Command: cmd, Consumed: n + offset, Arguments: words[n:]}
		}
	}
	if group := tree.Groups[strings.Join(commandWords(words), ".")]; group != nil {
		return CommandResolution{Group: group, Consumed: len(words) + offset}
	}
	if len(words) > 0 {
		for _, topic := range helpTopicNames {
			if words[0] == topic {
				return CommandResolution{Topic: topic, Consumed: 1 + offset, Arguments: words[1:]}
			}
		}
	}
	return CommandResolution{Suggestions: tree.Suggestions(words)}
}

func (tree *CommandTree) Suggestions(words []string) []string {
	input := strings.Join(commandWords(words), " ")
	seen := map[string]bool{}
	candidates := []string{}
	add := func(candidate string) {
		if candidate == input || seen[candidate] {
			return
		}
		seen[candidate] = true
		if editDistance(input, candidate) <= max(2, len(input)/3) || len(input) >= 4 && strings.HasPrefix(candidate, input) {
			candidates = append(candidates, candidate)
		}
	}
	for name := range tree.byWords {
		add(name)
	}
	for _, group := range tree.Groups {
		add(strings.Join(group.Words, " "))
	}
	sortStringsByDistance(candidates, input)
	if len(candidates) > 3 {
		candidates = candidates[:3]
	}
	return candidates
}

func groupCommand(group *CommandGroup) *Command {
	return &Command{Name: group.Name, CanonicalName: group.Name, Path: pathWithFlint(group.Words...), Description: group.Description, Section: group.Section, Local: true, Group: true}
}

func commandUsage(cmd *Command) string {
	words := append([]string{}, cmd.Path...)
	for _, arg := range cmd.Arguments {
		if arg.Positional > 0 {
			name := "<" + arg.Name + ">"
			if arg.Variadic {
				name += "..."
			}
			if !arg.Required {
				name = "[" + name + "]"
			}
			words = append(words, name)
		}
	}
	return strings.Join(words, " ")
}

func buyerCredentialName(cmd *Command) string {
	for _, requirement := range cmd.Security {
		if _, ok := requirement["CheckoutSessionBearer"]; ok {
			return "checkout session token"
		}
		if _, ok := requirement["InvoiceAccessTokenBearer"]; ok {
			return "invoice access token"
		}
	}
	return "customer session secret"
}
