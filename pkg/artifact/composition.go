package artifact

import (
	"archive/tar"
	"context"
	"path"
	"slices"
	"strings"
)

type layerTree map[string]Entry
type compiledLayer struct {
	entries []Entry
	nodes   layerTree
}

// resolveTree deliberately preserves ".." until after preceding symlinks have
// been resolved. Cleaning "alias/../file" first changes kernel path semantics.
func resolveTree(nodes layerTree, start, target string) (string, error) {
	if path.IsAbs(target) || strings.ContainsAny(target, "\x00\\") {
		return "", fail("LayerPathViolation", "invalid symlink target")
	}
	cur := start
	pending := strings.Split(target, "/")
	links := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			cur = path.Dir(cur)
			if cur != "app" && !strings.HasPrefix(cur, "app/") {
				return "", fail("LayerPathViolation", "link escapes /app")
			}
			continue
		}
		cur = path.Join(cur, part)
		if _, e := archivePath(cur); e != nil {
			return "", e
		}
		if n, ok := nodes[cur]; ok && n.Type == tar.TypeSymlink {
			links++
			if links > 40 {
				return "", fail("LayerPathViolation", "symlink cycle")
			}
			if path.IsAbs(n.Link) || strings.ContainsAny(n.Link, "\x00\\") {
				return "", fail("LayerPathViolation", "invalid symlink target")
			}
			cur = path.Dir(cur)
			pending = append(strings.Split(n.Link, "/"), pending...)
		}
	}
	return cur, nil
}
func removeTree(nodes layerTree, name string, childrenOnly bool) {
	for p := range nodes {
		if p == name && !childrenOnly || strings.HasPrefix(p, name+"/") {
			delete(nodes, p)
		}
	}
}
func ensureTreeParents(nodes layerTree, name string) error {
	parent := path.Dir(name)
	var dirs []string
	for parent != "." {
		dirs = append(dirs, parent)
		parent = path.Dir(parent)
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		dir := dirs[i]
		if n, ok := nodes[dir]; ok {
			if n.Type != tar.TypeDir {
				return fail("LayerPathViolation", "non-directory layer parent %s", dir)
			}
		} else {
			nodes[dir] = Entry{Path: dir, Type: tar.TypeDir, Mode: 0755}
		}
	}
	return nil
}

// compileLayer applies only this tar's links. An absent parent is an implicit
// upper directory, not a reference into a lower layer. That is what lets one
// immutable extraction serve every image which reuses the same compressed blob.
func compileLayer(info *LayerInfo) (*compiledLayer, error) {
	out := &compiledLayer{nodes: layerTree{}}
	var hardlinks []int
	for _, entry := range info.Entries {
		parent, e := resolveTree(out.nodes, ".", path.Dir(entry.Path))
		if e != nil {
			return nil, e
		}
		entry.Path = path.Join(parent, path.Base(entry.Path))
		if _, e = archivePath(entry.Path); e != nil {
			return nil, e
		}
		if e = ensureTreeParents(out.nodes, entry.Path); e != nil {
			return nil, e
		}
		if _, wh, _ := whiteout(entry.Path); wh {
			out.entries = append(out.entries, entry)
			continue
		}
		if entry.Type == tar.TypeSymlink {
			if _, e = confinedLink(entry.Path, entry.Link, false); e != nil {
				return nil, e
			}
		}
		if entry.Type == tar.TypeLink {
			hardlinks = append(hardlinks, len(out.entries))
		} else {
			if old, ok := out.nodes[entry.Path]; ok && (entry.Type != tar.TypeDir || old.Type != tar.TypeDir) {
				removeTree(out.nodes, entry.Path, false)
			}
			out.nodes[entry.Path] = entry
		}
		out.entries = append(out.entries, entry)
	}
	for _, i := range hardlinks {
		entry := out.entries[i]
		target, e := resolveTree(out.nodes, ".", entry.Link)
		if e != nil {
			return nil, e
		}
		node, ok := out.nodes[target]
		if !ok || node.Type != tar.TypeReg {
			return nil, fail("LayerPathViolation", "hardlink must resolve to a same-layer regular file")
		}
		entry.Link = target
		out.entries[i] = entry
		node.Path = entry.Path
		removeTree(out.nodes, entry.Path, false)
		out.nodes[entry.Path] = node
	}
	if e := validateTreeLinks(out.nodes); e != nil {
		return nil, e
	}
	return out, nil
}
func validateTreeLinks(nodes layerTree) error {
	for name, node := range nodes {
		if node.Type == tar.TypeSymlink {
			if _, e := resolveTree(nodes, path.Dir(name), node.Link); e != nil {
				return e
			}
		}
	}
	return nil
}
func mergeLayerTree(nodes layerTree, layer *compiledLayer) {
	for _, entry := range layer.entries {
		target, wh, opaque := whiteout(entry.Path)
		if wh {
			removeTree(nodes, target, opaque)
		}
	}
	names := make([]string, 0, len(layer.nodes))
	for name := range layer.nodes {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		node := layer.nodes[name]
		if lower, ok := nodes[name]; ok && (node.Type != tar.TypeDir || lower.Type != tar.TypeDir) {
			removeTree(nodes, name, false)
		}
		nodes[name] = node
	}
}

// ValidateComposition models reverse-ordered overlay lowers, including implicit
// upper directories which shadow inherited symlinks, then resolves every visible
// symlink against the composed namespace before a mount can expose it.
func ValidateComposition(ctx context.Context, sources []LayerSource) error {
	nodes := layerTree{}
	for _, source := range sources {
		if source.Descriptor.Annotations["brewlet.sh/layer"] == "launch" {
			continue
		}
		info, e := WalkLayer(ctx, source, nil)
		if e != nil {
			return e
		}
		layer, e := compileLayer(info)
		if e != nil {
			return e
		}
		mergeLayerTree(nodes, layer)
		if e = validateTreeLinks(nodes); e != nil {
			return e
		}
	}
	return nil
}
