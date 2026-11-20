package main

import (
	"encoding/json"
	"fmt"
	"io"

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
