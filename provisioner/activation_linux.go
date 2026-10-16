//go:build linux

package provisioner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/microsoft/brewlet/pkg/node"
	"github.com/pelletier/go-toml/v2"
)

type activationRecord struct {
	ProfileUID         string `json:"profileUID"`
	NodeUID            string `json:"nodeUID"`
	OriginalConfig     string `json:"originalConfig"`
	DesiredConfig      string `json:"desiredConfig"`
	DesiredBinary      string `json:"desiredBinary"`
	BinaryFile         string `json:"binaryFile,omitempty"`
	PreviousBinary     string `json:"previousBinary,omitempty"`
	PreviousBinaryFile string `json:"previousBinaryFile,omitempty"`
	RestartRequired    bool   `json:"restartRequired"`
	ConfigChanged      bool   `json:"configChanged"`
	Phase              string `json:"phase"`
}

// HostActivation journals the original host state before installing anything.
// The worker's exclusive writer lock must cover Ensure and Restore.
type HostActivation struct {
	HostRoot         string
	InventoryRoot    string
	SourceBinary     string
	ProfileUID       string
	NodeUID          string
	runHost          func(context.Context, ...string) error
	validateBaseline func(context.Context) error
}

func (a HostActivation) journal() string { return filepath.Join(a.InventoryRoot, ".host") }
func (a HostActivation) config() string {
	return filepath.Join(a.HostRoot, "etc/containerd/config.toml")
}
func (a HostActivation) binary() string {
	return filepath.Join(a.HostRoot, "usr/local/bin/containerd-shim-brewlet-v2")
}
func (a HostActivation) shimFile(record activationRecord) string {
	name := record.BinaryFile
	if name == "" {
		name = "desired-shim"
	}
	return filepath.Join(a.journal(), name)
}
func (a HostActivation) host(ctx context.Context, args ...string) error {
	if a.runHost != nil {
		return a.runHost(ctx, args...)
	}
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	command := []string{"--mount=" + a.HostRoot + "/proc/1/ns/mnt", "--root=" + a.HostRoot + "/proc/1/root", "--wd=/", "--"}
	command = append(command, args...)
	out, err := exec.CommandContext(callCtx, "nsenter", command...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("host %s: %w: %.2048s", args[0], err, out)
	}
	return nil
}
func fileHash(path string) (string, error) {
	if err := trustedFile(path); err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func writeDurable(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".activation-file-")
	if err != nil {
		return err
	}

	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func copyDurable(source, target string, mode os.FileMode) error {
	if err := trustedFile(source); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(target), ".activation-copy-")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	if err = out.Chmod(mode); err == nil {
		_, err = io.Copy(out, in)
	}
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
	if err := os.Rename(out.Name(), target); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(target))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (a HostActivation) save(record activationRecord) error {
	b, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return writeDurable(filepath.Join(a.journal(), "state.json"), b, 0600)
}
func (a HostActivation) load() (activationRecord, error) {
	var record activationRecord
	if err := node.NewStore(a.journal()).Check(); err != nil {
		return record, err
	}
	p := filepath.Join(a.journal(), "state.json")
	if err := trustedFile(p); err != nil {
		return record, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal(b, &record); err != nil {
		return record, err
	}
	if record.ProfileUID != a.ProfileUID || record.NodeUID != a.NodeUID {
		return record, fmt.Errorf("ownership-fence-failed: foreign host activation journal")
	}
	for _, name := range []string{record.BinaryFile, record.PreviousBinaryFile} {
		if name != "" && (filepath.Base(name) != name || name == "." || name == "..") {
			return record, fmt.Errorf("invalid activation binary journal path")
		}
	}
	switch record.Phase {
	case "Prepared", "Applied", "Committed", "Restored":
	default:
		return record, fmt.Errorf("unknown activation transaction phase")
	}
	return record, nil
}
func mapAt(parent map[string]any, key string) map[string]any {
	child, _ := parent[key].(map[string]any)
	if child == nil {
		child = map[string]any{}
		parent[key] = child
	}
	return child
}
func configuredRuntime(original []byte) ([]byte, error) {
	var config map[string]any
	if err := toml.Unmarshal(original, &config); err != nil {
		return nil, err
	}
	version, _ := config["version"].(int64)
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("containerd config version 2 or 3 required")
	}
	plugin := "io.containerd.grpc.v1.cri"
	if version == 3 {
		plugin = "io.containerd.cri.v1.runtime"
	}
	containerd := mapAt(mapAt(mapAt(config, "plugins"), plugin), "containerd")
	runtimes := mapAt(containerd, "runtimes")
	if _, exists := runtimes["brewlet"]; exists {
		return nil, fmt.Errorf("unmanaged brewlet handler cannot be adopted")
	}
	options := map[string]any{}
	if runc, ok := runtimes["runc"].(map[string]any); ok {
		if existing, ok := runc["options"].(map[string]any); ok {
			for k, v := range existing {
				options[k] = v
			}
		}
	}
	runtimes["brewlet"] = map[string]any{
		"runtime_type":    "io.containerd.brewlet.v2",
		"pod_annotations": []string{"brewlet.sh/runtime-image", "brewlet.sh/rotated-to-generation"},
		"options":         options,
	}
	return toml.Marshal(config)
}
func (a HostActivation) prepare(ctx context.Context, policy string, fence func(context.Context) error) (activationRecord, error) {
	var record activationRecord
	if entries, err := os.ReadDir(filepath.Join(a.InventoryRoot, ".staging")); err == nil && len(entries) > 0 {
		return record, fmt.Errorf("unmanaged staging workspace cannot be adopted before activation")
	} else if err != nil && !os.IsNotExist(err) {
		return record, err
	}
	if err := trustedFile(a.config()); err != nil {
		return record, err
	}
	if _, err := os.Lstat(a.binary()); err == nil {
		return record, fmt.Errorf("unmanaged shim binary cannot be adopted")
	} else if !os.IsNotExist(err) {
		return record, err
	}
	if err := node.NewStore(filepath.Dir(a.binary())).Check(); err != nil {
		return record, err
	}
	original, err := os.ReadFile(a.config())
	if err != nil {
		return record, err
	}
	desired := original
	if policy == "validated" {
		desired, err = configuredRuntime(original)
		if err != nil {
			return record, err
		}
	}
	if err := fence(ctx); err != nil {
		return record, err
	}
	stage, err := os.MkdirTemp(a.InventoryRoot, ".activation-")
	if err != nil {
		return record, err
	}
	if err := writeDurable(filepath.Join(stage, "original-config"), original, 0600); err != nil {
		return record, err
	}
	if err := writeDurable(filepath.Join(stage, "desired-config"), desired, 0600); err != nil {
		return record, err
	}
	if err := copyDurable(a.SourceBinary, filepath.Join(stage, "desired-shim"), 0755); err != nil {
		return record, err
	}
	record = activationRecord{ProfileUID: a.ProfileUID, NodeUID: a.NodeUID, ConfigChanged: policy == "validated", RestartRequired: policy == "validated", Phase: "Prepared"}
	record.OriginalConfig, err = fileHash(filepath.Join(stage, "original-config"))
	if err != nil {
		return record, err
	}
	record.DesiredConfig, err = fileHash(filepath.Join(stage, "desired-config"))
	if err != nil {
		return record, err
	}
	record.DesiredBinary, err = fileHash(filepath.Join(stage, "desired-shim"))
	if err != nil {
		return record, err
	}
	b, err := json.Marshal(record)
	if err != nil {
		return record, err
	}
	if err := writeDurable(filepath.Join(stage, "state.json"), b, 0600); err != nil {
		return record, err
	}
	if err := fence(ctx); err != nil {
		return record, err
	}
	if _, err := os.Lstat(a.journal()); !os.IsNotExist(err) {
		return record, fmt.Errorf("activation journal unexpectedly exists")
	}
	if err := os.Rename(stage, a.journal()); err != nil {
		return record, err
	}
	d, err := os.Open(a.InventoryRoot)
	if err != nil {
		return record, err
	}
	err = d.Sync()
	d.Close()
	if err != nil {
		return record, err
	}
	return record, nil
}
func (a HostActivation) Ensure(ctx context.Context, policy string, fence func(context.Context) error) error {
	if fence == nil || a.ProfileUID == "" || a.NodeUID == "" {
		return fmt.Errorf("activation requires UID-bound fresh fence")
	}
	if policy != "validated" && policy != "none" {
		return fmt.Errorf("unsupported containerd restart policy")
	}
	if err := fence(ctx); err != nil {
		return err
	}
	for _, command := range [][]string{{"containerd", "--version"}, {"crictl", "--version"}, {"systemctl", "is-active", "--quiet", "containerd"}} {
		if err := a.host(ctx, command...); err != nil {
			return fmt.Errorf("host activation prerequisite missing: %w", err)
		}
	}
	record, err := a.load()
	if os.IsNotExist(err) {
		record, err = a.prepare(ctx, policy, fence)
	}
	if err != nil {
		return err
	}
	sourceHash, err := fileHash(a.SourceBinary)
	if err != nil {
		return err
	}
	if sourceHash != record.DesiredBinary && record.Phase == "Committed" {
		if hash, err := fileHash(a.binary()); err != nil || hash != record.DesiredBinary {
			return fmt.Errorf("shim upgrade found an externally changed binary")
		}
		if err := fence(ctx); err != nil {
			return err
		}
		name := "shim-" + sourceHash
		if err := copyDurable(a.SourceBinary, filepath.Join(a.journal(), name), 0755); err != nil {
			return err
		}
		record.PreviousBinary = record.DesiredBinary
		record.PreviousBinaryFile = record.BinaryFile
		if record.PreviousBinaryFile == "" {
			record.PreviousBinaryFile = "desired-shim"
		}
		record.DesiredBinary = sourceHash
		record.BinaryFile = name
		record.RestartRequired = policy == "validated"
		record.Phase = "Prepared"
		if err := fence(ctx); err != nil {
			return err
		}
		if err := a.save(record); err != nil {
			return err
		}
	}
	if hash, err := fileHash(a.shimFile(record)); err != nil || hash != record.DesiredBinary {
		return fmt.Errorf("staged shim verification failed")
	}
	if hash, err := fileHash(filepath.Join(a.journal(), "desired-config")); err != nil || hash != record.DesiredConfig {
		return fmt.Errorf("staged containerd config verification failed")
	}
	configHash, err := fileHash(a.config())
	if err != nil {
		return err
	}
	binaryHash, binaryErr := fileHash(a.binary())
	if record.Phase == "Committed" && configHash == record.DesiredConfig && binaryErr == nil && binaryHash == record.DesiredBinary {
		return nil
	}
	if configHash != record.OriginalConfig && configHash != record.DesiredConfig {
		return fmt.Errorf("host config changed outside the activation transaction")
	}
	if policy == "none" && configHash != record.DesiredConfig {
		return fmt.Errorf("none policy cannot change or repair containerd configuration")
	}
	if binaryErr != nil && !os.IsNotExist(binaryErr) {
		return binaryErr
	}
	if binaryErr == nil && binaryHash != record.DesiredBinary && binaryHash != record.PreviousBinary {
		return fmt.Errorf("host shim changed outside the activation transaction")
	}
	if err := fence(ctx); err != nil {
		return err
	}
	if err := copyDurable(a.shimFile(record), a.binary(), 0755); err != nil {
		return err
	}
	if record.RestartRequired {
		if err := fence(ctx); err != nil {
			return err
		}
		if err := copyDurable(filepath.Join(a.journal(), "desired-config"), a.config(), 0644); err != nil {
			return err
		}
	}
	record.Phase = "Applied"
	if err := fence(ctx); err != nil {
		return err
	}
	if err := a.save(record); err != nil {
		return err
	}
	activate := func() error {
		if err := fence(ctx); err != nil {
			return err
		}
		if err := a.host(ctx, "containerd", "config", "dump"); err != nil {
			return err
		}
		if record.ConfigChanged && configHash != record.DesiredConfig {
			if err := fence(ctx); err != nil {
				return err
			}
			if err := a.host(ctx, "systemctl", "restart", "containerd"); err != nil {
				return err
			}
		}
		validate := a.validateBaseline
		if validate == nil {
			validate = (HostBaseline{HostRoot: a.HostRoot}).Validate
		}
		if err := validate(ctx); err != nil {
			return err
		}
		if err := fence(ctx); err != nil {
			return err
		}
		committed := record
		committed.Phase = "Committed"
		committed.PreviousBinary = ""
		committed.PreviousBinaryFile = ""
		if err := a.save(committed); err != nil {
			return err
		}
		record = committed
		return nil
	}
	if err := activate(); err != nil {
		if fenceErr := fence(ctx); fenceErr != nil {
			return fenceErr
		}
		restore := func() error { return a.Restore(ctx, fence) }
		if record.PreviousBinary != "" {
			restore = func() error { return a.restorePrevious(ctx, record, fence) }
		}
		if restoreErr := restore(); restoreErr != nil {
			return fmt.Errorf("activation: %v; rollback blocked: %w", err, restoreErr)
		}
		return fmt.Errorf("activation rolled back: %w", err)
	}
	if sourceHash != record.DesiredBinary {
		return a.Ensure(ctx, policy, fence)
	}
	return nil
}

func (a HostActivation) restorePrevious(ctx context.Context, record activationRecord, fence func(context.Context) error) error {
	previous := filepath.Join(a.journal(), record.PreviousBinaryFile)
	if hash, err := fileHash(previous); err != nil || hash != record.PreviousBinary {
		return fmt.Errorf("previous committed shim backup corrupt")
	}
	if err := fence(ctx); err != nil {
		return err
	}
	if err := copyDurable(previous, a.binary(), 0755); err != nil {
		return err
	}
	if record.RestartRequired {
		if err := fence(ctx); err != nil {
			return err
		}
		if err := a.host(ctx, "systemctl", "restart", "containerd"); err != nil {
			return err
		}
	}
	validate := a.validateBaseline
	if validate == nil {
		validate = (HostBaseline{HostRoot: a.HostRoot}).Validate
	}
	if err := validate(ctx); err != nil {
		return err
	}
	record.DesiredBinary = record.PreviousBinary
	record.BinaryFile = record.PreviousBinaryFile
	record.PreviousBinary = ""
	record.PreviousBinaryFile = ""
	record.Phase = "Committed"
	if err := fence(ctx); err != nil {
		return err
	}
	return a.save(record)
}
func (a HostActivation) Restore(ctx context.Context, fence func(context.Context) error) error {
	if fence == nil {
		return fmt.Errorf("fresh cleanup fence required")
	}
	if err := fence(ctx); err != nil {
		return err
	}
	record, err := a.load()
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	original, err := fileHash(filepath.Join(a.journal(), "original-config"))
	if err != nil {
		return err
	}
	if original != record.OriginalConfig {
		return fmt.Errorf("activation backup config corrupt")
	}
	current, err := fileHash(a.config())
	if err != nil {
		return err
	}
	if current != record.OriginalConfig && current != record.DesiredConfig {
		return fmt.Errorf("refusing to overwrite externally changed containerd config")
	}
	if record.ConfigChanged {
		if err := fence(ctx); err != nil {
			return err
		}
		if err := copyDurable(filepath.Join(a.journal(), "original-config"), a.config(), 0644); err != nil {
			return err
		}
	}
	if hash, err := fileHash(a.binary()); err == nil {
		if hash != record.DesiredBinary && hash != record.PreviousBinary {
			return fmt.Errorf("refusing to remove externally changed shim binary")
		}
		if err := fence(ctx); err != nil {
			return err
		}
		if err := os.Remove(a.binary()); err != nil {
			return err
		}
		if err := syncDir(filepath.Dir(a.binary())); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if record.ConfigChanged {
		if err := fence(ctx); err != nil {
			return err
		}
		if err := a.host(ctx, "systemctl", "restart", "containerd"); err != nil {
			return err
		}
		if err := a.host(ctx, "systemctl", "is-active", "--quiet", "containerd"); err != nil {
			return err
		}
		if err := a.host(ctx, "crictl", "--runtime-endpoint", "unix:///run/containerd/containerd.sock", "info", "-o", "json"); err != nil {
			return err
		}
	}
	if hash, err := fileHash(a.config()); err != nil || hash != record.OriginalConfig {
		return fmt.Errorf("original containerd config restoration not observed")
	}
	if _, err := os.Lstat(a.binary()); !os.IsNotExist(err) {
		return fmt.Errorf("owned shim removal not observed")
	}
	if err := fence(ctx); err != nil {
		return err
	}
	record.Phase = "Restored"
	if err := a.save(record); err != nil {
		return err
	}
	if err := fence(ctx); err != nil {
		return err
	}
	archive := filepath.Join(a.InventoryRoot, fmt.Sprintf(".host-restored-%s-%d", a.ProfileUID, time.Now().UnixNano()))
	if err := os.Rename(a.journal(), archive); err != nil {
		return err
	}
	d, err := os.Open(a.InventoryRoot)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
