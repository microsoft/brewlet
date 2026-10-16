package artifact

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

const PolicyVersion = "v2-3"

type LayerSource struct {
	Descriptor v1.Descriptor
	DiffID     digest.Digest
	Open       func() (io.ReadCloser, error)
}
type Entry struct {
	Path, Link string
	Type       byte
	Mode       int64
	ModTime    time.Time
}
type LayerInfo struct {
	DiffID  digest.Digest
	Entries []Entry
	Launch  []byte
}

func archivePath(s string) (string, error) {
	if s == "" || strings.HasPrefix(s, "/") || strings.ContainsRune(s, 0) || strings.Contains(s, `\`) {
		return "", fail("LayerPathViolation", "invalid path %q", s)
	}
	parts := strings.Split(strings.TrimSuffix(s, "/"), "/")
	for i, part := range parts {
		if part == ".." {
			return "", fail("LayerPathViolation", "parent traversal %q", s)
		}
		if i < len(parts)-1 && strings.HasPrefix(part, ".wh.") {
			return "", fail("LayerPathViolation", "whiteout name used as directory %q", s)
		}
	}
	s = path.Clean(s)
	if s != "app" && !strings.HasPrefix(s, "app/") {
		return "", fail("LayerPathViolation", "outside app: %q", s)
	}
	return s, nil
}
func confinedLink(name, target string, hard bool) (string, error) {
	if target == "" || path.IsAbs(target) || strings.ContainsAny(target, "\x00\\") {
		return "", fail("LayerPathViolation", "invalid link %q", target)
	}
	dest := target
	if !hard {
		dest = path.Join(path.Dir(name), target)
	}
	dest = path.Clean(dest)
	if dest != "app" && !strings.HasPrefix(dest, "app/") {
		return "", fail("LayerPathViolation", "escaping link %q", target)
	}
	return dest, nil
}
func whiteout(name string) (string, bool, bool) {
	base := path.Base(name)
	if base == ".wh..wh..opq" {
		return path.Dir(name), true, true
	}
	if strings.HasPrefix(base, ".wh.") {
		return path.Join(path.Dir(name), strings.TrimPrefix(base, ".wh.")), true, false
	}
	return "", false, false
}

// WalkLayer verifies compressed identity and diff ID while streaming. The visitor
// must consume each regular file before returning; unread bytes are drained.
func WalkLayer(ctx context.Context, src LayerSource, visit func(*tar.Header, io.Reader) error) (*LayerInfo, error) {
	if src.Descriptor.Digest.Algorithm() != digest.SHA256 || src.Descriptor.Digest.Validate() != nil || src.Descriptor.Size < 0 || src.Descriptor.Size == math.MaxInt64 {
		return nil, fail("UnsupportedArtifact", "invalid layer descriptor")
	}
	input, err := src.Open()
	if err != nil {
		return nil, err
	}
	defer input.Close()
	cd := digest.SHA256.Digester()
	count := &countReader{r: io.TeeReader(io.LimitReader(contextReader{ctx, input}, src.Descriptor.Size+1), cd.Hash())}
	var uncompressed io.Reader
	var closeDecoder func()
	switch src.Descriptor.MediaType {
	case v1.MediaTypeImageLayerGzip:
		gz, e := gzip.NewReader(count)
		if e != nil {
			return nil, e
		}
		uncompressed = gz
		closeDecoder = func() { _ = gz.Close() }
	case v1.MediaTypeImageLayerZstd:
		z, e := zstd.NewReader(count, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(128<<20))
		if e != nil {
			return nil, e
		}
		uncompressed = z
		closeDecoder = z.Close
	default:
		return nil, fail("UnsupportedArtifact", "unsupported layer media type")
	}
	defer closeDecoder()
	ud := digest.SHA256.Digester()
	stream := io.TeeReader(uncompressed, ud.Hash())
	tr := tar.NewReader(stream)
	info := &LayerInfo{}
	regular := map[string]bool{}
	pending := map[string]string{}
	seen := map[string]bool{}
	launch := src.Descriptor.Annotations["brewlet.sh/layer"] == "launch"
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, fail("LayerPathViolation", "invalid tar: %v", e)
		}
		name := path.Clean(h.Name)
		if launch {
			if len(info.Entries) != 0 || h.Name != "launch.json" || h.Typeflag != tar.TypeReg || h.Size > 1<<20 {
				return nil, fail("InvalidLaunchConfig", "launch layer must contain exactly launch.json")
			}
		} else {
			name, e = archivePath(h.Name)
			if e != nil {
				return nil, e
			}
		}
		if seen[name] {
			return nil, fail("LayerPathViolation", "duplicate entry %q", name)
		}
		seen[name] = true
		for k := range h.PAXRecords {
			if strings.Contains(strings.ToLower(k), "xattr") || strings.Contains(strings.ToLower(k), "capability") {
				return nil, fail("LayerPathViolation", "extended attributes forbidden")
			}
		}
		if len(h.Xattrs) > 0 || h.Mode&06000 != 0 {
			return nil, fail("LayerPathViolation", "xattrs or set-id bits forbidden")
		}
		_, wh, _ := whiteout(name)
		if wh {
			target, _, opaque := whiteout(name)
			if launch || h.Typeflag != tar.TypeReg || h.Size != 0 || target == "app" && !opaque || path.Base(target) == "." || path.Base(target) == "" {
				return nil, fail("LayerPathViolation", "invalid whiteout")
			}
			if _, e = archivePath(target); e != nil {
				return nil, e
			}
		} else {
			switch h.Typeflag {
			case tar.TypeReg:
				if h.Mode&0004 == 0 || h.Mode&0111 != 0 && h.Mode&0001 == 0 {
					return nil, fail("LayerPathViolation", "file not accessible to arbitrary UID: %s", name)
				}
				regular[name] = true
			case tar.TypeDir:
				if h.Mode&0005 != 0005 {
					return nil, fail("LayerPathViolation", "directory not accessible: %s", name)
				}
			case tar.TypeSymlink, tar.TypeLink:
				target, e := confinedLink(name, h.Linkname, h.Typeflag == tar.TypeLink)
				if e != nil {
					return nil, e
				}
				if h.Typeflag == tar.TypeLink {
					pending[name] = target
				}
			default:
				return nil, fail("LayerPathViolation", "unsupported entry type %d", h.Typeflag)
			}
		}
		info.Entries = append(info.Entries, Entry{name, h.Linkname, h.Typeflag, h.Mode, h.ModTime})
		if launch {
			info.Launch, e = io.ReadAll(tr)
			if e != nil {
				return nil, e
			}
		}
		if visit != nil {
			copyH := *h
			copyH.Name = name
			if e = visit(&copyH, tr); e != nil {
				return nil, e
			}
		}
	}
	for name, target := range pending {
		if !regular[target] {
			return nil, fail("LayerPathViolation", "hardlink %s does not reference same-layer regular file", name)
		}
	}
	// Consume the decoder to its checksum/footer and include all tar padding in the diff ID.
	if _, err = io.Copy(zeroWriter{}, stream); err != nil {
		return nil, err
	}

	if _, err = io.Copy(io.Discard, count); err != nil {
		return nil, err
	}
	if count.n != src.Descriptor.Size || cd.Digest() != src.Descriptor.Digest {
		return nil, fail("DigestMismatch", "compressed layer identity mismatch")
	}
	info.DiffID = ud.Digest()
	if src.DiffID != "" && src.DiffID != info.DiffID {
		return nil, fail("DigestMismatch", "layer diff ID mismatch")
	}
	if launch {
		if len(info.Entries) != 1 {
			return nil, fail("InvalidLaunchConfig", "empty launch layer")
		}
		if _, err = ParseLaunch(info.Launch); err != nil {
			return nil, err
		}
	} else if _, err = compileLayer(info); err != nil {
		return nil, err
	}
	return info, nil
}

type zeroWriter struct{}

func (zeroWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		if b != 0 {
			return 0, fail("LayerPathViolation", "nonzero data after tar terminator")
		}
	}
	return len(p), nil
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) { n, e := c.r.Read(p); c.n += int64(n); return n, e }

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(p)
}

// MaterializeLayers creates a flattened, verified /app tree in a newly published
// destination. The caller must own the parent directory. No existing destination
// is ever replaced. Runtime roots and live stages must never be supplied here.
func MaterializeLayers(ctx context.Context, sources []LayerSource, destination string) error {
	if err := ValidateComposition(ctx, sources); err != nil {
		return err
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".brewlet-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err = os.Chmod(stage, 0755); err != nil {
		return err
	}
	root, err := os.OpenRoot(stage)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, src := range sources {
		if src.Descriptor.Annotations["brewlet.sh/layer"] == "launch" {
			continue
		}
		// OCI deletion metadata affects lower layers only, regardless of where
		// its marker appears in the tar relative to this layer's new files.
		info, e := WalkLayer(ctx, src, nil)
		if e != nil {
			return e
		}
		layer, e := compileLayer(info)
		if e != nil {
			return e
		}
		for _, entry := range layer.entries {
			target, wh, opaque := whiteout(entry.Path)
			if !wh {
				continue
			}
			if err := ensureDiskParents(root, target); err != nil {
				return err
			}
			if opaque {
				if e = ensureDiskDirectory(root, target); e != nil {
					return e
				}
				dir, e := root.Open(target)
				if os.IsNotExist(e) {
					continue
				}
				if e != nil {
					return e
				}
				names, e := dir.Readdirnames(-1)
				dir.Close()
				if e != nil {
					return e
				}
				for _, n := range names {
					if e = root.RemoveAll(path.Join(target, n)); e != nil {
						return e
					}
				}
			} else if e := root.RemoveAll(target); e != nil {
				return e
			}
		}
		var links []*tar.Header
		var dirs []*tar.Header
		entryIndex := 0
		_, err = WalkLayer(ctx, src, func(h *tar.Header, r io.Reader) error {
			entry := layer.entries[entryIndex]
			entryIndex++
			h.Name = entry.Path
			if h.Typeflag == tar.TypeLink {
				h.Linkname = entry.Link
			}
			target, wh, opaque := whiteout(h.Name)
			if wh {
				_, _ = target, opaque
				return nil
			}
			if err := ensureDiskParents(root, h.Name); err != nil {
				return err
			}
			if h.Typeflag == tar.TypeDir {
				fi, e := root.Lstat(h.Name)
				if e == nil && !fi.IsDir() {
					if e = root.RemoveAll(h.Name); e != nil {
						return e
					}
				}
				if e = root.MkdirAll(h.Name, 0755); e != nil {
					return e
				}
				dirs = append(dirs, h)
				return nil
			}
			if err := root.RemoveAll(h.Name); err != nil {
				return err
			}
			switch h.Typeflag {
			case tar.TypeReg:
				f, e := root.OpenFile(h.Name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if e != nil {
					return e
				}
				_, e = io.Copy(f, r)
				ce := f.Close()
				if e != nil {
					return e
				}
				if ce != nil {
					return ce
				}
				if e = root.Chmod(h.Name, os.FileMode(h.Mode)&0777); e != nil {
					return e
				}
				return root.Chtimes(h.Name, h.ModTime, h.ModTime)
			case tar.TypeSymlink:
				return root.Symlink(h.Linkname, h.Name)
			case tar.TypeLink:
				links = append(links, h)
				return nil
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, h := range links {
			if err = ensureDiskParents(root, h.Linkname); err != nil {
				return err
			}
			fi, e := root.Lstat(h.Linkname)
			if e != nil || !fi.Mode().IsRegular() {
				return fail("LayerPathViolation", "invalid hardlink target")
			}
			if err = root.Link(h.Linkname, h.Name); err != nil {
				return err
			}
		}
		for i := len(dirs) - 1; i >= 0; i-- {
			h := dirs[i]
			if node, ok := layer.nodes[h.Name]; !ok || node.Type != tar.TypeDir {
				continue
			}
			if err = root.Chmod(h.Name, os.FileMode(h.Mode)&0777); err != nil {
				return err
			}
			if err = root.Chtimes(h.Name, h.ModTime, h.ModTime); err != nil {
				return err
			}
		}
	}
	if _, err = os.Lstat(destination); err == nil {
		return fmt.Errorf("destination already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	// Atomic no-replace publication is serialized by a sibling exclusive marker.
	lock, err := os.OpenFile(destination+".publish-lock", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	lock.Close()
	defer os.Remove(destination + ".publish-lock")
	if _, err = os.Lstat(destination); !os.IsNotExist(err) {
		return fmt.Errorf("destination already exists")
	}
	return os.Rename(stage, destination)
}
func safeParents(root *os.Root, name string) error {
	_, err := resolveLayerPath(root, path.Dir(name), 0)
	return err
}

func ensureDiskDirectory(root *os.Root, name string) error {
	fi, e := root.Lstat(name)
	if e == nil {
		if fi.IsDir() {
			return root.Chmod(name, 0755)
		}
		if e = root.RemoveAll(name); e != nil {
			return e
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	return root.Mkdir(name, 0755)
}
func ensureDiskParents(root *os.Root, name string) error {
	parts := strings.Split(path.Dir(name), "/")
	cur := "."
	for _, part := range parts {
		if part == "." || part == "" {
			continue
		}
		cur = path.Join(cur, part)
		if _, e := archivePath(cur); e != nil {
			return e
		}
		if e := ensureDiskDirectory(root, cur); e != nil {
			return e
		}
	}
	return nil
}

func resolveLayerPath(root *os.Root, name string, depth int) (string, error) {
	if depth > 40 {
		return "", fail("LayerPathViolation", "symlink cycle")
	}
	if name == "." {
		return ".", nil
	}
	cur := "."
	pending := strings.Split(name, "/")
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			cur = path.Dir(cur)
			if cur != "app" && !strings.HasPrefix(cur, "app/") {
				return "", fail("LayerPathViolation", "link escapes app")
			}
			continue
		}
		cur = path.Join(cur, part)
		if _, err := archivePath(cur); err != nil {
			return "", err
		}
		fi, err := root.Lstat(cur)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", fail("LayerPathViolation", "composed path %s: %v", cur, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			link, err := root.Readlink(cur)
			if err != nil {
				return "", err
			}
			if path.IsAbs(link) || strings.ContainsAny(link, "\x00\\") {
				return "", fail("LayerPathViolation", "invalid link target")
			}
			depth++
			if depth > 40 {
				return "", fail("LayerPathViolation", "symlink cycle")
			}
			cur = path.Dir(cur)
			pending = append(strings.Split(link, "/"), pending...)
		}
	}
	return cur, nil
}
