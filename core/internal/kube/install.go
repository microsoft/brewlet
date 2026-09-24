// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"flag"
	"fmt"
	"regexp"
	"time"
)

const chart = "oci://ghcr.io/microsoft/charts/brewlet"

type valuesFiles []string

func (v *valuesFiles) String() string { return fmt.Sprint([]string(*v)) }
func (v *valuesFiles) Set(value string) error {
	*v = append(*v, value)
	return nil
}

type installOptions struct {
	version, release string
	values           valuesFiles
	waitTimeout      time.Duration
	dryRun           bool
}

func (i *installOptions) flags(fs *flag.FlagSet) {
	fs.StringVar(&i.version, "version", "", "required exact Helm chart release version")
	fs.StringVar(&i.release, "release", "brewlet", "Helm release name")
	fs.Var(&i.values, "values", "complete Helm values file (repeatable)")
	fs.Var(&i.values, "f", "complete Helm values file (repeatable)")
	fs.DurationVar(&i.waitTimeout, "wait-timeout", 5*time.Minute, "Helm rollout timeout")
	fs.BoolVar(&i.dryRun, "dry-run", false, "render manifests locally instead of installing")
}

var chartVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

func (c *client) install(i installOptions) error {
	if !chartVersion.MatchString(i.version) {
		return fmt.Errorf("--version must be an exact chart version, such as 0.1.0 (not latest or a range)")
	}
	if err := validateName(i.release, 53); err != nil {
		return fmt.Errorf("--release: %w", err)
	}
	if i.waitTimeout <= 0 {
		return fmt.Errorf("--wait-timeout must be positive")
	}
	if len(i.values) == 0 {
		return fmt.Errorf("at least one --values/-f file is required: explicitly configure node pools and digest-pinned runtime sources, or disable defaultProfile")
	}
	for _, file := range i.values {
		if _, err := readDocument(file); err != nil {
			return err
		}
	}
	ns := c.opts.namespace
	if ns == "" {
		ns = "brewlet"
	}
	if err := validateToken(ns, 63); err != nil {
		return fmt.Errorf("--namespace: %w", err)
	}
	if !i.dryRun {
		existing, err := c.list("customresourcedefinitions", profilesResource, appsResource, "--ignore-not-found")
		if err != nil {
			return fmt.Errorf("check for existing Brewlet CRDs: %w", err)
		}
		if len(existing) != 0 {
			return fmt.Errorf("Brewlet CRDs already exist; install is fresh-install-only. Follow the documented CRD migration and Helm upgrade procedure")
		}
	}
	args := []string{"install", i.release, chart, "--create-namespace", "--wait", "--timeout", i.waitTimeout.String()}
	if i.dryRun {
		args = []string{"template", i.release, chart, "--include-crds"}
	}
	args = append(args, "--version", i.version, "--namespace", ns)
	if c.opts.kubeconfig != "" {
		args = append(args, "--kubeconfig", c.opts.kubeconfig)
	}
	if c.opts.context != "" {
		args = append(args, "--kube-context", c.opts.context)
	}
	for _, file := range i.values {
		args = append(args, "--values", file)
	}
	// The chart uses Values.namespace for resources, not Release.Namespace.
	args = append(args, "--set-string", "namespace="+ns)
	ctx, cancel := context.WithTimeout(c.ctx, i.waitTimeout+c.opts.timeout)
	defer cancel()
	result, err := c.exec(ctx, "helm", args, nil)
	if err != nil {
		return err
	}
	if _, err := c.out.Write(result); err != nil {
		return err
	}
	if !i.dryRun {
		fmt.Fprintf(c.err, "Helm release is ready. Node provisioning is separate; run brewlet k8s status --system-namespace %s with the same kubeconfig/context and inspect advertised JDK/launcher inventory.\n", ns)
	}
	return nil
}
