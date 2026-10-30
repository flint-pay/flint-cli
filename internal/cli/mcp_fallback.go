package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// cliCommandFor uses the prepared request so approval preserves resolved times,
// body defaults and the exact optional or caller-supplied idempotency key.
func (a *App) cliCommandFor(cmd *Command, req preparedRequest, resolved ResolvedConfig) (string, error) {
	if cmd == nil || len(cmd.Path) < 2 {
		return "", fmt.Errorf("the operation has no terminal command")
	}
	tokens := append([]string{"flint"}, cmd.Path[1:]...)
	flag := func(name, value string) { tokens = append(tokens, name+"="+value) }
	positionals := []string{}
	if cmd.CanonicalName == "history.clear" {
		tokens = append(tokens, "--clear")
	} else {
		parsed, err := url.Parse(req.Path)
		if err != nil {
			return "", err
		}
		if req.Method != cmd.Method || parsed.IsAbs() || parsed.Host != "" || parsed.Fragment != "" {
			return "", fmt.Errorf("the prepared request does not match the operation")
		}
		template, actual := strings.Split(cmd.APIPath, "/"), strings.Split(parsed.EscapedPath(), "/")
		if len(template) != len(actual) {
			return "", fmt.Errorf("the prepared path does not match the operation")
		}
		pathValues := map[string]string{}
		for i, part := range template {
			if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
				value, err := url.PathUnescape(actual[i])
				if err != nil {
					return "", err
				}
				pathValues[strings.Trim(part, "{}")] = value
			} else if part != actual[i] {
				return "", fmt.Errorf("the prepared path does not match the operation")
			}
		}
		query, err := url.ParseQuery(parsed.RawQuery)
		if err != nil {
			return "", err
		}
		queryNames := make([]string, 0, len(query))
		for name := range query {
			queryNames = append(queryNames, name)
		}
		sort.Strings(queryNames)
		for _, name := range queryNames {
			queryFlag := ""
			for _, argument := range cmd.Arguments {
				if argument.Query == name && argument.Flag != "" {
					queryFlag = argument.Flag
					break
				}
			}
			// These query values also come from global controls and need not
			// have a command-specific argument in the registry.
			if queryFlag == "" {
				switch name {
				case "expand", "page_size", "page_token":
					queryFlag = "--" + strings.ReplaceAll(name, "_", "-")
				default:
					return "", fmt.Errorf("the operation has no terminal flag for query parameter %q", name)
				}
			}
			for _, value := range query[name] {
				flag(queryFlag, value)
			}
		}
		if len(req.Body) > 0 {
			body := req.Body
			operation, _ := openAPIOperationByID(cmd.OperationID)
			if mediaType, _ := requestContentSchema(operation.RequestBody); mediaType == "application/x-www-form-urlencoded" {
				// --input reads JSON even when prepareRequest later form-encodes it.
				if req.BodyValue == nil {
					return "", fmt.Errorf("the prepared form has no input object")
				}
				body, err = json.Marshal(req.BodyValue)
				if err != nil {
					return "", err
				}
			}
			flag("--input", string(body))
		}
		if req.IdempotencyKey != "" {
			flag("--idempotency-key", req.IdempotencyKey)
		}
		tokens = append(tokens, "--live")
		for _, argument := range cmd.Arguments {
			if argument.Positional <= 0 {
				continue
			}
			value, ok := pathValues[argument.Name]
			if !ok {
				continue
			}
			for len(positionals) < argument.Positional {
				positionals = append(positionals, "")
			}
			positionals[argument.Positional-1] = value
			delete(pathValues, argument.Name)
		}
		if len(pathValues) > 0 {
			return "", fmt.Errorf("the operation has no terminal path arguments")
		}
	}
	switch resolved.Sources["profile"] {
	case "flag", "environment", "project":
		flag("--profile", resolved.ProfileName)
	}
	if resolved.ContextID != "" {
		flag("--context", resolved.ContextID)
	}
	if resolved.MerchantGuard != "" {
		flag("--merchant", resolved.MerchantGuard)
	}
	// Sandbox guards are project-only. The action instructs the user to run in
	// the same project directory rather than suggesting an unsupported flag.
	if len(positionals) > 0 {
		tokens = append(tokens, "--")
		tokens = append(tokens, positionals...)
	}
	for i, token := range tokens {
		if strings.ContainsRune(token, '\x00') {
			return "", fmt.Errorf("the prepared request contains a value that cannot be passed to a terminal")
		}
		tokens[i] = quoteRemediationArgument(token)
	}
	return strings.Join(tokens, " "), nil
}
