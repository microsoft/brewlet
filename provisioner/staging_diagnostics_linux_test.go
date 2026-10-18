//go:build linux

package provisioner

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
)

type diagnosticSnapshotter struct {
	snapshots.Snapshotter
	calls int
	err   error
}

func (s *diagnosticSnapshotter) Remove(context.Context, string) error {
	s.calls++
	return s.err
}

func TestDeferredStagingSnapshotCleanupReportsFailureWithoutIgnoringFence(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		fence, remove error
		calls         int
	}{
		{name: "removed", calls: 1},
		{name: "already absent", remove: errdefs.ErrNotFound, calls: 1},
		{name: "remove failed", remove: errors.New("snapshot store unavailable"), calls: 1,
			message: "snapshot store unavailable"},
		{name: "stale owner", fence: errors.New("ownership-fence-failed"),
			message: "ownership-fence-failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&output)
			defer log.SetOutput(previous)
			installer := &ContainerdInstaller{ProfileUID: "profile", NodeUID: "node", ProfileGeneration: 7,
				Fence: func(context.Context) error { return tc.fence }}
			snapshotter := &diagnosticSnapshotter{err: tc.remove}
			installer.removeStagingSnapshot(context.Background(), snapshotter, "brewlet-copy-1-2")
			if snapshotter.calls != tc.calls {
				t.Fatal("snapshot cleanup ignored authority", snapshotter.calls)
			}
			if tc.message == "" {
				if output.Len() != 0 {
					t.Fatal("successful cleanup reported a failure", output.String())
				}
			} else if !strings.Contains(output.String(), tc.message) ||
				!strings.Contains(output.String(), "profileUID=profile nodeUID=node generation=7") ||
				!strings.Contains(output.String(), "snapshot=brewlet-copy-1-2") {
				t.Fatal("deferred cleanup lost failure/ownership diagnostics", output.String())
			}
		})
	}
}
