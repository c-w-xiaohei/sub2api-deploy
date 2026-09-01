//go:build race

package providerruntime

import "time"

const providerRaceEnabled = true

const (
	providerBuildTimeout       = 60 * time.Second
	prerequisiteCreateTimeout  = 30 * time.Second
	matrixCreateUpdateTimeout  = 60 * time.Second
	configureReadDeleteTimeout = 30 * time.Second
	providerPortReadTimeout    = 15 * time.Second
)
