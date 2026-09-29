// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build !linux

package runnablestage

import (
	"context"
	"fmt"
	"io"
)

type noopLock struct{}

func (noopLock) Close() error { return nil }

// Acquire is a no-op outside Linux, where Reap is unsupported.
func Acquire(root string) (io.Closer, error) { return noopLock{}, nil }

func Reap(context.Context, string, Options) (Result, error) {
	return Result{}, fmt.Errorf("runnable stage reaping is supported only on Linux")
}
