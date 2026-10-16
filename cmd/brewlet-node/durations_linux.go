//go:build linux

package main

import (
	"fmt"
	"os"
	"time"
)

func productionDuration(name string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s must be a positive real duration", name)
	}
	return duration, nil
}
