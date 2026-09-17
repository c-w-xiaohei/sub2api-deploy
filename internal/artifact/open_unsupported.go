//go:build !linux

package artifact

import "fmt"

func LoadBundle(string) (Bundle, error) {
	return Bundle{}, fmt.Errorf("safe artifact loading is unsupported on this platform")
}
