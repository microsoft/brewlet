package v1alpha1

import (
	"os"
	"os/exec"
	"testing"
)

// requireHelm skips locally without helm but fails in CI, so chart contract
// checks (including chart/raw manifest parity) cannot silently disappear.
func requireHelm(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI for chart contract tests")
		}
		t.Skip("helm unavailable")
	}
}
