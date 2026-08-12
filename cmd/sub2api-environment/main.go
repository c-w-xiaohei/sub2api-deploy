package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/artifact"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/program"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func main() {
	pulumi.Run(run)
}

func run(ctx *pulumi.Context) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate environment program")
	}
	return register(ctx, executable)
}

func register(ctx *pulumi.Context, executablePath string) error {
	config, hasConfig := ctx.GetConfig("sub2api-environment:environmentConfig")
	secrets, hasSecrets := ctx.GetConfig("sub2api-environment:environmentSecrets")
	if !hasConfig || !hasSecrets || ctx.IsConfigSecret("sub2api-environment:environmentConfig") || !ctx.IsConfigSecret("sub2api-environment:environmentSecrets") {
		return fmt.Errorf("invalid environment configuration")
	}

	bundle, err := artifact.LoadBundle(filepath.Join(filepath.Dir(filepath.Dir(executablePath)), "artifacts", "sub2api-host"))
	if err != nil {
		return fmt.Errorf("invalid environment artifact bundle")
	}
	return program.Register(ctx, bundle.Manifest.Release, []byte(config), []byte(secrets))
}
