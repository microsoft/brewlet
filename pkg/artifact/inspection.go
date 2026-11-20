package artifact

import (
	"context"
	"encoding/json"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// ApplicationReport describes a structurally verified application graph.
// It makes no claim about publisher trust or vulnerability status.
type ApplicationReport struct {
	SchemaVersion int             `json:"schemaVersion"`
	Structural    bool            `json:"structural"`
	Root          v1.Descriptor   `json:"root"`
	Manifest      v1.Descriptor   `json:"manifest"`
	Config        v1.Descriptor   `json:"config"`
	Platform      v1.Platform     `json:"platform"`
	Layers        []v1.Descriptor `json:"layers"`
	Launch        json.RawMessage `json:"launch"`
}

func InspectApplication(ctx context.Context, store Store, target v1.Descriptor, platform v1.Platform) (ApplicationReport, error) {
	if err := VerifyGraph(ctx, store, target); err != nil {
		return ApplicationReport{}, err
	}
	resolved, err := Resolve(ctx, store.Open, target, platform)
	if err != nil {
		return ApplicationReport{}, err
	}
	return ApplicationReport{1, true, target, resolved.ManifestDescriptor, resolved.Manifest.Config,
		resolved.Image.Platform, resolved.Manifest.Layers, resolved.LaunchJSON}, nil
}
