//go:build linux

package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func publicationFixture(t *testing.T) (*Store, string, Metadata) {
	t.Helper()
	store := testStore(t)
	if err := store.WriteAuthorization(context.Background(), testAuthorization()); err != nil {
		t.Fatal(err)
	}
	stage, metadata := publicationStage(t)
	return store, stage, metadata
}

func publicationStage(t *testing.T) (string, Metadata) {
	t.Helper()
	root, metadata := runtimeRootFixture(t)
	stage := filepath.Join(testDirectory(t), "stage")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, filepath.Join(stage, "rootfs")); err != nil {
		t.Fatal(err)
	}
	return stage, metadata
}

func TestPublicationAtomicallyBindsImmutableMetadataRootAndReceipt(t *testing.T) {
	store, stage, metadata := publicationFixture(t)
	if err := store.Publish(context.Background(), stage, metadata, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	current, root, err := store.Current(context.Background(), metadata.ID)
	if err != nil || current.ID != metadata.ID || current.InventoryUID != metadata.InventoryUID ||
		current.Generation != metadata.Generation || current.SourceDigest != metadata.SourceDigest ||
		current.ManifestDigest != metadata.ManifestDigest || !reflect.DeepEqual(current.Env, metadata.Env) ||
		!reflect.DeepEqual(current.Platform, metadata.Platform) || !reflect.DeepEqual(current.Homes, metadata.Homes) ||
		current.UID != metadata.UID || current.GID != metadata.GID || current.Home != "/home/app" ||
		current.ValidatedAt.IsZero() {
		t.Fatalf("published metadata is not the validated generation: %+v, %v", current, err)
	}
	expected := filepath.Join(store.Root, metadata.ID, metadata.InventoryUID, "1", "rootfs")
	if root != expected {
		t.Fatalf("current root differs from immutable metadata generation: %s", root)
	}
	receipt, err := os.ReadFile(filepath.Join(root, ".brewlet-source"))
	if err != nil || string(receipt) != metadata.SourceDigest+"\n" {
		t.Fatal("publication lost the resolved source identity", string(receipt), err)
	}
	info, err := os.Stat(filepath.Join(filepath.Dir(root), "metadata.json"))
	if err != nil || info.Mode().Perm()&0077 != 0 {
		t.Fatal("immutable metadata is exposed to workload users", info, err)
	}
}

func TestPublicationFreshFenceFailureNeverPublishesCurrent(t *testing.T) {
	for _, failedCall := range []int{1, 2} {
		t.Run(strconv.Itoa(failedCall), func(t *testing.T) {
			store, stage, metadata := publicationFixture(t)
			calls := 0
			rejected := errors.New("fresh inventory or ownership changed")
			err := store.Publish(context.Background(), stage, metadata, func(context.Context) error {
				calls++
				if calls == failedCall {
					return rejected
				}
				return nil
			})
			if !errors.Is(err, rejected) {
				t.Fatal("lost fresh-fence failure", err)
			}
			if _, err := os.Lstat(filepath.Join(store.Root, metadata.ID, "current")); !os.IsNotExist(err) {
				t.Fatal("failed fresh fence published a current generation", err)
			}
		})
	}
}

func TestPublicationRejectsUnsafePreviousPointerWithoutReplacingIt(t *testing.T) {
	for _, previous := range []string{"../../foreign", "epoch-one/0", "epoch-one/no-generation", "epoch-one/1/extra"} {
		t.Run(strings.ReplaceAll(previous, "/", "_"), func(t *testing.T) {
			store, stage, metadata := publicationFixture(t)
			pointer := filepath.Join(store.Root, metadata.ID, "current")
			if err := os.Symlink(previous, pointer); err != nil {
				t.Fatal(err)
			}
			if err := store.Publish(context.Background(), stage, metadata,
				func(context.Context) error { return nil }); err == nil {
				t.Fatal("unsafe previous current pointer accepted")
			}
			if actual, err := os.Readlink(pointer); err != nil || actual != previous {
				t.Fatalf("failed publication replaced unvalidated durable current: %q, %v", actual, err)
			}
		})
	}
}

func TestPublicationRejectsImmutableGenerationCollision(t *testing.T) {
	store, stage, metadata := publicationFixture(t)
	ctx := context.Background()
	fence := func(context.Context) error { return nil }
	if err := store.Publish(ctx, stage, metadata, fence); err != nil {
		t.Fatal(err)
	}
	next, changed := publicationStage(t)
	changed.Env = append(changed.Env, "UNAPPROVED=collision")
	if err := store.Publish(ctx, next, changed, fence); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatal("immutable generation metadata replaced", err)
	}
	current, _, err := store.Current(ctx, metadata.ID)
	if err != nil || !reflect.DeepEqual(current.Env, metadata.Env) {
		t.Fatal("rejected collision changed current metadata", current, err)
	}
}

func TestPublicationRotationRetainsPreviousRootAndRecordsRetirement(t *testing.T) {
	store, stage, metadata := publicationFixture(t)
	ctx := context.Background()
	fence := func(context.Context) error { return nil }
	if err := store.Publish(ctx, stage, metadata, fence); err != nil {
		t.Fatal(err)
	}
	auth := testAuthorization()
	auth.Generation, auth.Digest = 2, "sha256:"+strings.Repeat("c", 64)
	if err := store.WriteAuthorization(ctx, auth); err != nil {
		t.Fatal(err)
	}
	next, target := publicationStage(t)
	target.Generation, target.SourceDigest = auth.Generation, auth.Digest
	if err := store.Publish(ctx, next, target, fence); err != nil {
		t.Fatal(err)
	}
	current, _, err := store.Current(ctx, metadata.ID)
	if err != nil || current.Generation != 2 || current.SourceDigest != auth.Digest {
		t.Fatal("rotation did not atomically publish the approved replacement", current, err)
	}
	if _, err := os.Stat(filepath.Join(store.Root, metadata.ID, metadata.InventoryUID, "1", "rootfs")); err != nil {
		t.Fatal("rotation removed the previous root", err)
	}
	var record retirement
	if err := store.read(store.retirementName(metadata.ID, metadata.InventoryUID, 1), &record); err != nil ||
		record.InventoryUID != metadata.InventoryUID || record.Generation != 1 || record.RetiredAt.IsZero() {
		t.Fatal("retirement lost the previous root identity or grace checkpoint", record, err)
	}
}

func TestPublishedMetadataReadbackRejectsUntrustedPermissions(t *testing.T) {
	store, stage, metadata := publicationFixture(t)
	ctx := context.Background()
	if err := store.Publish(ctx, stage, metadata, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(store.Root, metadata.ID, metadata.InventoryUID, "1", "metadata.json")
	if err := os.Chmod(record, 0666); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Current(ctx, metadata.ID); err == nil {
		t.Fatal("writable immutable metadata authorized current root readback")
	}
}

func TestCreateLeasesRemainAtomicWithConcurrentRotationAndCollection(t *testing.T) {
	store, stage, metadata := publicationFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fence := func(context.Context) error { return nil }
	if err := store.Publish(ctx, stage, metadata, fence); err != nil {
		t.Fatal(err)
	}
	old, _, err := store.Resolve(ctx, metadata.ID, "k8s.io", "old-task", nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	auth := testAuthorization()
	auth.Generation, auth.Digest = 2, "sha256:"+strings.Repeat("c", 64)
	if err := store.WriteAuthorization(ctx, auth); err != nil {
		t.Fatal(err)
	}
	next, target := publicationStage(t)
	target.Generation, target.SourceDigest = auth.Generation, auth.Digest
	results := make(chan error, 34)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for index := range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			lease, root, err := store.Resolve(ctx, metadata.ID, "k8s.io",
				"racing-task-"+strconv.Itoa(index), nil, 5*time.Second)
			if err != nil {
				results <- err
				return
			}
			expected := filepath.Join(store.Root, metadata.ID, metadata.InventoryUID,
				strconv.FormatInt(lease.Metadata.Generation, 10), "rootfs")
			if root != expected || (lease.Metadata.Generation != 1 && lease.Metadata.Generation != 2) {
				results <- errors.New("lease and resolved runtime root crossed generation boundaries")
				return
			}
			results <- nil
		}()
	}
	for _, action := range []func() error{
		func() error { return store.Publish(ctx, next, target, fence) },
		func() error { return store.Collect(ctx, metadata.ID, time.Nanosecond, fixedEvidence{}, fence) },
	} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			results <- action()
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	leases, err := store.Leases()
	if err != nil || len(leases) != 33 {
		t.Fatal("concurrent collection lost durable task leases", len(leases), err)
	}
	for _, lease := range leases {
		root := filepath.Join(store.Root, lease.Metadata.ID, lease.Metadata.InventoryUID,
			strconv.FormatInt(lease.Metadata.Generation, 10), "rootfs")
		if _, err := os.Stat(root); err != nil {
			t.Fatal("collection reclaimed a leased generation", lease.TaskID, err)
		}
	}
	current, _, err := store.Current(ctx, metadata.ID)
	if err != nil || current.Generation != 2 || old.Metadata.Generation != 1 {
		t.Fatal("rotation changed a previous lease or lost the new current", current, old, err)
	}
}
