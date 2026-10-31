package cli

import "strings"

// Help has its own tolerant pass so invalid values and missing arguments never
// prevent inspection. A terminator protects literal --help positional values.
func parseHelpInvocation(r *Registry, argv []string) (*Command, Options, bool, *CLIError) {
	opts := defaultOptions()
	requested := false
	end := len(argv)
	for i, arg := range argv {
		if arg == "--" {
			end = i
			break
		}
		if arg == "--help" || arg == "-h" || arg == "--help=true" {
			requested = true
		}
		if arg == "--help=false" || (arg == "--help" || arg == "-h") && i+1 < len(argv) && argv[i+1] == "false" {
			requested = false
		}
	}
	if !requested {
		return nil, opts, false, nil
	}
	words := []string{}
	for i := 0; i < end; i++ {
		arg := argv[i]
		if !strings.HasPrefix(arg, "-") {
			words = append(words, arg)
			continue
		}
		key, inline, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if arg == "-h" {
			key = "help"
		}
		if arg == "-o" {
			key = "output"
		}
		value := inline
		if !hasValue && i+1 < end && !strings.HasPrefix(argv[i+1], "-") {
			if boolFlags[key] {
				if argv[i+1] == "true" || argv[i+1] == "false" {
					i++
					value = argv[i]
				}
			} else {
				i++
				value = argv[i]
			}
		}
		if key == "output" && (value == "human" || value == "json") {
			opts.Output = value
			opts.Raw["output"] = []string{value}
		}
		if key == "all" && value != "false" {
			opts.All = true
		}
	}
	opts.Raw["help"] = []string{"true"}
	opts.NoInput = opts.Output == "json"
	if len(words) == 0 {
		cmd, _ := r.ByName("help")
		return cmd, opts, true, nil
	}
	resolution := r.resolveWords(words)
	if resolution.Command != nil {
		opts.Positionals = resolution.Arguments
		return resolution.Command, opts, true, nil
	}
	if resolution.Group != nil {
		return groupCommand(resolution.Group), opts, true, nil
	}
	input := strings.Join(commandWords(words), " ")
	err := usageError("UNKNOWN_COMMAND", "Unknown command: "+input+".", "")
	err.Details = suggestionDetails(input, resolution.Suggestions)
	return nil, opts, true, err
}
