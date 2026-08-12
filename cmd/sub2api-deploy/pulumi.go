package main

import (
	"errors"
	"strings"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/environment"
)

var errInvalidPulumiPlan = errors.New("invalid Pulumi command")

type pulumiPlan struct {
	operation   string
	environment string
	userArgs    []string
}

func (p pulumiPlan) arguments(configPath string) []string {
	arguments := make([]string, 0, len(p.userArgs)+3)
	arguments = append(arguments, p.operation, "--stack="+p.environment, "--config-file="+configPath)
	return append(arguments, p.userArgs...)
}

func parsePulumiPlan(argv []string) (pulumiPlan, error) {
	if len(argv) < 3 || argv[0] != "pulumi" || !environment.ValidID(argv[1]) || !pulumiOperation(argv[2]) {
		return pulumiPlan{}, errInvalidPulumiPlan
	}
	for _, argument := range argv {
		if strings.ContainsRune(argument, '\x00') {
			return pulumiPlan{}, errInvalidPulumiPlan
		}
	}

	plan := pulumiPlan{operation: argv[2], environment: argv[1], userArgs: append([]string(nil), argv[3:]...)}
	afterSeparator, hasFileOption, positionalCount := false, false, 0
	for _, argument := range plan.userArgs {
		if afterSeparator {
			positionalCount++
			continue
		}
		if argument == "--" {
			afterSeparator = true
			continue
		}
		if strings.HasPrefix(argument, "--") {
			name, value, hasValue := strings.Cut(argument[2:], "=")
			if pulumiUnsafeLongOption(name) {
				return pulumiPlan{}, errInvalidPulumiPlan
			}
			if name == "file" {
				if plan.operation != "import" || !hasValue || value == "" || hasFileOption {
					return pulumiPlan{}, errInvalidPulumiPlan
				}
				hasFileOption = true
				continue
			}
			if (name == "message" || name == "parallel" || name == "target") && !hasValue {
				return pulumiPlan{}, errInvalidPulumiPlan
			}
			continue
		}
		if strings.HasPrefix(argument, "-") {
			if !pulumiSafeShortOption(argument) {
				return pulumiPlan{}, errInvalidPulumiPlan
			}
			continue
		}
		positionalCount++
	}
	if plan.operation == "import" {
		if (hasFileOption && positionalCount != 0) || (!hasFileOption && positionalCount != 3) {
			return pulumiPlan{}, errInvalidPulumiPlan
		}
	} else if positionalCount != 0 {
		return pulumiPlan{}, errInvalidPulumiPlan
	}
	return plan, nil
}

func pulumiOperation(value string) bool {
	switch value {
	case "preview", "up", "refresh", "destroy", "import":
		return true
	}
	return false
}

func pulumiUnsafeLongOption(name string) bool {
	switch name {
	case "stack", "config-file", "config", "config-path", "cwd", "secrets-provider", "show-secrets", "help", "remote":
		return true
	}
	return strings.HasPrefix(name, "remote-")
}

func pulumiSafeShortOption(argument string) bool {
	if len(argument) == 2 {
		return pulumiShortName(argument[1]) && argument[1] != 's' && argument[1] != 'c' && argument[1] != 'C' && argument[1] != 'h' && argument[1] != 'v'
	}
	return len(argument) > 3 && argument[2] == '=' && pulumiShortName(argument[1]) && argument[1] != 's' && argument[1] != 'c' && argument[1] != 'C' && argument[1] != 'h'
}

func pulumiShortName(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}
