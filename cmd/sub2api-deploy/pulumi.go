package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/environment"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostresource"
)

var errInvalidPulumiPlan = errors.New("invalid Pulumi command")

type pulumiPlan struct {
	operation   string
	environment string
	userArgs    []string
	importTarget string
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
	afterSeparator, positionalCount := false, 0
	positionals := make([]string, 0, 3)
	safeArgs := make([]string, 0, len(plan.userArgs))
	for _, argument := range plan.userArgs {
		if afterSeparator {
			positionalCount++
			positionals = append(positionals, argument)
			continue
		}
		if argument == "--" {
			afterSeparator = true
			continue // Separators are not needed after managed import positionals are removed.
		}
		if strings.HasPrefix(argument, "--") {
			name, _, hasValue := strings.Cut(argument[2:], "=")
			if pulumiUnsafeLongOption(name) {
				return pulumiPlan{}, errInvalidPulumiPlan
			}
			if name == "file" {
				return pulumiPlan{}, errInvalidPulumiPlan
			}
			if (name == "message" || name == "parallel" || name == "target") && !hasValue {
				return pulumiPlan{}, errInvalidPulumiPlan
			}
			safeArgs = append(safeArgs, argument); continue
		}
		if strings.HasPrefix(argument, "-") {
			if !pulumiSafeShortOption(argument) {
				return pulumiPlan{}, errInvalidPulumiPlan
			}
			safeArgs = append(safeArgs, argument); continue
		}
		positionalCount++
		positionals = append(positionals, argument)
	}
	if plan.operation == "import" {
		if positionalCount != 3 || positionals[0] != hostresource.HostToken || !strings.HasPrefix(positionals[1], "host-") {
			return pulumiPlan{}, errInvalidPulumiPlan
		}
		target := strings.TrimPrefix(positionals[1], "host-")
		if !environment.ValidID(target) || (positionals[2] != target && positionals[2] != hostStableID(plan.environment, target)) { return pulumiPlan{}, errInvalidPulumiPlan }
		for _, argument := range safeArgs { if strings.HasPrefix(argument, "--target=") { return pulumiPlan{}, errInvalidPulumiPlan } }
		plan.operation, plan.importTarget, plan.userArgs = "up", target, safeArgs
	} else if positionalCount != 0 {
		return pulumiPlan{}, errInvalidPulumiPlan
	} else {
		plan.userArgs = safeArgs
	}
	return plan, nil
}

func hostStableID(environment, server string) string {
	payload := "sub2api-host-resource-id-v1:" + strconv.Itoa(len(environment)) + ":" + environment + strconv.Itoa(len(server)) + ":" + server
	sum := sha256.Sum256([]byte(payload))
	return "host-" + hex.EncodeToString(sum[:])
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
