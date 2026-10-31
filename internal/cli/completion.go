package cli

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
)

func completionCommand() *Command {
	return &Command{Name: "completion", Path: pathWithFlint("completion"), Description: "Print an offline bash, zsh, or fish completion script", Local: true, MCPHidden: true, Arguments: []Arg{{Name: "shell", Type: "string", Positional: 1, Required: true, Enum: []string{"bash", "zsh", "fish"}, Description: "Shell to configure"}}, Examples: ex("flint completion bash", "flint completion zsh", "flint completion fish")}
}

func (a *App) localCompletion(cmd *Command, opts Options) int {
	script := completionScripts[opts.Positionals[0]]
	if opts.Output == "json" {
		return a.outputLocal(map[string]any{"data": map[string]any{"shell": opts.Positionals[0], "script": script}}, cmd, opts)
	}
	if _, err := io.WriteString(a.Stdout, script); err != nil {
		return a.fail(networkError("OUTPUT_WRITE_FAILED", "Could not write the completion script.", err), opts)
	}
	return ExitOK
}

var completionScripts = map[string]string{
	"bash": `_flint_complete() {
    local candidate prefix=""
    if [[ ${COMP_WORDS[COMP_CWORD-1]} == = ]]; then prefix="${COMP_WORDS[COMP_CWORD-2]}=";
    elif [[ ${COMP_WORDS[COMP_CWORD]} == = ]]; then prefix="${COMP_WORDS[COMP_CWORD-1]}="; fi
    COMPREPLY=()
    while IFS= read -r candidate; do COMPREPLY+=("${candidate#"$prefix"}"); done < <(flint __complete "${COMP_WORDS[@]:1:COMP_CWORD}")
}
complete -F _flint_complete flint
`,
	"zsh": `#compdef flint
_flint_complete() {
    local -a candidates
    candidates=("${(@f)$(flint __complete "${words[@]:1:$((CURRENT-1))}")}")
    compadd -- "${candidates[@]}"
}
compdef _flint_complete flint
`,
	"fish": `function __flint_complete
    set -l words (commandline -opc)
    set -e words[1]
    set -l current (commandline -ct)
    flint __complete $words "$current"
end
complete -c flint -f -a '(__flint_complete)'
`,
}

// __complete is intentionally absent from the public tree. It consumes the
// shell's words directly, before normal flags, credentials, or environment IO.
func (a *App) runCompletionWords(words []string) int {
	for _, candidate := range a.completionCandidates(words) {
		if _, err := fmt.Fprintln(a.Stdout, candidate); err != nil {
			return ExitNetwork
		}
	}
	return ExitOK
}

func (a *App) completionCandidates(words []string) []string {
	// Bash's default word breaks split --flag=value into --flag, =, value.
	normalized := make([]string, 0, len(words))
	for i := 0; i < len(words); i++ {
		if words[i] == "=" && len(normalized) > 0 && strings.HasPrefix(normalized[len(normalized)-1], "--") {
			normalized[len(normalized)-1] += "="
			if i+1 < len(words) {
				i++
				normalized[len(normalized)-1] += words[i]
			}
		} else {
			normalized = append(normalized, words[i])
		}
	}
	words = normalized
	tree := a.Registry.commandTree()
	if len(words) == 0 {
		words = []string{""}
	}
	current := words[len(words)-1]
	previous := words[:len(words)-1]
	var commandWords []string
	var cmd *Command
	var pending *Arg
	positionals := 0
	terminated := false
	for i := 0; i < len(previous); i++ {
		word := previous[i]
		if pending != nil {
			pending = nil
			continue
		}
		if word == "--" {
			terminated = true
			continue
		}
		if !terminated && strings.HasPrefix(word, "--") {
			key, _, inline := strings.Cut(word, "=")
			if arg, ok := completionArgument(cmd, key); ok && !inline {
				if arg.Type != "boolean" {
					copy := arg
					pending = &copy
				} else if i+1 < len(previous) && (previous[i+1] == "true" || previous[i+1] == "false") {
					i++
				}
			}
			continue
		}
		if cmd != nil {
			positionals++
			continue
		}
		commandWords = append(commandWords, word)
		resolution := tree.Resolve(commandWords)
		cmd = resolution.Command
	}
	if pending != nil {
		return completionValues(*pending, current, "")
	}
	if !terminated && strings.HasPrefix(current, "--") {
		if key, value, inline := strings.Cut(current, "="); inline {
			if arg, ok := completionArgument(cmd, key); ok {
				return completionValues(arg, value, key+"=")
			}
			return nil
		}
	}
	candidates := map[string]bool{}
	add := func(value string) {
		if strings.HasPrefix(value, current) {
			candidates[value] = true
		}
	}
	if !terminated && (current == "" || strings.HasPrefix(current, "-")) {
		for _, arg := range completionFlagArguments(cmd) {
			if arg.Flag == "" {
				continue
			}
			add(arg.Flag)
		}
	}
	if !strings.HasPrefix(current, "-") {
		if cmd == nil {
			for _, command := range tree.Commands {
				paths := append([][]string{command.Path}, command.AliasPaths...)
				for _, path := range paths {
					path = path[1:]
					if len(path) > len(commandWords) && slices.Equal(path[:len(commandWords)], commandWords) {
						add(path[len(commandWords)])
					}
				}
			}
		} else {
			for _, arg := range cmd.Arguments {
				if arg.Positional == positionals+1 {
					for _, value := range completionValues(arg, current, "") {
						add(value)
					}
				}
			}
		}
	}
	out := make([]string, 0, len(candidates))
	for value := range candidates {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func completionValues(arg Arg, current, prefix string) []string {
	if arg.Repeat {
		if index := strings.LastIndex(current, ","); index >= 0 {
			prefix += current[:index+1]
			current = current[index+1:]
		}
	}
	values := arg.Enum
	if arg.Type == "boolean" {
		values = []string{"true", "false"}
	}
	var out []string
	for _, value := range values {
		if strings.HasPrefix(value, current) {
			out = append(out, prefix+value)
		}
	}
	sort.Strings(out)
	return out
}

func completionArgument(cmd *Command, flag string) (Arg, bool) {
	for _, arg := range completionFlagArguments(cmd) {
		if arg.Flag == flag {
			return arg, true
		}
	}
	key := strings.TrimPrefix(flag, "--")
	if globalFlags[key] {
		arg := Arg{Flag: flag}
		if boolFlags[key] {
			arg.Type = "boolean"
		}
		return arg, true
	}
	return Arg{}, false
}

func completionFlagArguments(cmd *Command) []Arg {
	args := []Arg{
		{Flag: "--help", Type: "boolean"}, {Flag: "--output", Enum: []string{"human", "json", "ndjson"}},
		{Flag: "--quiet", Type: "boolean"}, {Flag: "--debug", Type: "boolean"}, {Flag: "--no-input", Type: "boolean"},
		{Flag: "--profile"}, {Flag: "--color", Enum: []string{"auto", "always", "never"}},
		{Flag: "--credential-store", Enum: []string{"keyring", "file"}},
	}
	if cmd == nil {
		return args
	}
	args = append(args, cmd.Arguments...)
	add := func(flag string, values ...string) { args = append(args, Arg{Flag: flag, Enum: values}) }
	if !cmd.Local {
		add("--live")
		add("--merchant")
		add("--timeout")
	}
	if supportsContextSelection(cmd) {
		add("--context")
	}
	if cmd.Mutation {
		add("--confirm")
		add("--dry-run", "client")
		add("--idempotency-key")
	}
	if cmd.Supports.Pagination {
		for _, flag := range []string{"--all", "--limit", "--page-size", "--page-token"} {
			add(flag)
		}
	}
	if cmd.Supports.WaitFor {
		add("--wait-for")
		add("--for")
	}
	if cmd.Stream {
		add("--max-events")
		add("--for")
	}
	if cmd.Stream || cmd.Supports.Pagination || cmd.Supports.WaitFor {
		add("--progress", "auto", "plain", "json", "quiet")
	}
	if cmd.Supports.Field {
		add("--field")
	}
	if cmd.Supports.Select {
		add("--select")
	}
	if cmd.Supports.JQ {
		add("--jq")
	}
	if cmd.Supports.Open {
		add("--open")
	}
	if values, _ := expansionValues(cmd.OperationID); len(values) > 0 {
		args = append(args, Arg{Flag: "--expand", Repeat: true, Enum: values})
	}
	// Global booleans have no value to consume while walking shell words.
	for i := range args {
		if boolFlags[strings.TrimPrefix(args[i].Flag, "--")] {
			args[i].Type = "boolean"
		}
	}
	return args
}
