// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Brewlet-specific bundle assembly shared by the harness (`prepare-bundle`) and
// the production Linux shim's Create() hook. This is the *novel* part of the
// shim: turning an OCI artifact + resource limits into an OCI runtime bundle a
// node-resident JDK can execute. It is deliberately portable (no containerd or
// Linux-only deps) so it builds and is testable on any host.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/microsoft/brewlet/internal/artifact"
	kcruntime "github.com/microsoft/brewlet/internal/runtime"
)

// The webhook carries OCI identity hints on these annotations for diagnostics
// and compatibility. Production execution derives its authoritative target
// digest from containerd and rejects any conflicting digest-bearing hint.
const (
	annArtifactContainer = "brewlet.sh/artifact-container"
	annArtifactRef       = "brewlet.sh/artifact-ref"
	annArtifactDigest    = "brewlet.sh/artifact-digest"
	// annRequestedJDK carries the deployment descriptor's JDK request.
	annRequestedJDK = "brewlet.sh/jdk"
	// annJVMArgs carries the deployment descriptor's jvm.args onto the pod (set
	// by the operator from spec.jvm.args, or by the user on a raw Deployment) as
	// a JSON array of strings. The shim appends them to the Java argv ahead
	// of the entrypoint, so deployment tuning is applied AFTER the artifact's own
	// launch knobs and therefore wins under the JVM's last-wins semantics (§4.2).
	// A JSON array — not a whitespace-joined string — is the wire form so an
	// argument that itself contains spaces (e.g.
	// -XX:OnOutOfMemoryError="kill -9 %p") survives delivery intact.
	annJVMArgs         = "brewlet.sh/jvm-args"
	jdkHomeMetadata    = ".brewlet-java-home"
	jdkActiveInventory = ".brewlet-active"
)

// decodeJVMArgs decodes the brewlet.sh/jvm-args annotation into the extra
// JVM args BuildJVMArgs appends before the entrypoint. An absent or empty
// annotation means "no deployment tuning" and is not an error; anything else
// must be a JSON array of strings, and a malformed value fails the launch
// rather than silently dropping the platform team's tuning.
func decodeJVMArgs(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, fmt.Errorf("annotation %s must be a JSON array of strings: %w", annJVMArgs, err)
	}
	return args, nil
}

// imageConfig describes where a Brewlet image or local artifact lives and the
// deployment/runtime choices used to assemble its bundle.
type imageConfig struct {
	StoreRoot   string  `json:"storeRoot"`   // OCI content store / layout root
	Ref         string  `json:"ref"`         // image/artifact reference
	JDKRootsDir string  `json:"jdkRootsDir"` // e.g. /opt/brewlet/jdks
	CPULimit    string  `json:"cpuLimit"`    // from container.resources.limits.cpu
	MemoryLimit string  `json:"memoryLimit"` // from container.resources.limits.memory
	ProcessUID  *uint32 `json:"processUID,omitempty"`
	ProcessGID  *uint32 `json:"processGID,omitempty"`

	// JDKRequest is the deployment descriptor's "<dist>-<feature>" token or
	// bare feature, carried by brewlet.sh/jdk. Empty means the node default.
	JDKRequest string `json:"jdkRequest,omitempty"`

	// Content-store resolution (production path). When Backend is "containerd",
	// the runnable image manifest is read from containerd's own content store by
	// its authoritative target digest rather than from a Brewlet-local OCI layout.
	Backend        string `json:"backend,omitempty"`        // "" (infer) | "layout" | "containerd"
	ContentRoot    string `json:"contentRoot,omitempty"`    // containerd content root
	ManifestDigest string `json:"manifestDigest,omitempty"` // verified resolved platform-manifest digest
}

func (ic imageConfig) processIdentity() kcruntime.ProcessIdentity {
	identity := kcruntime.DefaultProcessIdentity()
	if ic.ProcessUID != nil {
		identity.UID = *ic.ProcessUID
	}
	if ic.ProcessGID != nil {
		identity.GID = *ic.ProcessGID
	}
	return identity
}

// resolvedArtifact is everything the shim needs after disassembling a Brewlet
// image or local artifact against the node's installed JDK roots.
type resolvedArtifact struct {
	Config              artifact.JVMConfig
	JarHostPath         string   // on-disk path of the JAR payload blob
	ClasspathHostPaths  []string // on-disk paths of the optional classpath layer tars
	ModulepathHostPaths []string // on-disk paths of the optional modulepath layer tars
	CDSHostPath         string   // on-disk path of the optional AppCDS archive blob, or ""
	ManifestDigest      string   // verified digest of the resolved platform manifest
	JDKRoot             string   // selected node-resident userland root (e.g. /opt/brewlet/jdks/temurin-21)
	JDKHome             string   // JDK or jlink runtime within JDKRoot; mounted at /opt/jdk
	Format              string   // native artifact or runnable image
}

// resolveArtifact performs the shared §6.1 steps 1-2b: read the artifact,
// separate the JVM config from the JAR layer (from whichever blob source the
// deployment uses), and select the node-resident JDK the deployment
// descriptor requested. Failures map to NoCompatibleJDK in the real shim.
func resolveArtifact(ic imageConfig) (resolvedArtifact, error) {
	blobs, err := loadArtifactBlobs(ic)
	if err != nil {
		return resolvedArtifact{}, err
	}
	jdkRoot, err := selectJDK(ic.JDKRootsDir, ic.JDKRequest)
	if err != nil {
		return resolvedArtifact{}, err
	}
	jdkHome, err := resolveJDKHome(jdkRoot)
	if err != nil {
		return resolvedArtifact{}, err
	}
	return resolvedArtifact{
		Config:              blobs.Config,
		JarHostPath:         blobs.JarHostPath,
		ClasspathHostPaths:  blobs.ClasspathHostPaths,
		ModulepathHostPaths: blobs.ModulepathHostPaths,
		CDSHostPath:         blobs.CDSHostPath,
		ManifestDigest:      blobs.ManifestDigest,
		JDKRoot:             jdkRoot,
		JDKHome:             jdkHome,
		Format:              blobs.Format,
	}, nil
}

// prepareBundle is the heart of Create(): turn an artifact + limits into an OCI
// runtime bundle that runc can execute. This mirrors §6.1 of the spec and is the
// entrypoint the e2e harness drives directly.
func prepareBundle(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: prepare-bundle <imageConfig.json> <bundleDir>")
	}
	cfgBytes, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	var ic imageConfig
	if err := json.Unmarshal(cfgBytes, &ic); err != nil {
		return err
	}
	bundleDir := args[1]

	ra, err := resolveArtifact(ic)
	if err != nil {
		return err
	}

	res := kcruntime.Resources{CPULimit: ic.CPULimit, MemoryLimit: ic.MemoryLimit}
	if err := kcruntime.GenerateBundleWithIdentity(ra.Config, ra.JDKHome, ra.JarHostPath, ra.ClasspathHostPaths, ra.ModulepathHostPaths, ra.CDSHostPath, bundleDir, res, nil, ic.processIdentity()); err != nil {
		return fmt.Errorf("generate bundle: %w", err)
	}

	fmt.Printf("shim: prepared bundle %s (jdk=%s)\n", bundleDir, ra.JDKRoot)
	fmt.Printf("shim: containerd Start() would now exec: runc run -b %s <container-id>\n", bundleDir)
	return nil
}

// selectJDK finds the node-installed JDK for the descriptor's request. The
// request is a "<dist>-<feature>" token (e.g. "temurin-21", matched exactly), a
// bare feature ("21"), or empty (feature 21). It is supplied by the deployment
// descriptor (brewlet.sh/jdk annotation), not the artifact config.
//
// For a bare feature (no distribution) there is NO built-in vendor preference:
// the node picks the lexically-first installed distribution that provides the
// feature, so selection is deterministic across nodes with the same inventory.
// To pin a distribution, set spec.jvm.distribution (or a "<dist>-<feature>"
// annotation).
func selectJDK(rootsDir, request string) (string, error) {
	dist, feature := parseJDKRequest(request)
	active, err := activeJDKs(rootsDir)
	if err != nil {
		return "", err
	}

	// Explicit distribution: exact match only — never silently fall back to a
	// different vendor's build. The distribution is tenant-controlled annotation
	// input that becomes a directory name under the JDK roots, so it is validated
	// as a safe token before it is joined (CWE-22).
	if dist != "" {
		if err := artifact.ValidateRuntimeToken("JDK distribution", dist, maxJDKDistributionLength); err != nil {
			return "", fmt.Errorf("NoCompatibleJDK: %w", err)
		}
		name := fmt.Sprintf("%s-%d", dist, feature)
		if !active[name] {
			return "", fmt.Errorf("NoCompatibleJDK: JDK %s is not active under %s", name, rootsDir)
		}
		root, err := resolveRuntimeRoot(rootsDir, name, "NoCompatibleJDK")
		if err != nil {
			return "", err
		}
		if _, err := resolveJDKHome(root); err == nil {
			return root, nil
		}
		return "", fmt.Errorf("NoCompatibleJDK: no JDK %s-%d under %s", dist, feature, rootsDir)
	}

	// Bare feature: choose the lexically-first installed distribution for this
	// feature. os.ReadDir returns entries already sorted by name; sort again to
	// keep the guarantee explicit and independent of that detail.
	suffix := fmt.Sprintf("-%d", feature)
	entries, err := os.ReadDir(rootsDir)
	if err != nil {
		return "", fmt.Errorf("NoCompatibleJDK: cannot read JDK roots %s: %w", rootsDir, err)
	}
	var matches []string
	for _, e := range entries {
		name := e.Name()
		if !active[name] {
			continue
		}
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		root, err := resolveRuntimeRoot(rootsDir, name, "NoCompatibleJDK")
		if err != nil {
			continue
		}
		if _, err := resolveJDKHome(root); err == nil {
			matches = append(matches, name)
		}
	}
	sort.Strings(matches)
	if len(matches) > 0 {
		return resolveRuntimeRoot(rootsDir, matches[0], "NoCompatibleJDK")
	}
	return "", fmt.Errorf("NoCompatibleJDK: no JDK feature %d under %s", feature, rootsDir)
}

// maxJDKDistributionLength bounds a requested JDK distribution, matching the
// NodeProfile validator's limit for spec.jdks[].distribution
// (kubernetes/internal/controller/nodeprofile_validate.go), so the shim accepts
// exactly the distributions an administrator can declare.
const maxJDKDistributionLength = 48

func activeJDKs(rootsDir string) (map[string]bool, error) {
	active, err := activeRuntimeInventory(rootsDir, jdkActiveInventory, "NoCompatibleJDK")
	if err != nil {
		return nil, err
	}
	if active == nil {
		return nil, fmt.Errorf("NoCompatibleJDK: active inventory missing under %s", rootsDir)
	}
	return active, nil
}

func activeRuntimeInventory(rootsDir, inventory, reason string) (map[string]bool, error) {
	data, err := os.ReadFile(filepath.Join(rootsDir, inventory))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: read active inventory under %s: %w", reason, rootsDir, err)
	}
	active := make(map[string]bool)
	for _, name := range strings.Fields(string(data)) {
		active[name] = true
	}
	return active, nil
}

func resolveJDKHome(root string) (string, error) {
	home := root
	data, err := os.ReadFile(filepath.Join(root, jdkHomeMetadata))
	switch {
	case err == nil:
		inRoot := strings.TrimSpace(string(data))
		if inRoot == "" || !filepath.IsAbs(inRoot) || filepath.Clean(inRoot) != inRoot || inRoot == string(filepath.Separator) {
			return "", fmt.Errorf("NoCompatibleJDK: invalid %s in %s", jdkHomeMetadata, root)
		}
		home = filepath.Join(root, strings.TrimPrefix(inRoot, string(filepath.Separator)))
	case !os.IsNotExist(err):
		return "", fmt.Errorf("NoCompatibleJDK: read %s in %s: %w", jdkHomeMetadata, root, err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("NoCompatibleJDK: resolve root %s: %w", root, err)
	}
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", fmt.Errorf("NoCompatibleJDK: resolve Java home %s: %w", home, err)
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedHome)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("NoCompatibleJDK: Java home %s resolves outside %s", home, root)
	}
	if _, err := os.Stat(filepath.Join(resolvedHome, "bin", "java")); err != nil {
		return "", fmt.Errorf("NoCompatibleJDK: java missing from %s: %w", resolvedHome, err)
	}
	return resolvedHome, nil
}

// parseJDKRequest splits a brewlet.sh/jdk request into distribution and feature.
// Accepted forms: "<dist>-<feature>" (e.g. "temurin-21"), a bare feature ("21"),
// or "" (no distribution, feature 21). An unparseable feature defaults to 21.
// An empty distribution means "any installed distribution of this feature"
// (selectJDK picks the lexically-first one).
func parseJDKRequest(request string) (dist string, feature int) {
	feature = 21
	request = strings.TrimSpace(request)
	if request == "" {
		return "", feature
	}
	if i := strings.LastIndex(request, "-"); i > 0 && i < len(request)-1 {
		if n, err := strconv.Atoi(request[i+1:]); err == nil && n > 0 {
			return request[:i], n
		}
	}
	if n, err := strconv.Atoi(request); err == nil && n > 0 {
		return "", n
	}
	return "", feature
}

// resolveRuntimeRoot joins a validated runtime name onto a roots directory and
// proves the result is a direct child of that directory after symlink
// resolution. Name validation alone is not enough: a symlinked entry inside the
// roots directory would otherwise redirect a privileged bind-mount or overlay
// lower layer at an arbitrary host path. reason prefixes the error so failures
// surface as the NoCompatibleJDK pod events.
func resolveRuntimeRoot(rootsDir, name, reason string) (string, error) {
	resolvedRoots, err := filepath.EvalSymlinks(rootsDir)
	if err != nil {
		return "", fmt.Errorf("%s: resolve roots %s: %w", reason, rootsDir, err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(filepath.Join(rootsDir, name))
	if err != nil {
		return "", fmt.Errorf("%s: %q not installed under %s", reason, name, rootsDir)
	}
	rel, err := filepath.Rel(resolvedRoots, resolvedRoot)
	if err != nil || rel != name {
		return "", fmt.Errorf("%s: %q resolves outside %s", reason, name, rootsDir)
	}
	return resolvedRoot, nil
}

// containedIn reports whether path is root itself or lies beneath it. Both
// arguments must already be symlink-resolved.
func containedIn(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
