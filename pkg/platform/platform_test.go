package platform

import (
	"reflect"
	"testing"

	"github.com/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestExactPlatformsNormalizeOnlyEquivalentAliases(t *testing.T) {
	for _, test := range []struct {
		name, arch, left, right string
		equal                   bool
	}{
		{"arm v7 exact", "arm", "v7", "v7", true},
		{"arm v7 not v6", "arm", "v7", "v6", false},
		{"arm default v7", "arm", "", "v7", true},
		{"arm64 default v8", "arm64", "", "v8", true},
		{"arm64 default not v9", "arm64", "", "v9", false},
		{"amd64 default v1", "amd64", "", "v1", true},
		{"amd64 default not v2", "amd64", "", "v2", false},
		{"amd64 v3 not v2", "amd64", "v3", "v2", false},
		{"amd64 v3 exact", "amd64", "v3", "v3", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := ocispec.Platform{OS: "linux", Architecture: test.arch, Variant: test.left}
			b := ocispec.Platform{OS: "linux", Architecture: test.arch, Variant: test.right}
			if Exact(a, b) != test.equal {
				t.Fatal(a, b)
			}
		})
	}
	if !Exact(ocispec.Platform{OS: "linux", Architecture: "aarch64", Variant: "v8.0"}, ocispec.Platform{OS: "linux", Architecture: "arm64"}) {
		t.Fatal("safe architecture/default aliases rejected")
	}
	if Exact(ocispec.Platform{OS: "linux", Architecture: "arm64", OSVersion: "different"}, ocispec.Platform{OS: "linux", Architecture: "arm64"}) {
		t.Fatal("OS version constraint ignored")
	}
	if Exact(ocispec.Platform{OS: "linux", Architecture: "arm64", OSFeatures: []string{"required"}}, ocispec.Platform{OS: "linux", Architecture: "arm64"}) {
		t.Fatal("declared OS feature constraint ignored")
	}
	if Exact(ocispec.Platform{}, ocispec.Platform{}) {
		t.Fatal("missing declaration guessed")
	}
}

func TestNativePlatformRetainsContainerdDetectedCPUVariant(t *testing.T) {
	expected := platforms.Normalize(platforms.DefaultSpec())
	if actual := Native(); !reflect.DeepEqual(actual, expected) {
		t.Fatal(actual, expected)
	}
}
