package provisioner

import (
	"context"
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
