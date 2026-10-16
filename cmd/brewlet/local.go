package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/microsoft/brewlet/pkg/artifact"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func localLayerSet(reference string) (artifact.Store, v1.Descriptor, bool, error) {
	directory, pin := reference, ""
	if i := strings.LastIndex(reference, "@sha256:"); i >= 0 {
		directory, pin = reference[:i], reference[i+1:]
	}
	store := artifact.Store{Dir: directory}
	info, e := os.Stat(filepath.Join(directory, "oci-layout"))
	if os.IsNotExist(e) {
		return store, v1.Descriptor{}, false, nil
	}
	if e != nil {
		return store, v1.Descriptor{}, true, e
	}
	if !info.Mode().IsRegular() {
		return store, v1.Descriptor{}, true, fmt.Errorf("oci-layout must be a regular file")
	}
	target, e := store.Target(pin)
	return store, target, true, e
}
