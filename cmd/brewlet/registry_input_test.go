package main

import (
	"strings"
	"testing"
)

func TestRegistryCredentialsAreExplicitlyOriginScoped(t *testing.T) {
	t.Setenv("BREWLET_REGISTRY_USERNAME", "publisher")
	t.Setenv("BREWLET_REGISTRY_PASSWORD", "test-password")
	t.Setenv("BREWLET_REGISTRY_ORIGIN", "")
	if _, err := loadRegistries("", false, false); err == nil {
		t.Fatal("unscoped credentials accepted")
	}
	t.Setenv("BREWLET_REGISTRY_ORIGIN", "https://approved.example")
	options, err := loadRegistries("", false, false)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := options.registry("approved.example/apps/payload@sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := options.registry("unrelated.example/apps/payload@sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if approved.Username != "publisher" || foreign.Username != "" || foreign.Password != "" {
		t.Fatal("credentials crossed registry origin")
	}
}
