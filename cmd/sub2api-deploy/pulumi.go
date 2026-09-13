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
	operation    string
	environment  string
	importTarget string
	options      pulumiOptions
}

type pulumiOptions struct {
	message          string
	parallel         int
	targets          []string
	replaces         []string
	excludes         []string
	policyPacks      []string
	policyPackConfig []string
	plan             string
	color            string
	approve          bool
	diff             bool
	expectNoChanges  bool
	targetDependents  bool
	excludeDependents bool
	refresh           bool
	suppressProgress  bool
	suppressOutputs   bool
	continueOnError   bool
	debugLevel        *uint
	configFile        string
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

	plan := pulumiPlan{operation: argv[2], environment: argv[1]}
	afterSeparator, positionalCount := false, 0
	positionals := make([]string, 0, 3)
	for _, argument := range argv[3:] {
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
			if !hasValue && pulumiOptionRequiresValue(name) {
				return pulumiPlan{}, errInvalidPulumiPlan
			}
			if !pulumiSupportedLongOption(name, hasValue) {
				return pulumiPlan{}, errInvalidPulumiPlan
			}
			if hasValue && !appendPulumiOption(&plan.options, name, strings.TrimPrefix(argument, "--"+name+"="), argv[1]) {
				return pulumiPlan{}, errInvalidPulumiPlan
			}
			if !hasValue {
				appendPulumiOption(&plan.options, name, "", argv[1])
			}
			continue
		}
		if strings.HasPrefix(argument, "-") {
			if !pulumiSafeShortOption(argument) || !appendPulumiShortOption(&plan.options, argument) {
				return pulumiPlan{}, errInvalidPulumiPlan
			}
			continue
		}
		positionalCount++
		positionals = append(positionals, argument)
	}
	if plan.operation == "import" {
		if positionalCount != 3 || positionals[0] != hostresource.HostToken || !strings.HasPrefix(positionals[1], "host-") {
			return pulumiPlan{}, errInvalidPulumiPlan
		}
		target := strings.TrimPrefix(positionals[1], "host-")
		if !environment.ValidID(target) || (positionals[2] != target && positionals[2] != hostStableID(plan.environment, target)) {
			return pulumiPlan{}, errInvalidPulumiPlan
		}
		if len(plan.options.targets) != 0 {
			return pulumiPlan{}, errInvalidPulumiPlan
		}
		plan.operation, plan.importTarget = "up", target
	} else if positionalCount != 0 {
		return pulumiPlan{}, errInvalidPulumiPlan
	}
	if !validPulumiOptions(plan.operation, plan.options) {
		return pulumiPlan{}, errInvalidPulumiPlan
	}
	return plan, nil
}

func validPulumiOptions(operation string, options pulumiOptions) bool {
	if operation == "preview" || operation == "up" {
		return true
	}
	if operation == "refresh" {
		return len(options.replaces) == 0 && len(options.policyPacks) == 0 && len(options.policyPackConfig) == 0 && options.plan == "" && !options.refresh && !options.continueOnError
	}
	if operation == "destroy" {
		return len(options.replaces) == 0 && len(options.policyPacks) == 0 && len(options.policyPackConfig) == 0 && options.plan == "" && !options.expectNoChanges
	}
	return false
}

func pulumiOptionRequiresValue(name string) bool {
	switch name {
	case "message", "parallel", "target", "replace", "exclude", "policy-pack", "policy-pack-config", "plan", "color":
		return true
	default:
		return false
	}
}

func pulumiSupportedLongOption(name string, hasValue bool) bool {
	switch name {
	case "yes", "diff", "expect-no-changes", "target-dependents", "exclude-dependents", "refresh", "suppress-progress", "suppress-outputs", "continue-on-error":
		return !hasValue
	case "message", "parallel", "target", "replace", "exclude", "policy-pack", "policy-pack-config", "plan", "color":
		return hasValue
	case "non-interactive":
		return !hasValue
	default:
		return false
	}
}

func appendPulumiOption(options *pulumiOptions, name, value, environmentName string) bool {
	switch name {
	case "yes":
		options.approve = true
	case "diff":
		options.diff = true
	case "expect-no-changes":
		options.expectNoChanges = true
	case "target-dependents":
		options.targetDependents = true
	case "exclude-dependents":
		options.excludeDependents = true
	case "refresh":
		options.refresh = true
	case "suppress-progress":
		options.suppressProgress = true
	case "suppress-outputs":
		options.suppressOutputs = true
	case "continue-on-error":
		options.continueOnError = true
	case "non-interactive":
		return true
	case "message":
		options.message = value
	case "parallel":
		parallel, err := strconv.Atoi(value)
		if err != nil || parallel < 1 {
			return false
		}
		options.parallel = parallel
	case "target", "replace", "exclude":
		if containsControl(value) || value == "" {
			return false
		}
		if (name == "target" || name == "replace" || name == "exclude") && !documentedPulumiTarget(environmentName, value) {
			return false
		}
		switch name {
		case "target":
			options.targets = append(options.targets, value)
		case "replace":
			options.replaces = append(options.replaces, value)
		case "exclude":
			options.excludes = append(options.excludes, value)
		}
	case "policy-pack":
		if value == "" || containsControl(value) { return false }
		options.policyPacks = append(options.policyPacks, value)
	case "policy-pack-config":
		if value == "" || containsControl(value) { return false }
		options.policyPackConfig = append(options.policyPackConfig, value)
	case "plan":
		if value == "" || containsControl(value) { return false }
		options.plan = value
	case "color":
		switch value {
		case "always", "never", "raw", "auto":
			options.color = value
		default:
			return false
		}
	default:
		return false
	}
	return true
}

func appendPulumiShortOption(options *pulumiOptions, argument string) bool {
	switch {
	case argument == "-y":
		options.approve = true
		return true
	case strings.HasPrefix(argument, "-m="):
		options.message = strings.TrimPrefix(argument, "-m=")
		return options.message != "" && !containsControl(options.message)
	case strings.HasPrefix(argument, "-v="):
		level, err := strconv.ParseUint(strings.TrimPrefix(argument, "-v="), 10, 32)
		if err != nil {
			return false
		}
		levelValue := uint(level)
		options.debugLevel = &levelValue
		return true
	default:
		// Unsupported flags are rejected instead of bypassing the SDK lifecycle.
		return false
	}
}

// documentedPulumiTarget accepts safe current-stack URNs without owning Pulumi resource selection.
func documentedPulumiTarget(environmentName, target string) bool {
	parts := strings.Split(target, "::")
	if len(parts) != 4 || parts[0] != "urn:pulumi:"+environmentName || parts[1] != "sub2api-environment" {
		return false
	}
	for _, part := range parts[2:] {
		if part == "" || containsControl(part) {
			return false
		}
	}
	typeParts := strings.Split(parts[2], ":")
	if len(typeParts) != 3 {
		return false
	}
	for _, part := range typeParts {
		if part == "" || containsControl(part) {
			return false
		}
	}
	return true
}

func containsControl(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
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
