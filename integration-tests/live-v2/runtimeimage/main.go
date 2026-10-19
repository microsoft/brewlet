// runtimeimage imports an unchanged approved upstream graph for isolated tests.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/microsoft/brewlet/kubernetes/policy"
	"github.com/microsoft/brewlet/pkg/artifact"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func run() error {
	source := flag.String("source", "", "canonical upstream manifest reference")
	output := flag.String("output", "", "new local OCI layout")
	flag.Parse()
	if err := policy.Source(*source); err != nil {
		return err
	}
	if *output == "" {
		return fmt.Errorf("new output layout required")
	}
	if _, err := os.Lstat(*output); !os.IsNotExist(err) {
		return fmt.Errorf("output layout already exists or cannot be inspected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	registry, err := artifact.NewRegistry(*source, false)
	if err != nil {
		return err
	}
	store := artifact.Store{Dir: *output}
	target, err := registry.Pull(ctx, store)
	if err != nil {
		return err
	}
	if target.MediaType != ocispec.MediaTypeImageManifest {
		return fmt.Errorf("explicit unchanged platform manifest required")
	}
	data, err := store.Read(target)
	if err != nil {
		return err
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return err
	}
	data, err = store.Read(manifest.Config)
	if err != nil {
		return err
	}
	var config ocispec.Image
	if err := json.Unmarshal(data, &config); err != nil {
		return err
	}
	if err := store.WriteIndex(target, *source); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"source": *source, "descriptor": target, "config": config})
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
