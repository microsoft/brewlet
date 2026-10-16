package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	coordination "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// RestartLeadership adds a cluster singleton without changing the manager's
// component-namespace election lease during a rolling feature enablement.
// Its leader context cancels in-flight restart API calls if that lease is lost.
type RestartLeadership struct {
	Config *rest.Config
	mu     sync.RWMutex
	leader context.Context
}

func (l *RestartLeadership) current() context.Context {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.leader
}
func (l *RestartLeadership) set(ctx context.Context)  { l.mu.Lock(); l.leader = ctx; l.mu.Unlock() }
func (l *RestartLeadership) NeedLeaderElection() bool { return true }
func (l *RestartLeadership) Start(ctx context.Context) error {
	c, err := coordination.NewForConfig(l.Config)
	if err != nil {
		return err
	}
	lock := &resourcelock.LeaseLock{LeaseMeta: metav1.ObjectMeta{Name: "brewlet-operator.node.brewlet.sh", Namespace: "kube-system"}, Client: c, LockConfig: resourcelock.ResourceLockConfig{Identity: string(uuid.NewUUID())}}
	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock: lock, LeaseDuration: 30 * time.Second, RenewDeadline: 20 * time.Second, RetryPeriod: 5 * time.Second, ReleaseOnCancel: true, Name: "brewlet-rotation-restarts",
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leadCtx context.Context) { l.set(leadCtx); <-leadCtx.Done(); l.set(nil) },
			OnStoppedLeading: func() { l.set(nil) },
		},
	})
	if err != nil {
		return err
	}
	elector.Run(ctx)
	l.set(nil)
	if ctx.Err() == nil {
		return fmt.Errorf("global restart leadership lost; stopping manager rather than issuing uncoordinated patches")
	}
	return nil
}
