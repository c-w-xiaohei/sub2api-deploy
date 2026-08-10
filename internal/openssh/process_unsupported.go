//go:build !linux

package openssh

import (
	"context"
	"errors"
)

func systemStart(context.Context, string, []string, []byte) processResult {
	return processResult{err: errors.New("openssh process-group cleanup is unsupported on this platform")}
}
