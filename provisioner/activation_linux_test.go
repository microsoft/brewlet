//go:build linux

package provisioner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigureContainerdVersions(t *testing.T) {
	for _, version := range []string{"2", "3"} {
		config := []byte("version = " + version + "\n")
		desired, err := configuredRuntime(config)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(desired), "io.containerd.brewlet.v2") || !strings.Contains(string(desired), "brewlet.sh/rotated-to-generation") {
			t.Fatal(string(desired))
		}
		if _, err := configuredRuntime(desired); err == nil {
			t.Fatal("unmanaged handler adopted")
		}
	}
	if _, err := configuredRuntime([]byte("version=1")); err == nil {
		t.Fatal("unsupported containerd config")
	}
}
func activationFixture(t *testing.T) (HostActivation, []byte, *[]string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root-owned isolated Linux working directory")
	}
	dir, err := os.MkdirTemp(".", ".activation-test-")
	if err != nil {
		t.Fatal(err)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	for _, name := range []string{"host/etc/containerd", "host/usr/local/bin", "runtimes"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	original := []byte("version = 3\n")
	if err := os.WriteFile(filepath.Join(dir, "host/etc/containerd/config.toml"), original, 0644); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "source-shim")
	if err := os.WriteFile(source, []byte("test shim bytes"), 0755); err != nil {
		t.Fatal(err)
	}
	var calls []string
	a := HostActivation{HostRoot: filepath.Join(dir, "host"), InventoryRoot: filepath.Join(dir, "runtimes"), SourceBinary: source, ProfileUID: "profile-one", NodeUID: "node-one", runHost: func(_ context.Context, args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}, validateBaseline: func(context.Context) error { return nil }}
	return a, original, &calls
}
func TestActivationJournalCommitAndRestorationUnit(t *testing.T) {
	a, original, calls := activationFixture(t)
	ctx := context.Background()
	fence := func(context.Context) error { return nil }
	if err := a.Ensure(ctx, "validated", fence); err != nil {
		t.Fatal(err)
	}
	record, err := a.load()
	if err != nil || record.Phase != "Committed" {
		t.Fatal(record, err)
	}
	b, err := os.ReadFile(a.config())
	if err != nil || !strings.Contains(string(b), "io.containerd.brewlet.v2") {
		t.Fatal(string(b), err)
	}
	if err := a.Restore(ctx, fence); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(a.config())
	if err != nil || string(b) != string(original) {
		t.Fatal("original config not restored", err)
	}
	if _, err := os.Stat(a.binary()); !os.IsNotExist(err) {
		t.Fatal("shim not removed", err)
	}
	if _, err := os.Stat(a.journal()); !os.IsNotExist(err) {
		t.Fatal("restored journal not archived", err)
	}
	restarts := 0
	for _, call := range *calls {
		if call == "systemctl restart containerd" {
			restarts++
		}
	}
	if restarts != 2 {
		t.Fatal("activation and cleanup restarts not both invoked", *calls)
	}
}
func TestActivationRollbackUnit(t *testing.T) {
	a, original, _ := activationFixture(t)
	ctx := context.Background()
	a.validateBaseline = func(context.Context) error { return errors.New("live CRI not forwarding") }
	if err := a.Ensure(ctx, "validated", func(context.Context) error { return nil }); err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatal(err)
	}
	b, err := os.ReadFile(a.config())
	if err != nil || string(b) != string(original) {
		t.Fatal("rollback lost original config", err)
	}
	if _, err := os.Stat(a.binary()); !os.IsNotExist(err) {
		t.Fatal("rollback left shim", err)
	}
}
func TestActivationLostFenceLeavesPreparedJournalUnit(t *testing.T) {
	a, original, _ := activationFixture(t)
	ctx := context.Background()
	fence := func(context.Context) error {
		if _, err := os.Stat(a.journal()); err == nil {
			return errors.New("ownership-fence-failed")
		}
		return nil
	}
	if err := a.Ensure(ctx, "validated", fence); err == nil {
		t.Fatal("lost fence accepted")
	}
	b, err := os.ReadFile(a.config())
	if err != nil || string(b) != string(original) {
		t.Fatal("lost authority changed host config", err)
	}
	if _, err := os.Stat(a.binary()); !os.IsNotExist(err) {
		t.Fatal("lost authority installed shim", err)
	}
	record, err := a.load()
	if err != nil || record.Phase != "Prepared" {
		t.Fatal("recovery evidence missing", record, err)
	}
}
