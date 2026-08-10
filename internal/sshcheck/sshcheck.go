package sshcheck

import (
	"context"
	"fmt"
	"regexp"
	"sort"
)

var safeAlias = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

type runner func(name string, args ...string) error

func CheckAliases(aliases []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()
	return checkAliases(aliases, func(name string, args ...string) error { return systemRun(ctx, name, args) })
}

func ValidateAlias(alias string) error {
	if !safeAlias.MatchString(alias) {
		return fmt.Errorf("alias %q is not a safe OpenSSH name", alias)
	}
	return nil
}

// ConfigArgs only expands the configured alias; it never connects.
func ConfigArgs(alias string) []string { return []string{"-G", "--", alias} }

// ConnectCheckArgs has a fixed harmless remote command. Callers cannot supply one.
func ConnectCheckArgs(alias string) []string { return append(safetyArgs(), "--", alias, "true") }

func safetyArgs() []string {
	return []string{"-T", "-a", "-x", "-o", "BatchMode=yes", "-o", "NumberOfPasswordPrompts=0", "-o", "RequestTTY=no", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ForwardX11Trusted=no", "-o", "ClearAllForwardings=yes", "-o", "Tunnel=no", "-o", "ExitOnForwardFailure=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no", "-o", "PermitLocalCommand=no", "-o", "ForkAfterAuthentication=no", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "RemoteCommand=none", "-o", "SessionType=default", "-o", "StdinNull=no", "-o", "ConnectTimeout=10"}
}

func checkAliases(aliases []string, run runner) error {
	ordered := append([]string(nil), aliases...)
	sort.Strings(ordered)
	for _, alias := range ordered {
		if err := ValidateAlias(alias); err != nil {
			return err
		}
		if err := run("ssh", ConfigArgs(alias)...); err != nil {
			return fmt.Errorf("ssh alias %q expand failed", alias)
		}
		if err := run("ssh", ConnectCheckArgs(alias)...); err != nil {
			return fmt.Errorf("ssh alias %q connect failed", alias)
		}
	}
	return nil
}
