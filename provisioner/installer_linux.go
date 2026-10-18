//go:build linux

package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/mount"
	snapshotapi "github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/kubernetes/policy"
	"github.com/microsoft/brewlet/pkg/node"
	"github.com/opencontainers/image-spec/identity"
)

type ContainerdInstaller struct {
	Client            *client.Client
	Snapshotter       string
	RegistryHostsDir  string
	ProfileUID        string
	NodeUID           string
	ProfileGeneration int64
	Fence             func(context.Context) error
	MountEvidence     node.MountEvidence
	// Rewrite validates mirror policy and preserves the source digest.
	Rewrite func(string) (string, error)
}

func (i *ContainerdInstaller) Stage(ctx context.Context, r *api.RuntimeImage, base string) (string, node.Metadata, error) {
	var m node.Metadata
	if i.ProfileUID == "" || i.NodeUID == "" || i.ProfileGeneration < 1 {
		return "", m, fmt.Errorf("literal staging owner identity required")
	}
	if err := i.fence(ctx); err != nil {
		return "", m, err
	}
	if err := policy.Runtime(r, nil); err != nil {
		return "", m, err
	}
	if !strings.HasSuffix(r.Spec.Source.Image, "@"+r.Status.Digest) {
		return "", m, fmt.Errorf("approved digest does not match source")
	}
	ref := r.Spec.Source.Image
	if i.Rewrite != nil {
		var err error
		ref, err = i.Rewrite(ref)
		if err != nil {
			return "", m, err
		}
	}
	if i.Snapshotter == "" {
		i.Snapshotter = "overlayfs"
	}
	declared, err := i.runtimePlatforms(ctx, r)
	if err != nil {
		return "", m, err
	}
	wanted := platforms.Normalize(platforms.DefaultSpec())
	selected, err := declared.Select(wanted)
	if err != nil {
		return "", m, err
	}
	options := []client.RemoteOpt{client.WithPlatformMatcher(platforms.OnlyStrict(wanted)), client.WithPullUnpack, client.WithPullSnapshotter(i.Snapshotter)}
	if i.RegistryHostsDir != "" {
		resolver, err := registryResolver(ctx, i.RegistryHostsDir)
		if err != nil {
			return "", m, err
		}
		options = append(options, client.WithResolver(resolver))
	}
	image, err := i.Client.GetImage(ctx, ref)
	if errdefs.IsNotFound(err) && ref != r.Spec.Source.Image {
		image, err = i.Client.GetImage(ctx, r.Spec.Source.Image)
	}
	if errdefs.IsNotFound(err) {
		image, err = i.Client.Pull(ctx, ref, options...)
	}
	if err != nil {
		return "", m, err
	}
	if image.Target().Digest.String() != r.Status.Digest {
		return "", m, fmt.Errorf("approved source digest mismatch")
	}
	image = client.NewImageWithPlatform(i.Client, image.Metadata(), platforms.OnlyStrict(wanted))
	unpacked, err := image.IsUnpacked(ctx, i.Snapshotter)
	if err != nil {
		return "", m, err
	}
	if !unpacked {
		if err := image.Unpack(ctx, i.Snapshotter); err != nil {
			return "", m, err
		}
	}
	config := selected.Config
	if r.Spec.User.UID <= 0 || r.Spec.User.UID >= 1<<32-1 || r.Spec.User.GID <= 0 || r.Spec.User.GID >= 1<<32-1 {
		return "", m, fmt.Errorf("invalid runtime app identity")
	}
	m = node.Metadata{ID: r.Name, InventoryUID: string(r.UID), Generation: r.Status.Generation, SourceDigest: r.Status.Digest, ManifestDigest: selected.Manifest.Digest.String(), Platform: selected.Platform, Env: config.Config.Env, UID: uint32(r.Spec.User.UID), GID: uint32(r.Spec.User.GID)}
	stagingBase := filepath.Join(base, ".staging")
	if err := os.MkdirAll(stagingBase, 0700); err != nil {
		return "", m, err
	}
	stage, err := os.MkdirTemp(stagingBase, ".runtime-pending-")
	if err != nil {
		return "", m, err
	}
	diffs, err := image.RootFS(ctx)
	if err != nil {
		return stage, m, err
	}
	if !slices.Equal(diffs, config.RootFS.DiffIDs) {
		return stage, m, fmt.Errorf("runtime snapshot platform/config diff IDs do not match selected manifest")
	}
	key := fmt.Sprintf("brewlet-copy-%d-%d", os.Getpid(), time.Now().UnixNano())
	record := stagingRecord{ProfileUID: i.ProfileUID, NodeUID: i.NodeUID, ProfileGeneration: i.ProfileGeneration, ID: r.Name, InventoryUID: string(r.UID), Generation: r.Status.Generation, Digest: r.Status.Digest, Snapshotter: i.Snapshotter, SnapshotKey: key}
	data, err := json.Marshal(record)
	if err != nil {
		return stage, m, err
	}
	if err := i.fence(ctx); err != nil {
		return stage, m, err
	}
	if err := writeDurable(filepath.Join(stage, "pending.json"), data, 0600); err != nil {
		return stage, m, err
	}
	if err := i.fence(ctx); err != nil {
		return stage, m, err
	}
	published := filepath.Join(stagingBase, "runtime-"+strings.TrimPrefix(filepath.Base(stage), ".runtime-pending-"))
	if err := os.Rename(stage, published); err != nil {
		return stage, m, err
	}
	stage = published
	if err := syncDir(stagingBase); err != nil {
		return stage, m, err
	}
	source := filepath.Join(stage, "source")
	root := filepath.Join(stage, "rootfs")
	if err := i.fence(ctx); err != nil {
		return stage, m, err
	}
	if err := os.Mkdir(source, 0700); err != nil {
		return stage, m, err
	}
	if err := os.Mkdir(root, 0755); err != nil {
		return stage, m, err
	}
	snapshots := i.Client.SnapshotService(i.Snapshotter)
	mounts, err := snapshots.View(ctx, key, identity.ChainID(diffs).String(), snapshotapi.WithLabels(map[string]string{"brewlet.sh/profile-uid": i.ProfileUID, "brewlet.sh/node-uid": i.NodeUID, "brewlet.sh/runtime-image": r.Name, "brewlet.sh/inventory-uid": string(r.UID)}))
	if err != nil {
		return stage, m, err
	}
	defer i.removeStagingSnapshot(ctx, snapshots, key)
	if err := mount.All(mounts, source); err != nil {
		return stage, m, err
	}
	copyErr := copyRoot(ctx, source, root)
	if err := i.fence(ctx); err != nil {
		return stage, m, err
	}
	if err := mount.UnmountAll(source, 0); err != nil {
		return stage, m, err
	}
	if copyErr != nil {
		return stage, m, copyErr
	}
	if err := os.Remove(source); err != nil {
		return stage, m, err
	}
	if err := node.ValidateRoot(root, &m); err != nil {
		return stage, m, err
	}
	return stage, m, nil
}

func (i *ContainerdInstaller) removeStagingSnapshot(ctx context.Context, snapshots snapshotapi.Snapshotter, key string) {
	if err := i.fence(ctx); err != nil {
		log.Printf("runtime-staging-snapshot-cleanup-deferred: snapshot=%s profileUID=%s nodeUID=%s generation=%d: %v",
			key, i.ProfileUID, i.NodeUID, i.ProfileGeneration, err)
		return
	}
	if err := snapshots.Remove(ctx, key); err != nil && !errdefs.IsNotFound(err) {
		log.Printf("runtime-staging-snapshot-cleanup-deferred: snapshot=%s profileUID=%s nodeUID=%s generation=%d: %v",
			key, i.ProfileUID, i.NodeUID, i.ProfileGeneration, err)
	}
}

func copyRoot(ctx context.Context, source, destination string) error {
	type directory struct {
		path string
		info os.FileInfo
	}
	var directories []directory
	err := filepath.WalkDir(source, func(name string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(source, name)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		st, err := e.Info()
		if err != nil {
			return err
		}
		if st.Mode().IsRegular() && st.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
			return fmt.Errorf("runtime setuid/setgid entry %s", rel)
		}
		// Inspect the source before copying: dropping capabilities while copying
		// must not turn an invalid approved image into an accepted root.
		if err := node.CheckFileCapabilities(name); err != nil {
			return err
		}
		switch {
		case e.IsDir():
			if rel != "." {
				if err := os.Mkdir(target, st.Mode().Perm()); err != nil {
					return err
				}
			}
			directories = append(directories, directory{target, st})
			return nil
		case st.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(name)
			if err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
			if stat, ok := st.Sys().(*syscall.Stat_t); ok {
				return os.Lchown(target, int(stat.Uid), int(stat.Gid))
			}
			return fmt.Errorf("runtime source ownership unavailable")
		case st.Mode().IsRegular():
			in, err := os.Open(name)
			if err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, st.Mode().Perm())
			if err != nil {
				in.Close()
				return err
			}
			_, err = io.Copy(out, contextReader{ctx: ctx, Reader: in})
			in.Close()
			if err == nil {
				err = out.Sync()
			}

			closeErr := out.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("runtime special file %s", rel)
		}
		stat, ok := st.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("runtime source ownership unavailable")
		}
		if err := os.Chown(target, int(stat.Uid), int(stat.Gid)); err != nil {
			return err
		}
		if err := os.Chmod(target, st.Mode()&(os.ModePerm|os.ModeSticky)); err != nil {
			return err
		}
		return os.Chtimes(target, st.ModTime(), st.ModTime())
	})
	if err != nil {
		return err
	}
	for i := len(directories) - 1; i >= 0; i-- {
		d := directories[i]
		stat, ok := d.info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("runtime directory ownership unavailable")
		}
		if err := os.Chown(d.path, int(stat.Uid), int(stat.Gid)); err != nil {
			return err
		}
		if err := os.Chmod(d.path, d.info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky)); err != nil {
			return err
		}
		if err := os.Chtimes(d.path, d.info.ModTime(), d.info.ModTime()); err != nil {
			return err
		}
	}
	return nil
}
