// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/microsoft/brewlet/internal/sourcepolicy"
	"go.yaml.in/yaml/v3"
)

type document map[string]json.RawMessage

type jdkSource struct {
	Distribution string `json:"distribution"`
	Feature      int64  `json:"feature"`
	Source       struct {
		Image    string `json:"image"`
		JavaHome string `json:"javaHome"`
	} `json:"source"`
}

type launcherSource struct {
	Name   string `json:"name"`
	Source struct {
		Image string `json:"image"`
		Path  string `json:"path"`
	} `json:"source"`
}

type updateOptions struct {
	profile, file, values, distribution, name, image, path string
	feature                                                int64
	jdk, replace                                           bool
	dryRun                                                 dryRunMode
}

type dryRunMode string

const (
	dryRunClient dryRunMode = "client"
	dryRunServer dryRunMode = "server"
)

func (m *dryRunMode) String() string   { return string(*m) }
func (m *dryRunMode) IsBoolFlag() bool { return true }
func (m *dryRunMode) Set(value string) error {
	switch value {
	case "true", "client":
		*m = dryRunClient
	case "server":
		*m = dryRunServer
	default:
		return fmt.Errorf("--dry-run accepts client or server; omit the flag to apply changes")
	}
	return nil
}

func (u *updateOptions) flags(fs *flag.FlagSet, jdk bool) {
	u.jdk = jdk
	fs.StringVar(&u.profile, "profile", "", "explicit target NodeProfile name")
	fs.StringVar(&u.file, "file", "", "read a NodeProfile YAML/JSON file (requires --dry-run=client)")
	fs.StringVar(&u.values, "values", "", "read complete Helm values (requires --dry-run=client)")
	fs.Var(&u.dryRun, "dry-run", "validate and print without saving: client (default when bare) or server")
	fs.StringVar(&u.image, "image", "", "fully qualified, tagless SHA-256 digest reference")
	fs.BoolVar(&u.replace, "replace", false, "replace a source with the same inventory identity")
	if jdk {
		fs.StringVar(&u.distribution, "distribution", "", "JDK distribution token")
		fs.Int64Var(&u.feature, "feature", 0, "JDK feature version")
		fs.StringVar(&u.path, "java-home", "", "absolute JDK home inside the image")
	} else {
		fs.StringVar(&u.name, "name", "", "optional launcher name (not java)")
		fs.StringVar(&u.path, "path", "", "absolute launcher binary path inside the image")
	}
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func validateName(value string, max int) error {
	if value == "" || len(value) > max {
		return fmt.Errorf("name %q must contain 1-%d characters", value, max)
	}
	for _, part := range strings.Split(value, ".") {
		if len(part) > 63 || !dnsLabel.MatchString(part) {
			return fmt.Errorf("name %q must be a lowercase DNS name", value)
		}
	}
	return nil
}

func validateToken(value string, max int) error {
	if len(value) > max || !dnsLabel.MatchString(value) {
		return fmt.Errorf("%q must be a lowercase DNS label of at most %d characters", value, max)
	}
	return nil
}

func validateSource(image, path string) error {
	if len(image) > 2048 || len(path) > 1024 {
		return fmt.Errorf("source image must be at most 2048 characters and source path at most 1024")
	}
	if err := sourcepolicy.ValidateDigestReference(image); err != nil {
		return err
	}
	return sourcepolicy.ValidateAbsolutePath(path)
}

func (u updateOptions) entry() (string, json.RawMessage, error) {
	if err := validateSource(u.image, u.path); err != nil {
		return "", nil, err
	}
	if u.jdk {
		if err := validateToken(u.distribution, 48); err != nil {
			return "", nil, err
		}
		if u.feature < 1 || u.feature > 2147483647 {
			return "", nil, fmt.Errorf("--feature must be between 1 and 2147483647")
		}
		jdk := jdkSource{Distribution: u.distribution, Feature: u.feature}
		jdk.Source.Image, jdk.Source.JavaHome = u.image, u.path
		raw, err := json.Marshal(jdk)
		return "jdks", raw, err
	}
	if err := validateToken(u.name, 54); err != nil {
		return "", nil, err
	}
	if u.name == "java" {
		return "", nil, fmt.Errorf("java is supplied by each JDK and must not be added as a launcher")
	}
	launcher := launcherSource{Name: u.name}
	launcher.Source.Image, launcher.Source.Path = u.image, u.path
	raw, err := json.Marshal(launcher)
	return "launchers", raw, err
}

func entryIdentity(field string, raw json.RawMessage) (string, error) {
	if field == "jdks" {
		var entry jdkSource
		if err := json.Unmarshal(raw, &entry); err != nil {
			return "", err
		}
		if err := validateToken(entry.Distribution, 48); err != nil {
			return "", err
		}
		if entry.Feature < 1 || entry.Feature > 2147483647 {
			return "", fmt.Errorf("invalid JDK feature %d", entry.Feature)
		}
		return fmt.Sprintf("%s-%d", entry.Distribution, entry.Feature), nil
	}
	var entry launcherSource
	if err := json.Unmarshal(raw, &entry); err != nil {
		return "", err
	}
	if err := validateToken(entry.Name, 54); err != nil {
		return "", err
	}
	if entry.Name == "java" {
		return "", fmt.Errorf("java must not appear in the launcher inventory")
	}
	return entry.Name, nil
}

func updateInventory(spec document, field string, entry json.RawMessage, replace bool) error {
	entries := []json.RawMessage{}
	if raw, exists := spec[field]; exists {
		if err := json.Unmarshal(raw, &entries); err != nil {
			return fmt.Errorf("decode %s: %w", field, err)
		}
	}
	key, err := entryIdentity(field, entry)
	if err != nil {
		return err
	}
	found := false
	seen := map[string]bool{}
	for i, existing := range entries {
		id, err := entryIdentity(field, existing)
		if err != nil {
			return fmt.Errorf("invalid existing %s entry: %w", field, err)
		}
		if seen[id] {
			return fmt.Errorf("duplicate %s entry %q; repair the source configuration first", field, id)
		}
		seen[id] = true
		if id != key {
			continue
		}
		found = true
		var oldValue, newValue any
		if err := json.Unmarshal(existing, &oldValue); err != nil {
			return err
		}
		if err := json.Unmarshal(entry, &newValue); err != nil {
			return err
		}
		oldJSON, err := json.Marshal(oldValue)
		if err != nil {
			return err
		}
		newJSON, err := json.Marshal(newValue)
		if err != nil {
			return err
		}
		if !bytes.Equal(oldJSON, newJSON) && !replace {
			return fmt.Errorf("%s %q already exists with a different source; use --replace deliberately", field, key)
		}
		entries[i] = entry
	}
	if !found {
		entries = append(entries, entry)
	}
	if len(entries) > 32 {
		return fmt.Errorf("%s inventory cannot exceed 32 entries", field)
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	spec[field] = raw
	return nil
}

func readDocument(file string) (document, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var value map[string]any
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode %s: %w", file, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", file, err)
		}
		return nil, fmt.Errorf("%s must contain exactly one YAML/JSON document", file)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("convert %s to JSON: %w", file, err)
	}
	return decodeDocument(data)
}

func decodeDocument(raw []byte) (document, error) {
	var doc document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("expected a non-null object")
	}
	return doc, nil
}

func managedBy(meta metadata) string {
	if meta.Labels["app.kubernetes.io/managed-by"] != "" &&
		!oneOf(strings.ToLower(meta.Labels["app.kubernetes.io/managed-by"]), "brewlet", "kubectl") {
		return meta.Labels["app.kubernetes.io/managed-by"]
	}
	for _, fields := range []map[string]string{meta.Labels, meta.Annotations} {
		for _, key := range []string{
			"meta.helm.sh/release-name", "meta.helm.sh/release-namespace",
			"argocd.argoproj.io/instance", "argocd.argoproj.io/tracking-id",
			"kustomize.toolkit.fluxcd.io/name", "helm.toolkit.fluxcd.io/name",
		} {
			if fields[key] != "" {
				return key + "=" + fields[key]
			}
		}
	}
	if len(meta.OwnerReferences) > 0 {
		return "an owning Kubernetes resource"
	}
	for _, field := range meta.ManagedFields {
		if field.Subresource != "" {
			continue
		}
		var fields document
		// Unknown ownership is not permission to overwrite a profile.
		if err := json.Unmarshal(field.Fields, &fields); err != nil {
			return "unrecognized field ownership"
		}
		if _, ownsSpec := fields["f:spec"]; ownsSpec &&
			!oneOf(field.Manager, "brewlet", "kubectl", "kubectl-client-side-apply", "kubectl-edit", "kubectl-patch") {
			return "field manager " + field.Manager
		}
	}
	return ""
}

func (c *client) updateProfile(u updateOptions) error {
	if err := validateName(u.profile, 253); err != nil {
		return fmt.Errorf("--profile: %w", err)
	}
	if u.file != "" && u.values != "" {
		return fmt.Errorf("--file and --values are mutually exclusive")
	}
	if u.dryRun != dryRunClient && (u.file != "" || u.values != "") {
		return fmt.Errorf("--file and --values require --dry-run=client (or bare --dry-run)")
	}
	field, entry, err := u.entry()
	if err != nil {
		return err
	}
	if u.values != "" {
		return c.updateValues(u, field, entry)
	}
	var doc document
	var live object
	if u.file != "" {
		doc, err = readDocument(u.file)
	} else {
		live, err = c.get(profilesResource, u.profile, "--show-managed-fields=true")
		if err == nil {
			doc, err = decodeDocument(live.raw)
		}
	}
	if err != nil {
		return err
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	var profile object
	if err := json.Unmarshal(raw, &profile); err != nil {
		return fmt.Errorf("decode profile: %w", err)
	}
	if profile.APIVersion != "node.brewlet.sh/v1alpha1" || profile.Kind != "NodeProfile" ||
		profile.Metadata.Name != u.profile || profile.Metadata.Namespace != "" {
		return fmt.Errorf("expected cluster-scoped node.brewlet.sh/v1alpha1 NodeProfile %q", u.profile)
	}
	if profile.Metadata.DeletionTimestamp != "" {
		return fmt.Errorf("profile %q is terminating; refusing to change its inventory", u.profile)
	}
	owner := managedBy(profile.Metadata)
	if u.dryRun != dryRunClient && owner != "" {
		return fmt.Errorf("profile %q is managed by %s; edit its source of truth (use add --dry-run --values FILE for Helm), not the live object", u.profile, owner)
	}
	spec, err := decodeDocument(doc["spec"])
	if err != nil {
		return fmt.Errorf("decode profile spec: %w", err)
	}
	if err := updateInventory(spec, field, entry, u.replace); err != nil {
		return err
	}
	doc["spec"], err = json.Marshal(spec)
	if err != nil {
		return err
	}
	if u.dryRun != dryRunClient {
		if live.Metadata.UID == "" || live.Metadata.ResourceVersion == "" {
			return fmt.Errorf("live profile has no UID/resourceVersion; cannot update safely")
		}
		patch := []struct {
			Op    string `json:"op"`
			Path  string `json:"path"`
			Value any    `json:"value"`
		}{
			{"test", "/metadata/uid", live.Metadata.UID},
			{"test", "/metadata/resourceVersion", live.Metadata.ResourceVersion},
			{"add", "/spec/" + field, spec[field]},
		}
		input, err := json.Marshal(patch)
		if err != nil {
			return err
		}
		patchFile, err := os.CreateTemp("", "brewlet-profile-patch-*.json")
		if err != nil {
			return err
		}
		defer os.Remove(patchFile.Name())
		if _, err := patchFile.Write(input); err != nil {
			patchFile.Close()
			return err
		}
		if err := patchFile.Close(); err != nil {
			return err
		}
		args := []string{"patch", profilesResource, u.profile, "--type=json", "--patch-file", patchFile.Name(), "--field-manager=brewlet", "-o", "json"}
		if u.dryRun == dryRunServer {
			args = append(args, "--dry-run=server")
		}
		result, err := c.kubectl(nil, args...)
		if err != nil {
			return fmt.Errorf("profile update failed (concurrent changes require re-reading and reviewing the profile): %w", err)
		}
		doc, err = decodeDocument(result)
		if err != nil {
			return fmt.Errorf("decode updated profile: %w", err)
		}
	}
	if err := cleanManifest(doc); err != nil {
		return err
	}
	if err := encode(c.out, doc, c.opts.output); err != nil {
		return err
	}
	switch u.dryRun {
	case dryRunServer:
		fmt.Fprintln(c.err, "Server dry run succeeded; no inventory was changed.")
	case dryRunClient:
		fmt.Fprintln(c.err, "Client dry run succeeded; no inventory was changed. Server validation was not performed.")
		if owner != "" {
			fmt.Fprintf(c.err, "Profile is managed by %s. Update its source of truth; for Helm use add --dry-run --values FILE.\n", owner)
		}
	default:
		fmt.Fprintln(c.err, "Profile update accepted; provisioning is asynchronous. Inspect the profile and advertised node inventory before using the new source.")
	}
	return nil
}

func cleanManifest(doc document) error {
	meta, err := decodeDocument(doc["metadata"])
	if err != nil {
		return err
	}
	for _, key := range []string{"uid", "resourceVersion", "generation", "creationTimestamp", "deletionTimestamp", "managedFields", "selfLink", "finalizers"} {
		delete(meta, key)
	}
	if raw, exists := meta["annotations"]; exists && string(raw) != "null" {
		annotations, err := decodeDocument(raw)
		if err != nil {
			return err
		}
		delete(annotations, "kubectl.kubernetes.io/last-applied-configuration")
		meta["annotations"], err = json.Marshal(annotations)
		if err != nil {
			return err
		}
	}
	doc["metadata"], err = json.Marshal(meta)
	delete(doc, "status")
	return err
}

func (c *client) updateValues(u updateOptions, field string, entry json.RawMessage) error {
	doc, err := readDocument(u.values)
	if err != nil {
		return err
	}
	if u.profile == "default" {
		var defaultProfile struct {
			Enabled *bool `json:"enabled"`
		}
		if raw, exists := doc["defaultProfile"]; exists {
			if err := json.Unmarshal(raw, &defaultProfile); err != nil {
				return err
			}
			if defaultProfile.Enabled != nil && !*defaultProfile.Enabled {
				return fmt.Errorf("defaultProfile.enabled is false; edit the enabled profile's source instead")
			}
		}
		spec, err := decodeDocument(doc["provisioner"])
		if err != nil {
			return fmt.Errorf("values must contain the complete provisioner configuration: %w", err)
		}
		if err := updateInventory(spec, field, entry, u.replace); err != nil {
			return err
		}
		doc["provisioner"], err = json.Marshal(spec)
		if err != nil {
			return err
		}
	} else {
		var profiles []document
		if err := json.Unmarshal(doc["profiles"], &profiles); err != nil {
			return fmt.Errorf("values must contain profiles: %w", err)
		}
		found := false
		for _, profile := range profiles {
			var name string
			if err := json.Unmarshal(profile["name"], &name); err != nil {
				return err
			}
			if name == u.profile {
				if found {
					return fmt.Errorf("duplicate profile %q in values", name)
				}
				found = true
				if err := updateInventory(profile, field, entry, u.replace); err != nil {
					return err
				}
			}
		}
		if !found {
			return fmt.Errorf("profile %q is not present in the values file", u.profile)
		}
		doc["profiles"], err = json.Marshal(profiles)
		if err != nil {
			return err
		}
	}
	return encode(c.out, doc, c.opts.output)
}
