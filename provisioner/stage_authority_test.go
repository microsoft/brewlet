package provisioner

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/pkg/node"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type cancellableInstaller struct{ started, stopped chan struct{} }

func (i cancellableInstaller) Stage(ctx context.Context, _ *api.RuntimeImage, _ string) (string, node.Metadata, error) {
	close(i.started)
	<-ctx.Done()
	close(i.stopped)
	return "", node.Metadata{}, ctx.Err()
}

type completionRaceReader struct {
	client.Reader
	entered, stopped chan struct{}
	err              error
}

func (r completionRaceReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*api.RuntimeImage); !ok {
		return r.Reader.Get(ctx, key, obj, opts...)
	}
	close(r.entered)
	defer close(r.stopped)
	<-ctx.Done()
	return r.err
}

type completionRaceInstaller struct {
	entered <-chan struct{}
	err     error
}

func (i completionRaceInstaller) Stage(ctx context.Context, _ *api.RuntimeImage, _ string) (string, node.Metadata, error) {
	select {
	case <-i.entered:
		return "completed-stage", node.Metadata{}, i.err
	case <-ctx.Done():
		return "", node.Metadata{}, ctx.Err()
	}
}

func TestStageCompletionJoinsInFlightMonitorWithoutFalseInstallFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		monitorErr error
		stageErr   error
	}{
		{name: "canceled read", monitorErr: context.Canceled},
		{name: "wrapped canceled read", monitorErr: fmt.Errorf("API read: %w", context.Canceled)},
		{name: "real monitor failure", monitorErr: errors.New("authority API unavailable")},
		{name: "monitor deadline", monitorErr: context.DeadlineExceeded},
		{name: "real installer failure", monitorErr: context.Canceled, stageErr: errors.New("invalid runtime layer")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := authorityFixture(t)
			reader := completionRaceReader{Reader: w.Reader, entered: make(chan struct{}),
				stopped: make(chan struct{}), err: tc.monitorErr}
			w.Reader = reader
			w.Installer = completionRaceInstaller{entered: reader.entered, err: tc.stageErr}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			stage, _, err := w.stage(ctx, &api.RuntimeImage{})
			want := tc.stageErr
			if !errors.Is(tc.monitorErr, context.Canceled) {
				want = tc.monitorErr
			}

			if !errors.Is(err, want) {
				t.Fatalf("stage error = %v, want %v", err, want)
			}
			if stage != "completed-stage" {
				t.Fatalf("stage = %q", stage)
			}
			select {
			case <-reader.stopped:
			default:
				t.Fatal("monitor API reader was not joined")
			}
		})
	}
}
func TestStageCompletionDoesNotHideParentCancellation(t *testing.T) {
	w, _ := authorityFixture(t)
	installer := cancellableInstaller{started: make(chan struct{}), stopped: make(chan struct{})}
	w.Installer = installer
	parent, timeout := context.WithTimeout(context.Background(), 3*time.Second)
	defer timeout()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	go func() {
		<-installer.started
		cancel()
	}()
	_, _, err := w.stage(ctx, &api.RuntimeImage{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("parent cancellation = %v", err)
	}
	select {
	case <-installer.stopped:
	default:
		t.Fatal("canceled installer was not joined")
	}
}

func TestInstallationStopsAndJoinsAfterFreshAuthorityLoss(t *testing.T) {
	w, _ := authorityFixture(t)
	i := cancellableInstaller{started: make(chan struct{}), stopped: make(chan struct{})}
	w.Installer = i
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	changed := make(chan error, 1)
	go func() {
		<-i.started
		p := new(api.NodeProfile)
		if err := w.Client.Get(ctx, client.ObjectKey{Name: "profile"}, p); err != nil {
			changed <- err
			return
		}
		p.Generation = 2
		changed <- w.Client.Update(ctx, p)
	}()
	_, _, err := w.stage(ctx, &api.RuntimeImage{})
	if err == nil {
		t.Fatal("installation continued after authority loss")
	}
	if err := <-changed; err != nil {
		t.Fatal(err)
	}
	select {
	case <-i.stopped:
	default:
		t.Fatal("staging worker was not joined")
	}
}
