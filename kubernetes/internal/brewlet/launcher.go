// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package brewlet

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// MaxLauncherNameLength bounds a custom launcher name: the DNS-1123 label budget
// left after the LabelLauncherPrefix ("launcher.") this package stamps onto
// nodes, so every accepted name can also become a capability label.
const MaxLauncherNameLength = validation.DNS1123LabelMaxLength - len("launcher.")

// ValidateLauncherName enforces that a requested launcher is the vanilla "java"
// launcher or a safe DNS-1123 token. The name is tenant-controlled input
// (AnnotationRequestedLauncher, or spec.jvm.launcher via the operator) that the
// node shim joins onto its launcher roots to pick an overlay lower layer and a
// privileged bind-mount source, so a value carrying a path separator or a parent
// reference is a host-path-traversal primitive (CWE-22), not a malformed hint.
// Admission rejects it here so it never reaches a node — and never becomes a
// malformed brewlet.sh/launcher.<name> affinity key — while the shim
// (core/internal/artifact.ValidateLauncherName) applies the identical rule
// independently, so the guarantee does not depend on admission running.
func ValidateLauncherName(name string) error {
	if name == "" || name == VanillaLauncher {
		return nil
	}
	if len(name) > MaxLauncherNameLength {
		return fmt.Errorf("launcher %q exceeds %d characters", name, MaxLauncherNameLength)
	}
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		return fmt.Errorf("launcher %q is invalid: %s", name, strings.Join(errs, "; "))
	}
	return nil
}
