package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

type FixtureEntry struct {
	Path   string            `json:"path"`
	Type   string            `json:"type"`
	Mode   int64             `json:"mode"`
	Data   string            `json:"data,omitempty"`
	Link   string            `json:"link,omitempty"`
	Xattrs map[string]string `json:"xattrs,omitempty"`
}
type Fixture struct {
	Name    string           `json:"name"`
	Kind    string           `json:"kind"`
	Launch  json.RawMessage  `json:"launch,omitempty"`
	Image   json.RawMessage  `json:"image,omitempty"`
	Value   string           `json:"value,omitempty"`
	Entries []FixtureEntry   `json:"entries,omitempty"`
	Layers  [][]FixtureEntry `json:"layers,omitempty"`
	Process *Process         `json:"process,omitempty"`
	Reason  string           `json:"reason,omitempty"`
}
type FixtureResult struct {
	Name    string   `json:"name"`
	Process *Process `json:"process,omitempty"`
	Reason  string   `json:"reason,omitempty"`
	Passed  bool     `json:"passed"`
}

// EvaluateFixture is shared by producer and shim conformance tests; non-Go
// producers can invoke "brewlet conformance FILE" rather than duplicate parsing.
func EvaluateFixture(ctx context.Context, f Fixture) (FixtureResult, error) {
	result := FixtureResult{Name: f.Name}
	var err error
	switch f.Kind {
	case "launch":
		var l *Launch
		l, err = ParseLaunch(f.Launch)
		if err == nil {
			result.Process = &l.Process
		}
	case "mirror":
		var l *Launch
		l, err = ParseLaunch(f.Launch)
		if err == nil {
			var image v1.Image
			image, err = ParseImage(f.Image)
			if err == nil {
				err = ValidateMirror(l, image)
			}
		}
	case "id":
		err = ValidateRuntimeImageID(f.Value)
	case "platform":
		_, err = ParsePlatform(f.Value)
	case "compatibility":
		var launch *Launch
		launch, err = ParseLaunch(f.Launch)
		if err == nil && !launch.Allows(f.Value) {
			err = fail("RuntimeImageIncompatible", "runtime image %q excluded by launch allow-list", f.Value)
		}
	case "layer":
		source, e := fixtureSource(f.Entries)
		if e != nil {
			return result, e
		}
		_, err = WalkLayer(ctx, source, nil)
		if err == nil {
			err = ValidateComposition(ctx, []LayerSource{source})
		}
	case "composition":
		var sources []LayerSource
		for _, entries := range f.Layers {
			source, e := fixtureSource(entries)
			if e != nil {
				return result, e
			}
			sources = append(sources, source)
		}
		err = ValidateComposition(ctx, sources)
	default:
		return result, fmt.Errorf("unknown fixture kind %q", f.Kind)
	}
	result.Reason = Reason(err)
	if err != nil && result.Reason == "" {
		return result, err
	}
	result.Passed = result.Reason == f.Reason
	if f.Process != nil && err == nil {
		want, _ := json.Marshal(f.Process)
		got, _ := json.Marshal(result.Process)
		result.Passed = result.Passed && bytes.Equal(want, got)
	}
	return result, nil
}
func fixtureSource(entries []FixtureEntry) (LayerSource, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		typ := byte(tar.TypeReg)
		switch entry.Type {
		case "file":
		case "directory":
			typ = tar.TypeDir
		case "symlink":
			typ = tar.TypeSymlink
		case "hardlink":
			typ = tar.TypeLink
		case "fifo":
			typ = tar.TypeFifo
		default:
			return LayerSource{}, fmt.Errorf("unknown fixture tar type")
		}
		h := &tar.Header{Name: entry.Path, Mode: entry.Mode, Size: int64(len(entry.Data)), Linkname: entry.Link, Typeflag: typ, Xattrs: entry.Xattrs}
		if typ != tar.TypeReg {
			h.Size = 0
		}
		if e := tw.WriteHeader(h); e != nil {
			return LayerSource{}, e
		}
		if typ == tar.TypeReg {
			if _, e := io.WriteString(tw, entry.Data); e != nil {
				return LayerSource{}, e
			}
		}
	}
	if e := tw.Close(); e != nil {
		return LayerSource{}, e
	}
	if e := gz.Close(); e != nil {
		return LayerSource{}, e
	}
	data := buf.Bytes()
	d := v1.Descriptor{MediaType: v1.MediaTypeImageLayerGzip, Digest: digest.FromBytes(data), Size: int64(len(data)), Annotations: map[string]string{"brewlet.sh/layer": "app"}}
	return LayerSource{Descriptor: d, Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }}, nil
}
func RunFixtures(ctx context.Context, data []byte) ([]FixtureResult, error) {
	// The envelope can contain intentionally duplicate JSON in raw launch values.
	var suite struct {
		SchemaVersion int       `json:"schemaVersion"`
		Cases         []Fixture `json:"cases"`
	}
	if err := json.Unmarshal(data, &suite); err != nil {
		return nil, err
	}
	if suite.SchemaVersion != 1 {
		return nil, fmt.Errorf("unsupported fixture suite")
	}
	var result []FixtureResult
	for _, f := range suite.Cases {
		r, e := EvaluateFixture(ctx, f)
		if e != nil {
			return nil, fmt.Errorf("%s: %w", f.Name, e)
		}
		result = append(result, r)
	}
	return result, nil
}
