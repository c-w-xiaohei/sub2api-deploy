package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/environment"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/sshcheck"
)

func main() {
	if err := execute(os.Args[1:], os.Getwd, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func execute(args []string, getwd func() (string, error), stdout, stderr io.Writer) error {
	directory, err := getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}
	return run(args, directory, stdout, stderr)
}

func run(args []string, workdir string, stdout, stderr io.Writer) error {
	if len(args) != 2 || args[0] != "validate" {
		return fmt.Errorf("usage: sub2api-deploy validate <environment>")
	}
	paths, err := environment.ResolveEnvironment(filepath.Clean(workdir), args[1])
	if err != nil {
		return err
	}
	configData, err := os.ReadFile(paths.Config)
	if err != nil {
		return fmt.Errorf("read config.yaml: %w", err)
	}
	config, err := environment.ParseConfig(configData)
	if err != nil {
		return fmt.Errorf("config.yaml is invalid: %w", err)
	}
	secretData, err := decryptSecrets(paths.Secrets)
	if err != nil {
		return err
	}
	secrets, err := environment.ParseSecrets(secretData)
	if err != nil {
		return fmt.Errorf("secrets.yaml is invalid")
	}
	validated, err := environment.Validate(config, secrets)
	if err != nil {
		return err
	}
	if err := sshcheck.CheckAliases(validated.SSHAliases); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "environment: %s\nservers: %d\napps: %d\n", args[1], len(validated.Servers), len(validated.Apps))
	return nil
}

func decryptSecrets(path string) ([]byte, error) {
	command := exec.Command("sops", "--decrypt", path)
	command.Stderr = io.Discard
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("sops --decrypt failed")
	}
	return output, nil
}
