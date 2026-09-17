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

// ParseSelectors rejects ambiguous selector spelling before Cobra dispatch.
// It intentionally recognizes only static selector names and never interprets
// positional command data or tokens after --.
func ParseSelectors(arguments []string) (Selectors, error) {
	var selectors Selectors
	seen := make(map[string]struct{}, 3)
	for index := 0; index < len(arguments); index++ {
		token := arguments[index]
		if token == "--" {
			return selectors, nil
		}
		name, inline, recognized := selectorToken(token)
		if !recognized {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			return Selectors{}, fmt.Errorf("%w: --%s may be specified only once", errUsage, name)
		}
		seen[name] = struct{}{}
		var value string
		if inline != nil {
			value = *inline
		} else {
			if index+1 >= len(arguments) || arguments[index+1] == "" || strings.HasPrefix(arguments[index+1], "--") {
				return Selectors{}, fmt.Errorf("%w: --%s requires one bounded non-empty value", errUsage, name)
			}
			index++
			value = arguments[index]
		}
		if value == "" || len(value) > maximumInputLength {
			return Selectors{}, fmt.Errorf("%w: --%s requires one bounded non-empty value", errUsage, name)
		}
		switch name {
		case "server-profile":
			selectors.ServerProfile = value
		case "workspace":
			selectors.Workspace = value
		case "connection":
			selectors.Connection = value
		}
	}
	return selectors, nil
}

func preparseSelectors(arguments []string) error {
	_, err := ParseSelectors(arguments)
	return err
}

func selectorToken(token string) (string, *string, bool) {
	for _, name := range []string{"server-profile", "workspace", "connection"} {
		plain := "--" + name
		if token == plain {
			return name, nil, true
		}
		if strings.HasPrefix(token, plain+"=") {
			value := strings.TrimPrefix(token, plain+"=")
			return name, &value, true
		}
	}
	return "", nil, false
}
