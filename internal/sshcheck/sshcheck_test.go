package sshcheck

import (
	"reflect"
	"strings"
	"testing"
)

func TestCheckAliasesUsesSortedSafeSSHArgvAndStopsOnFailure(t *testing.T) {
	var calls [][]string
	err := checkAliases([]string{"zeta", "Alpha_1", "node.local"}, func(name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		if len(calls) == 4 {
			return errSentinel
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "node.local") || !strings.Contains(err.Error(), "connect") {
		t.Fatalf("error = %v", err)
	}
	expected := [][]string{
		{"ssh", "-G", "--", "Alpha_1"},
		append([]string{"ssh"}, ConnectCheckArgs("Alpha_1")...),
		{"ssh", "-G", "--", "node.local"},
		append([]string{"ssh"}, ConnectCheckArgs("node.local")...),
	}
	if !reflect.DeepEqual(calls, expected) {
		t.Fatalf("calls = %#v, want %#v", calls, expected)
	}
}

func TestConnectCheckArgsAreTheFixedTransportSafetyContract(t *testing.T) {
	got := ConnectCheckArgs("edge")
	if !reflect.DeepEqual(got, []string{"-T", "-a", "-x", "-o", "BatchMode=yes", "-o", "NumberOfPasswordPrompts=0", "-o", "RequestTTY=no", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ForwardX11Trusted=no", "-o", "ClearAllForwardings=yes", "-o", "Tunnel=no", "-o", "ExitOnForwardFailure=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no", "-o", "PermitLocalCommand=no", "-o", "ForkAfterAuthentication=no", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "RemoteCommand=none", "-o", "SessionType=default", "-o", "StdinNull=no", "-o", "ConnectTimeout=10", "--", "edge", "true"}) {
		t.Fatalf("args = %#v", got)
	}
}

func TestCheckAliasesDoesNotConnectAfterExpansionFailure(t *testing.T) {
	calls := 0
	err := checkAliases([]string{"bad"}, func(name string, args ...string) error {
		calls++
		return errSentinel
	})
	if err == nil || !strings.Contains(err.Error(), "bad") || !strings.Contains(err.Error(), "expand") {
		t.Fatalf("error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("runner calls = %d, want 1", calls)
	}
}

func TestValidateAliasAcceptsOnlySafeOpenSSHTokens(t *testing.T) {
	for _, alias := range []string{"host", "Alpha_1", "node.local", "a-b"} {
		t.Run("accept "+alias, func(t *testing.T) {
			if err := ValidateAlias(alias); err != nil {
				t.Fatalf("ValidateAlias(%q) error = %v", alias, err)
			}
		})
	}
	for _, alias := range []string{"", "-node", " node", "node ", "node\tname", "node\x00name", "node\x7fname", "user@host", "host:22", "ssh://host", "host,other", "host other", "node;true", "cafe" + string(rune(0x00e9))} {
		t.Run("reject", func(t *testing.T) {
			if err := ValidateAlias(alias); err == nil {
				t.Fatalf("ValidateAlias accepted %q", alias)
			}
		})
	}
}

var errSentinel = &testError{}

type testError struct{}

func (*testError) Error() string { return "fake ssh failure" }
