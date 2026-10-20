//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	apitypes "github.com/containerd/containerd/api/types"
	runcoptions "github.com/containerd/containerd/api/types/runc/options"
	runtimeoptions "github.com/containerd/containerd/api/types/runtimeoptions/v1"
	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/task"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/errdefs"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"
	"github.com/containerd/typeurl/v2"
	"github.com/distribution/reference"
	"github.com/microsoft/brewlet/pkg/artifact"
	"github.com/microsoft/brewlet/pkg/node"
	brewletplatform "github.com/microsoft/brewlet/pkg/platform"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pelletier/go-toml/v2"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
)

const inventoryRoot = "/opt/brewlet/runtimes"
const cacheRoot = "/var/lib/brewlet/layers"

type service struct {
	taskapi.TTRPCTaskService
	client      *client.Client
	loadInitial func(string) (node.InitialState, error)
	publisher   shim.Publisher
}

func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.TTRPCPlugin, ID: "task", Requires: []plugin.Type{plugins.EventPlugin, plugins.InternalPlugin},
		InitFn: func(ic *plugin.InitContext) (any, error) {
			p, err := ic.GetByID(plugins.EventPlugin, "publisher")
			if err != nil {
				return nil, err
			}
			s, err := ic.GetByID(plugins.InternalPlugin, "shutdown")
			if err != nil {
				return nil, err
			}
			lifecycle := s.(shutdown.Service)
			inner, err := task.NewTaskService(ic.Context, p.(shim.Publisher), lifecycle)
			if err != nil {
				return nil, err
			}
			c, err := client.New(ic.Properties[plugins.PropertyGRPCAddress])
			if err != nil {
				return nil, err
			}
			lifecycle.RegisterCallback(func(context.Context) error { return c.Close() })
			return &service{TTRPCTaskService: inner, client: c, publisher: p.(shim.Publisher)}, nil
		},
	})
}
func (s *service) RegisterTTRPC(server *ttrpc.Server) error {
	taskapi.RegisterTTRPCTaskService(server, s)
	return nil
}

func (s *service) runtimeEvent(ctx context.Context, reason, id string, m node.Metadata) {
	if s.publisher == nil {
		log.Printf("runtime-image-event-failed: task=%s reason=%s: publisher unavailable", id, reason)
		return
	}
	event, err := structpb.NewStruct(map[string]any{"reason": reason, "containerID": id, "id": m.ID, "inventoryUID": m.InventoryUID, "generation": strconv.FormatInt(m.Generation, 10), "digest": m.SourceDigest})
	if err != nil {
		log.Printf("runtime-image-event-failed: task=%s reason=%s: encode: %v", id, reason, err)
		return
	}
	if err := s.publisher.Publish(ctx, "/brewlet/runtime-image", event); err != nil {
		log.Printf("runtime-image-event-failed: task=%s reason=%s: publish: %v", id, reason, err)
	}
}
func (s *service) Start(ctx context.Context, r *taskapi.StartRequest) (*taskapi.StartResponse, error) {
	response, err := s.TTRPCTaskService.Start(ctx, r)
	if err == nil && r.ExecID == "" {
		load := s.loadInitial
		if load == nil {
			load = node.NewStore(inventoryRoot).LoadInitial
		}
		if state, loadErr := load(r.ID); loadErr == nil {
			s.runtimeEvent(ctx, "RuntimeImageResolved", r.ID, state.Lease.Metadata)
		} else {
			infrastructure, kindErr := s.infrastructureTask(ctx, r.ID)
			if kindErr != nil || !infrastructure {
				log.Printf("runtime-image-event-failed: task=%s reason=RuntimeImageResolved: initial-state: %v; protected-kind: %v",
					r.ID, loadErr, kindErr)
			}
		}
	}
	return response, err
}

func (s *service) infrastructureTask(ctx context.Context, id string) (bool, error) {
	if s.client == nil {
		return false, fmt.Errorf("containerd metadata client unavailable")
	}
	container, err := s.client.LoadContainer(ctx, id)
	if err != nil {
		return false, err
	}
	spec, err := container.Spec(ctx)
	if err != nil {
		return false, err
	}
	return spec.Annotations["io.kubernetes.cri.container-type"] == "sandbox", nil
}

func normalizeOptions(r *taskapi.CreateTaskRequest) error {
	if r.Options == nil {
		return nil
	}
	v, err := typeurl.UnmarshalAny(r.Options)
	if err != nil {
		return err
	}
	switch o := v.(type) {
	case *runcoptions.Options:
		return nil
	case *runtimeoptions.Options:
		body := o.ConfigBody
		if o.ConfigPath != "" {
			body, err = os.ReadFile(o.ConfigPath)
			if err != nil {
				return err
			}
		}
		var opts runcoptions.Options
		if len(body) != 0 {
			if err := toml.Unmarshal(body, &opts); err != nil {
				return err
			}
		}
		r.Options, err = anypb.New(&opts)
		return err
	default:
		return fmt.Errorf("unsupported runc options type")
	}
}
func loadSpec(bundle string) (*specs.Spec, error) {
	b, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		return nil, err
	}
	var spec specs.Spec
	if err := json.Unmarshal(b, &spec); err != nil {
		return nil, err
	}
	return &spec, nil
}
func (s *service) Create(ctx context.Context, r *taskapi.CreateTaskRequest) (response *taskapi.CreateTaskResponse, createErr error) {
	if err := normalizeOptions(r); err != nil {
		return nil, err
	}
	spec, err := loadSpec(r.Bundle)
	if err != nil {
		return nil, err
	}
	if spec.Annotations["io.kubernetes.cri.container-type"] == "sandbox" {
		return s.TTRPCTaskService.Create(ctx, r)
	}
	cacheGuard, err := node.AcquireCacheGuard(ctx, cacheRoot, false)
	if err != nil {
		return nil, err
	}
	defer cacheGuard.Close()
	store := node.NewStore(inventoryRoot)
	taskGuard, err := store.AcquireTaskGuard(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	defer func() {
		taskGuard.Close()
		if createErr != nil {
			// An error is not proof of absence: runc may have left a task,
			// exec or mount. Retain the lease whenever inspection is uncertain.
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err := store.Release(cleanupCtx, r.ID, node.LinuxEvidence{Client: s.client, RuntimeRoot: inventoryRoot}); err != nil && !os.IsNotExist(err) {
				createErr = fmt.Errorf("%w; failed-create lease release deferred: %v", createErr, err)
			}
		}
	}()
	if err := s.assemble(ctx, r, spec); err != nil {
		return nil, err
	}
	state, err := node.NewStore(inventoryRoot).LoadInitial(r.ID)
	if err != nil {
		return nil, err
	}
	if err := node.NewStore(inventoryRoot).ValidateLease(ctx, state.Lease); err != nil {
		return nil, err
	}
	// The deferred unwind uses the same complete evidence as fenced recovery.
	return s.TTRPCTaskService.Create(ctx, r)
}
func (s *service) Exec(ctx context.Context, r *taskapi.ExecProcessRequest) (*emptypb.Empty, error) {
	load := s.loadInitial
	if load == nil {
		load = node.NewStore(inventoryRoot).LoadInitial
	}
	state, err := load(r.ID)
	if err != nil {
		return nil, err
	}
	namespace, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}
	if state.Lease.Namespace != namespace {
		return nil, &node.Error{Reason: "ExecStateUnavailable", Detail: "namespace mismatch"}
	}
	value, err := typeurl.UnmarshalAny(r.Spec)
	if err != nil {
		return nil, err
	}
	p, ok := value.(*specs.Process)
	if !ok {
		return nil, fmt.Errorf("invalid exec process protobuf type")
	}
	if err := node.RewriteExec(p, state); err != nil {
		return nil, err
	}
	encoded, err := typeurl.MarshalAny(p)
	if err != nil {
		return nil, err
	}
	r.Spec = &anypb.Any{TypeUrl: encoded.GetTypeUrl(), Value: encoded.GetValue()}
	return s.TTRPCTaskService.Exec(ctx, r)
}

func (s *service) Delete(ctx context.Context, r *taskapi.DeleteRequest) (*taskapi.DeleteResponse, error) {
	response, err := s.TTRPCTaskService.Delete(ctx, r)
	if err != nil || r.ExecID != "" {
		return response, err
	}
	store := node.NewStore(inventoryRoot)
	// Failed Create can leave a lease without initial.json. Its absence does not
	// prevent evidence-based release or authorize synthesizing process state.
	// Failure retains the lease and must not hide successful runc Delete.
	if err := store.Release(ctx, r.ID, node.LinuxEvidence{Client: s.client, RuntimeRoot: inventoryRoot}); err != nil && !os.IsNotExist(err) {
		log.Printf("lease-release-deferred: task=%s: %v", r.ID, err)
	}
	return response, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

func (s *service) resolve(ctx context.Context, id string, annotations map[string]string) (*artifact.Resolved, error) {
	container, err := s.client.LoadContainer(ctx, id)
	if err != nil {
		return nil, err
	}
	info, err := container.Info(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(info.Image) == "" {
		return nil, fmt.Errorf("protected container image identity missing")
	}
	ext, ok := info.Extensions["io.cri-containerd.container.metadata"]
	if !ok {
		return nil, fmt.Errorf("protected CRI metadata missing")
	}
	var envelope struct {
		Version  string `json:"Version"`
		Metadata struct {
			ImageRef string `json:"ImageRef"`
			Config   struct {
				Image struct {
					Image     string `json:"image"`
					Requested string `json:"user_specified_image"`
				} `json:"image"`
			} `json:"config"`
		} `json:"Metadata"`
	}
	if err := json.Unmarshal(ext.GetValue(), &envelope); err != nil {
		return nil, err
	}
	cri := envelope.Metadata
	if envelope.Version != "v1" || cri.ImageRef == "" || cri.Config.Image.Image != cri.ImageRef {
		return nil, fmt.Errorf("invalid protected CRI config identity")
	}
	requested := cri.Config.Image.Requested
	ref, err := reference.ParseNormalizedNamed(requested)
	if err != nil {
		return nil, err
	}
	canonical, ok := ref.(reference.Canonical)
	if !ok || canonical.Digest().Algorithm() != digest.SHA256 || canonical.Digest().Validate() != nil {
		return nil, fmt.Errorf("application image must be digest-pinned")
	}
	if _, tagged := ref.(reference.Tagged); tagged {
		return nil, fmt.Errorf("tagged application image is unsupported")
	}
	stamped, stampErr := reference.ParseNormalizedNamed(annotations["io.kubernetes.cri.image-name"])
	if stampErr != nil || stamped.String() != canonical.String() {
		return nil, fmt.Errorf("protected CRI image-name does not match requested image")
	}
	store := s.client.ContentStore()
	contentCache := &artifact.ContentCache{Dir: filepath.Join(cacheRoot, ".content"), IsMissing: func(err error) bool { return errdefs.IsNotFound(err) || os.IsNotExist(err) }}
	contentInfo, err := store.Info(ctx, canonical.Digest())
	if err != nil && !errdefs.IsNotFound(err) {
		return nil, err
	}
	if err == nil && contentInfo.Size > 16<<20 {
		return nil, fmt.Errorf("image target metadata too large")
	}
	var target ocispec.Descriptor
	if errdefs.IsNotFound(err) {
		target, err = contentCache.Target(ctx, canonical.Digest())
		if err != nil {
			return nil, err
		}
	} else {
		target = ocispec.Descriptor{Digest: canonical.Digest(), Size: contentInfo.Size}
	}
	if target.Size > 16<<20 {
		return nil, fmt.Errorf("image target metadata too large")
	}
	source := func(d ocispec.Descriptor) (io.ReadCloser, error) {
		r, err := store.ReaderAt(ctx, d)
		if err != nil {
			return nil, err
		}
		return readCloser{io.NewSectionReader(r, 0, r.Size()), r}, nil
	}
	reader, err := source(target)
	if errdefs.IsNotFound(err) {
		if target.MediaType == "" {
			// Content GC may race the successful Info above. Retain the
			// original descriptor, without masking inconsistent source info.
			retained, retainedErr := contentCache.Target(ctx, canonical.Digest())
			if retainedErr != nil {
				return nil, retainedErr
			}
			if retained.Size != target.Size {
				return nil, fmt.Errorf("retained target differs from protected content info")
			}
			target = retained
		}
		reader, err = contentCache.Open(ctx, source, target)
	}
	if err != nil {
		return nil, err
	}
	bytes, err := io.ReadAll(io.LimitReader(reader, 16<<20))
	reader.Close()
	if err != nil {
		return nil, err
	}
	var header struct {
		MediaType string `json:"mediaType"`
	}
	if err := json.Unmarshal(bytes, &header); err != nil {
		return nil, err
	}
	target.MediaType = header.MediaType
	open := func(d ocispec.Descriptor) (io.ReadCloser, error) { return contentCache.Open(ctx, source, d) }
	resolved, err := resolveNativeArtifact(ctx, open, target, brewletplatform.Native())
	if err != nil {
		return nil, err
	}
	if string(resolved.Manifest.Config.Digest) != cri.ImageRef {
		return nil, fmt.Errorf("LaunchConfigMismatch: selected manifest config differs from protected CRI config identity")
	}
	resolved.ContentRoots = contentCache.References()
	resolved.Reference = canonical.String()
	return resolved, nil
}

func (s *service) assemble(ctx context.Context, r *taskapi.CreateTaskRequest, spec *specs.Spec) error {
	id := spec.Annotations["brewlet.sh/runtime-image"]
	if id == "" {
		return &node.Error{Reason: "RuntimeImageRequired", Detail: "missing propagated pod annotation"}
	}
	if err := node.ValidateID(id); err != nil {
		return err
	}
	if err := node.CheckProcess(spec.Process); err != nil {
		return err
	}
	if err := validateHostMounts(spec.Mounts); err != nil {
		return err
	}
	for _, key := range []string{"brewlet.sh/jdk", "brewlet.sh/jvm-args", "brewlet.sh/arch"} {
		if _, ok := spec.Annotations[key]; ok {
			return &node.Error{Reason: "UnsupportedAnnotation", Detail: key}
		}
	}
	resolved, err := s.resolve(ctx, r.ID, spec.Annotations)
	if err != nil {
		return err
	}
	if !resolved.Launch.Allows(id) {
		return &node.Error{Reason: "RuntimeImageIncompatible", Detail: id}
	}
	barrier, err := node.ParseBarrier(spec.Annotations["brewlet.sh/rotated-to-generation"], id)
	if err != nil {
		return err
	}
	namespace, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return err
	}
	mounts, err := taskMountScope(r, namespace)
	if err != nil {
		return err
	}
	if spec.Root == nil || spec.Root.Path != "rootfs" {
		return fmt.Errorf("CRI bundle must use its managed rootfs path")
	}
	if !spec.Root.Readonly {
		return &node.Error{Reason: "ReadOnlyRootRequired", Detail: "Brewlet requires readOnlyRootFilesystem=true"}
	}
	store := node.NewStore(inventoryRoot)
	if current, _, currentErr := store.Current(ctx, id); os.IsNotExist(currentErr) {
		s.runtimeEvent(ctx, "RuntimeImageInstalling", r.ID, node.Metadata{ID: id})
	} else if currentErr == nil && barrier != nil && current.Generation < barrier.Generation {
		s.runtimeEvent(ctx, "RuntimeImageUpdating", r.ID, current)
	}
	lease, root, err := store.ResolveTask(ctx, id, namespace, r.ID, barrier, 30*time.Second, mounts)
	if err != nil {
		return err
	}
	if err := validateRuntimeHostPlatform(lease.Metadata.Platform, brewletplatform.Native()); err != nil {
		return err
	}
	if err := node.MergeProcess(spec.Process, lease.Metadata); err != nil {
		return err
	}
	if err := node.ValidateFilesystem(spec, lease.Metadata, root); err != nil {
		return err
	}
	if err := os.MkdirAll(cacheRoot, 0700); err != nil {
		return err
	}
	if err := node.NewStore(cacheRoot).Check(); err != nil {
		return err
	}
	var layerRoots []string
	for _, layer := range resolved.Layers {
		if layer.Descriptor.Annotations["brewlet.sh/layer"] == "launch" {
			continue
		}
		root, err := (artifact.LayerCache{Dir: cacheRoot}).Materialize(ctx, layer)
		if err != nil {
			return err
		}
		layerRoots = append(layerRoots, root)
	}
	var upper, work string
	var overlayExtras []string
	if len(r.Rootfs) != 1 {
		return fmt.Errorf("Brewlet requires a single managed overlay rootfs mount")
	}
	for _, mount := range r.Rootfs {
		if mount.Type != "overlay" {
			continue
		}
		for _, opt := range mount.Options {
			if strings.HasPrefix(opt, "upperdir=") {
				upper = strings.TrimPrefix(opt, "upperdir=")
			}
			if strings.HasPrefix(opt, "workdir=") {
				work = strings.TrimPrefix(opt, "workdir=")
			}
			if !strings.HasPrefix(opt, "lowerdir=") && !strings.HasPrefix(opt, "upperdir=") && !strings.HasPrefix(opt, "workdir=") && opt != "ro" && opt != "rw" {
				if opt == "userxattr" {
					return fmt.Errorf("rootless userxattr snapshotters are unsupported")
				}
				overlayExtras = append(overlayExtras, opt)
			}
		}
	}
	lowers, err := applicationRuntimeLowers(r.Bundle, layerRoots, root)
	if err != nil {
		return err
	}
	options := []string{"lowerdir=" + strings.Join(lowers, ":")}
	options = append(options, overlayExtras...)
	if spec.Root == nil {
		return fmt.Errorf("OCI root missing")
	}
	if err := prepareAppMountpoints(r.Bundle, spec.Root.Path, lowers, upper, work, spec.Mounts, overlayExtras...); err != nil {
		return err
	}
	if upper == "" || work == "" {
		return fmt.Errorf("CRI snapshotter must provide managed overlay upper/work directories")
	}
	// runc must prepare CRI mountpoints (/etc/hosts, resolv.conf, volumes)
	// before its final required read-only-root remount. Mounting the snapshot
	// ro here would fail before that stage.
	options = append(options, "upperdir="+upper, "workdir="+work)
	r.Rootfs = []*apitypes.Mount{{Type: "overlay", Source: "overlay", Options: options}}
	if spec.Root.Path != "rootfs" {
		return fmt.Errorf("CRI bundle must use its managed rootfs path")
	}
	appMount := specs.Mount{Destination: "/app", Type: "bind", Source: filepath.Join(r.Bundle, "rootfs", "app"), Options: []string{"rbind", "ro", "nosuid", "nodev"}}
	// Explicit volume mounts under /app must follow the packaged RO mount.
	spec.Mounts = append([]specs.Mount{appMount}, spec.Mounts...)
	application := node.ApplicationIdentity{Reference: resolved.Reference, RootDigest: resolved.Target.Digest.String(),
		ManifestDigest: resolved.ManifestDescriptor.Digest.String(), ConfigDigest: resolved.Manifest.Config.Digest.String(), Platform: resolved.Image.Platform}
	if err := application.Validate(); err != nil {
		return err
	}
	if err := store.SaveInitial(node.InitialState{Application: application, Lease: lease, Process: *spec.Process, LayerRoots: layerRoots, ContentRoots: resolved.ContentRoots}); err != nil {
		return err
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.Bundle, "config.json"), b, 0600)
}
