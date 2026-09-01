//go:build !race

package providerruntime

import "time"

const providerRaceEnabled = false

const (
	providerBuildTimeout       = 30 * time.Second
	prerequisiteCreateTimeout  = 10 * time.Second
	matrixCreateUpdateTimeout  = 30 * time.Second
	configureReadDeleteTimeout = 10 * time.Second
	providerPortReadTimeout    = 5 * time.Second
)
