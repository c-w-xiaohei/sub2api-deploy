package main

import (
	"reflect"
	"strings"
	"testing"
)

const pulumiPlanCanary = "PULUMI_PLAN_SECRET_CANARY"

func TestParsePulumiPlanBuildsOwnedArguments(t *testing.T) {
	configPath := "/workspace/environments/production/Pulumi.production.yaml"
	for _, test := range []struct {
		name string
		argv []string
		want []string
	}{
		{"preview", []string{"pulumi", "production", "preview"}, []string{"preview", "--stack=production", "--config-file=" + configPath}},
		{"up", []string{"pulumi", "production", "up", "--yes", "--message=release candidate"}, []string{"up", "--stack=production", "--config-file=" + configPath, "--yes", "--message=release candidate"}},
		{"refresh", []string{"pulumi", "staging-2", "refresh", "-y"}, []string{"refresh", "--stack=staging-2", "--config-file=" + configPath, "-y"}},
		{"destroy", []string{"pulumi", "production", "destroy", "--yes"}, []string{"destroy", "--stack=production", "--config-file=" + configPath, "--yes"}},
		{"import", []string{"pulumi", "production", "import", "aws:s3/bucket:Bucket", "logs", "bucket-id"}, []string{"import", "--stack=production", "--config-file=" + configPath, "aws:s3/bucket:Bucket", "logs", "bucket-id"}},
		{"import separator between type and name", []string{"pulumi", "production", "import", "pkg:index:Thing", "--", "thing", "id"}, []string{"import", "--stack=production", "--config-file=" + configPath, "pkg:index:Thing", "--", "thing", "id"}},
		{"import separator before dash ID", []string{"pulumi", "production", "import", "pkg:index:Thing", "thing", "--", "-id"}, []string{"import", "--stack=production", "--config-file=" + configPath, "pkg:index:Thing", "thing", "--", "-id"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, err := parsePulumiPlan(test.argv)
			if err != nil {
				t.Fatalf("parsePulumiPlan(%q) error = %v", test.argv, err)
			}
			if got := plan.arguments(configPath); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("arguments() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestParsePulumiPlanRejectsSyntaxEnvironmentOperationAndNUL(t *testing.T) {
	for _, test := range []struct {
		name string
		argv []string
	}{
		{"missing pulumi", []string{"production", "preview"}},
		{"missing environment", []string{"pulumi"}},
		{"missing operation", []string{"pulumi", "production"}},
		{"uppercase environment", []string{"pulumi", "Production", "preview"}},
		{"underscore environment", []string{"pulumi", "prod_env", "preview"}},
		{"leading hyphen environment", []string{"pulumi", "-production", "preview"}},
		{"unsupported operation", []string{"pulumi", "production", "stack"}},
		{"uppercase operation", []string{"pulumi", "production", "Preview"}},
		{"NUL in environment", []string{"pulumi", "prod\x00uction", "preview"}},
		{"NUL in option", []string{"pulumi", "production", "up", "--message=note\x00"}},
	} {
		t.Run(test.name, func(t *testing.T) { assertPulumiPlanRejected(t, test.argv) })
	}
}

func TestParsePulumiPlanRejectsOwnedAndUnsafeLongOptions(t *testing.T) {
	for _, test := range []struct {
		name string
		argv []string
	}{
		{"stack bare", []string{"pulumi", "production", "up", "--stack"}},
		{"stack equals", []string{"pulumi", "production", "up", "--stack=other"}},
		{"stack split", []string{"pulumi", "production", "up", "--stack", "other"}},
		{"config file bare", []string{"pulumi", "production", "up", "--config-file"}},
		{"config file equals", []string{"pulumi", "production", "up", "--config-file=other.yaml"}},
		{"config file split", []string{"pulumi", "production", "up", "--config-file", "other.yaml"}},
		{"config bare", []string{"pulumi", "production", "up", "--config"}},
		{"config equals", []string{"pulumi", "production", "up", "--config=key=value"}},
		{"config split", []string{"pulumi", "production", "up", "--config", "key=value"}},
		{"config path bare", []string{"pulumi", "production", "up", "--config-path"}},
		{"config path equals", []string{"pulumi", "production", "up", "--config-path=path"}},
		{"config path split", []string{"pulumi", "production", "up", "--config-path", "path"}},
		{"cwd bare", []string{"pulumi", "production", "up", "--cwd"}},
		{"cwd equals", []string{"pulumi", "production", "up", "--cwd=dir"}},
		{"cwd split", []string{"pulumi", "production", "up", "--cwd", "dir"}},
		{"secrets provider bare", []string{"pulumi", "production", "up", "--secrets-provider"}},
		{"secrets provider equals", []string{"pulumi", "production", "up", "--secrets-provider=passphrase"}},
		{"secrets provider split", []string{"pulumi", "production", "up", "--secrets-provider", "passphrase"}},
		{"show secrets bare", []string{"pulumi", "production", "up", "--show-secrets"}},
		{"show secrets equals", []string{"pulumi", "production", "up", "--show-secrets=true"}},
		{"help bare", []string{"pulumi", "production", "up", "--help"}},
		{"help equals", []string{"pulumi", "production", "up", "--help=true"}},
		{"remote bare", []string{"pulumi", "production", "up", "--remote"}},
		{"remote equals", []string{"pulumi", "production", "up", "--remote=true"}},
		{"remote split", []string{"pulumi", "production", "up", "--remote", "true"}},
		{"remote git bare", []string{"pulumi", "production", "up", "--remote-git"}},
		{"remote git equals", []string{"pulumi", "production", "up", "--remote-git=https://example.test/repo"}},
		{"remote git split", []string{"pulumi", "production", "up", "--remote-git", "https://example.test/repo"}},
		{"remote pre run equals", []string{"pulumi", "production", "up", "--remote-pre-run-command=command"}},
		{"remote inherit equals", []string{"pulumi", "production", "up", "--remote-inherit-settings=true"}},
		{"remote skip equals", []string{"pulumi", "production", "up", "--remote-skip-install-dependencies=true"}},
		{"remote canary", []string{"pulumi", "production", "up", "--remote-env-secret=" + pulumiPlanCanary}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertPulumiPlanRejected(t, test.argv)
		})
	}
}

func TestParsePulumiPlanHandlesShortOptions(t *testing.T) {
	for _, argv := range [][]string{
		{"pulumi", "production", "up", "-s"},
		{"pulumi", "production", "up", "-s=other"},
		{"pulumi", "production", "up", "-s", "other"},
		{"pulumi", "production", "up", "-c"},
		{"pulumi", "production", "up", "-c=key=value"},
		{"pulumi", "production", "up", "-c", "key=value"},
		{"pulumi", "production", "up", "-C"},
		{"pulumi", "production", "up", "-C=dir"},
		{"pulumi", "production", "up", "-C", "dir"},
		{"pulumi", "production", "up", "-h"},
		{"pulumi", "production", "up", "-h=true"},
		{"pulumi", "production", "up", "-sproduction"},
		{"pulumi", "production", "up", "-ys"},
		{"pulumi", "production", "up", "-ysproduction"},
		{"pulumi", "production", "up", "-yv"},
		{"pulumi", "production", "up", "-mnote"},
		{"pulumi", "production", "up", "-v"},
	} {
		assertPulumiPlanRejected(t, argv)
	}

	for _, argv := range [][]string{
		{"pulumi", "production", "up", "-y"},
		{"pulumi", "production", "up", "-v=3"},
		{"pulumi", "production", "up", "-m=release note"},
	} {
		if _, err := parsePulumiPlan(argv); err != nil {
			t.Fatalf("parsePulumiPlan(%q) error = %v", argv, err)
		}
	}
}

func TestParsePulumiPlanRejectsPositionalsOutsideSupportedImport(t *testing.T) {
	for _, argv := range [][]string{
		{"pulumi", "production", "preview", "template"},
		{"pulumi", "production", "up", "template"},
		{"pulumi", "production", "up", "./program"},
		{"pulumi", "production", "up", "https://example.test/program"},
		{"pulumi", "production", "refresh", "state"},
		{"pulumi", "production", "destroy", "target"},
		{"pulumi", "production", "up", "--", "template"},
		{"pulumi", "production", "preview", "--", "--stack=other"},
		{"pulumi", "production", "up", pulumiPlanCanary},
		{"pulumi", "production", "up", "--message", "note"},
		{"pulumi", "production", "up", "--parallel", "4"},
		{"pulumi", "production", "up", "--target", "urn:pulumi:production::project::type::name"},
	} {
		assertPulumiPlanRejected(t, argv)
	}
}

func TestParsePulumiPlanAcceptsAndRejectsImportShapes(t *testing.T) {
	for _, argv := range [][]string{
		{"pulumi", "production", "import", "pkg:index:Thing", "thing", "id"},
		{"pulumi", "production", "import", "--file=imports.json", "--protect=false"},
		{"pulumi", "production", "import", "pkg:index:Thing", "--", "thing", "id"},
		{"pulumi", "production", "import", "pkg:index:Thing", "thing", "--", "-id"},
		{"pulumi", "production", "import", "--", "pkg:index:Thing", "thing", "-id"},
	} {
		if _, err := parsePulumiPlan(argv); err != nil {
			t.Fatalf("parsePulumiPlan(%q) error = %v", argv, err)
		}
	}

	for _, argv := range [][]string{
		{"pulumi", "production", "import"},
		{"pulumi", "production", "import", "type", "name"},
		{"pulumi", "production", "import", "type", "name", "id", "extra"},
		{"pulumi", "production", "import", "--file="},
		{"pulumi", "production", "import", "--file", "imports.json"},
		{"pulumi", "production", "import", "--file=imports.json", "type", "name", "id"},
		{"pulumi", "production", "import", "--file=imports.json", "--", "extra"},
		{"pulumi", "production", "import", "--", "--file=imports.json"},
		{"pulumi", "production", "import", "--message", "note", "pkg:index:Thing", "thing"},
		{"pulumi", "production", "import", "--parallel", "4", "pkg:index:Thing", "thing"},
		{"pulumi", "production", "import", "--target", "urn:pulumi:production::project::type::name", "pkg:index:Thing", "thing"},
		{"pulumi", "production", "up", "--file=imports.json"},
		{"pulumi", "production", "preview", "--file=imports.json"},
	} {
		assertPulumiPlanRejected(t, argv)
	}
}

func assertPulumiPlanRejected(t *testing.T, argv []string) {
	t.Helper()
	_, err := parsePulumiPlan(argv)
	if err == nil {
		t.Fatalf("parsePulumiPlan(%q) unexpectedly succeeded", argv)
	}
	if strings.Contains(err.Error(), pulumiPlanCanary) {
		t.Fatalf("parsePulumiPlan(%q) exposed sensitive argument in error: %v", argv, err)
	}
}
