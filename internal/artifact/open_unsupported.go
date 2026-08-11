//go:build !linux

package artifact

import "fmt"

func readPinned(string, string, int64) ([]byte, error) {
	return nil, fmt.Errorf("safe artifact loading is unsupported on this platform")
}

func LoadBundle(string) (Bundle, error) {
	return Bundle{}, fmt.Errorf("safe artifact loading is unsupported on this platform")
}
