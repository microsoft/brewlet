// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// The shipped CRD schema alone, with no admission webhook installed, must
// refuse mutable or unqualified JDK and launcher source references.
func TestSourceImageCRDValidationWithoutAdmission(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)

	valid := profileNamed(uniqueName("source-schema"), []string{"schema"}, jdk("zulu", 21))
	valid.Spec.Launchers = []nodev1alpha1.LauncherRef{launcher("jaz")}
	if err := c.Create(ctx, &valid); err != nil {
		t.Fatalf("schema rejected digest-pinned sources: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), &valid) })

	for _, image := range []string{
		"docker.io/library/azul-zulu:21",
		"azul-zulu@" + testImageDigest,
		"docker.io/library/azul-zulu:21@" + testImageDigest,
	} {
		t.Run("jdk "+image, func(t *testing.T) {
			p := profileNamed(uniqueName("tagged-jdk"), []string{"schema"}, jdk("zulu", 21))
			p.Spec.JDKs[0].Source.Image = image
			if err := c.Create(ctx, &p); !apierrors.IsInvalid(err) {
				_ = c.Delete(context.Background(), &p)
				t.Fatalf("schema accepted JDK source %q: %v", image, err)
			}
		})
		t.Run("launcher "+image, func(t *testing.T) {
			p := profileNamed(uniqueName("tagged-launcher"), []string{"schema"}, jdk("zulu", 21))
			l := launcher("jaz")
			l.Source.Image = image
			p.Spec.Launchers = []nodev1alpha1.LauncherRef{l}
			if err := c.Create(ctx, &p); !apierrors.IsInvalid(err) {
				_ = c.Delete(context.Background(), &p)
				t.Fatalf("schema accepted launcher source %q: %v", image, err)
			}
		})
	}

	updated := getProfile(t, ctx, c, valid.Name)
	updated.Spec.JDKs[0].Source.Image = "docker.io/library/azul-zulu:21"
	if err := c.Update(ctx, &updated); !apierrors.IsInvalid(err) {
		t.Fatalf("schema accepted a mutable tag on update: %v", err)
	}
}
