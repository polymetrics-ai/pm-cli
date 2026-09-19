package cli

import (
	"fmt"
	"strings"
)

type Selectors struct {
	ServerProfile string
	Workspace     string
	Connection    string
}

type boundedArgumentScan struct {
	positionals []string
	helpEnabled bool
	helpInvalid bool
}

// scanBoundedArguments recognizes only PM controls and their fixed arity.
// Every other long flag is a generated flag and consumes exactly one following
// token when it has no inline value. The consumed token is never interpreted as
// a selector, help flag, or terminator; Cobra still final-parses the untouched
// original argument list after the generated tree is available.
func scanBoundedArguments(
	arguments []string,
	visitControl func(name, kind, value string) error,
) (boundedArgumentScan, error) {
	var result boundedArgumentScan
	for index := 0; index < len(arguments); index++ {
		token := arguments[index]
		if token == "--" {
			break
		}
		if present, enabled, valid := explicitHelpFlag(token); present {
			result.helpEnabled = enabled
			result.helpInvalid = result.helpInvalid || !valid
			continue
		}
		name, inline, kind, recognized := rootControlToken(token)
		if recognized {
			value := "true"
			if inline != nil {
				value = *inline
			} else if kind == "string" {
				if index+1 >= len(arguments) || arguments[index+1] == "" || strings.HasPrefix(arguments[index+1], "--") {
					return boundedArgumentScan{}, fmt.Errorf("%w: --%s requires one bounded non-empty value", errUsage, name)
				}
				index++
				value = arguments[index]
			}
			if visitControl != nil {
				if err := visitControl(name, kind, value); err != nil {
					return boundedArgumentScan{}, err
				}
			}
			continue
		}
		if strings.HasPrefix(token, "--") {
			if !strings.Contains(token, "=") && index+1 < len(arguments) {
				index++
			}
			continue
		}
		if strings.HasPrefix(token, "-") {
			continue
		}
		result.positionals = append(result.positionals, token)
	}
	return result, nil
}

// ParseSelectors rejects ambiguous selector spelling before Cobra dispatch.
// It recognizes only unconsumed static selector names and never parses a
// generated provider value.
func ParseSelectors(arguments []string) (Selectors, error) {
	var selectors Selectors
	seen := make(map[string]struct{}, 3)
	_, err := scanBoundedArguments(arguments, func(name, _ string, value string) error {
		switch name {
		case "server-profile", "workspace", "connection":
		default:
			return nil
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("%w: --%s may be specified only once", errUsage, name)
		}
		if value == "" || len(value) > maximumInputLength {
			return fmt.Errorf("%w: --%s requires one bounded non-empty value", errUsage, name)
		}
		seen[name] = struct{}{}
		switch name {
		case "server-profile":
			selectors.ServerProfile = value
		case "workspace":
			selectors.Workspace = value
		case "connection":
			selectors.Connection = value
		}
		return nil
	})
	if err != nil {
		return Selectors{}, err
	}
	return selectors, nil
}

func preparseSelectors(arguments []string) error {
	_, err := ParseSelectors(arguments)
	return err
}
