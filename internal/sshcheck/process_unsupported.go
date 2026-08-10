//go:build !linux

package sshcheck

import (
	"context"
	"fmt"
	"time"
)

const checkTimeout = 15 * time.Second

func systemRun(context.Context, string, []string) error {
	return fmt.Errorf("safe ssh check runner is unsupported on this platform")
}
