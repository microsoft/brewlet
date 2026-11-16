package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/microsoft/brewlet/pkg/artifact"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

const layerRoleAnnotation = "brewlet.sh/layer"

type platformInspection struct {
	Platform *v1.Platform    `json:"platform"`
	Launch   json.RawMessage `json:"launch"`
	Layers   []v1.Descriptor `json:"layers"`
}

type imageInspection struct {
	Digest    string               `json:"digest"`
	Platforms []platformInspection `json:"platforms"`
}

type platformDetail struct {
	Digest string          `json:"digest"`
	Launch json.RawMessage `json:"launch"`
	Layers []v1.Descriptor `json:"layers"`
}

type layerSetInspection struct {
	Digest   string                    `json:"digest"`
	Config   json.RawMessage           `json:"layerSet"`
	Lock     json.RawMessage           `json:"lock,omitempty"`
	Layers   []artifact.BuiltLayer     `json:"layers"`
	Evidence artifact.LayerSetEvidence `json:"evidence"`
}

func renderImage(w io.Writer, format, digest string, platforms []platformInspection) error {
	if format == outputJSON {
		return writeJSON(w, imageInspection{digest, platforms})
	}
	fmt.Fprintf(w, "Image %s\n", digest)
	for _, p := range platforms {
		fmt.Fprintf(w, "\n%s\n", platformLabel(p.Platform))
		if err := writePlatformBody(w, p.Launch, p.Layers); err != nil {
			return err
		}
	}
	return nil
}

// renderPlatform prints one resolved platform (inspect --platform).
func renderPlatform(w io.Writer, format string, detail platformDetail, platform *v1.Platform) error {
	if format == outputJSON {
		return writeJSON(w, detail)
	}
	fmt.Fprintf(w, "Image %s\n\n%s\n", detail.Digest, platformLabel(platform))
	return writePlatformBody(w, detail.Launch, detail.Layers)
}

func writePlatformBody(w io.Writer, launch json.RawMessage, layers []v1.Descriptor) error {
	fmt.Fprintln(w, "  Launch:")
	fmt.Fprintln(w, indentJSON(launch, "    "))
	fmt.Fprintln(w, "  Layers:")
	rows := make([][]string, 0, len(layers))
	for _, l := range layers {
		rows = append(rows, []string{orDash(l.Annotations[layerRoleAnnotation]), humanSize(l.Size), l.Digest.String()})
	}
	return writeTable(w, "    ", []string{"ROLE", "SIZE", "DIGEST"}, rows)
}

func renderLayerSet(w io.Writer, format string, inspection layerSetInspection) error {
	if format == outputJSON {
		return writeJSON(w, inspection)
	}
	fmt.Fprintf(w, "Layer set %s\n", inspection.Digest)
	var config artifact.LayerSetConfig
	if json.Unmarshal(inspection.Config, &config) == nil {
		if err := writeFields(w, "  ",
			field{"Name", config.Name},
			field{"Version", config.Version},
			field{"Ecosystem", config.Ecosystem}); err != nil {
			return err
		}
	}
	fmt.Fprintln(w, "\nLayers:")
	rows := make([][]string, 0, len(inspection.Layers))
	for _, l := range inspection.Layers {
		rows = append(rows, []string{orDash(l.Descriptor.Annotations[layerRoleAnnotation]), humanSize(l.Descriptor.Size), l.Descriptor.Digest.String()})
	}
	if err := writeTable(w, "  ", []string{"ROLE", "SIZE", "DIGEST"}, rows); err != nil {
		return err
	}
	var lock artifact.LayerSetLock
	if len(inspection.Lock) > 0 && json.Unmarshal(inspection.Lock, &lock) == nil {
		fmt.Fprintf(w, "\nPackages (%d):\n", len(lock.Entries))
		rows = rows[:0]
		for _, e := range lock.Entries {
			name := e.Name
			if e.Group != "" {
				name = e.Group + ":" + name
			}
			rows = append(rows, []string{e.Ecosystem, name, e.Version})
		}
		if err := writeTable(w, "  ", []string{"ECOSYSTEM", "NAME", "VERSION"}, rows); err != nil {
			return err
		}
	}
	e := inspection.Evidence
	fmt.Fprintln(w, "\nEvidence:")
	return writeFields(w, "  ",
		field{"Manifest", e.ManifestDigest.String()},
		field{"Config", e.ConfigDigest.String()},
		field{"Lock", e.LockDigest.String()},
		field{"SBOM", e.SBOMDigest.String()},
		field{"Attestation", e.AttestationDigest.String()})
}
