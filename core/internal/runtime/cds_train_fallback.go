// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package runtime

import (
	"context"
	"os/exec"
)

func trainingContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(parent)
}

func isolateTrainingGroup(*exec.Cmd) {}

func reapTrainingGroup(*exec.Cmd) {}
