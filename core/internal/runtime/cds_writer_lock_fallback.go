// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package runtime

import "sync"

var cdsWriterStateMu sync.Mutex

func acquireCDSWriterStateLock(string) (func(), error) {
	cdsWriterStateMu.Lock()
	return cdsWriterStateMu.Unlock, nil
}
