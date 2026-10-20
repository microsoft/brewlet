package artifact

import (
	"context"
	"encoding/json"
	"fmt"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

type DependencyReport struct {
	Verification CompositionInspection `json:"verification"`
	Packages     []LockEntry           `json:"packages"`
	SBOM         json.RawMessage       `json:"sbom"`
}

// InspectDependencies exports facts for external scanners, not vulnerability
// verdicts. The SBOM is the exact document covered by the publisher's evidence.
func InspectDependencies(ctx context.Context, store Store, target v1.Descriptor, platform v1.Platform, policy ReleasePolicy) (DependencyReport, error) {
	var result DependencyReport
	inspection, err := VerifyComposition(ctx, store, target, platform, policy)
	if err != nil {
		return result, err
	}
	result.Verification = inspection
	raw, err := store.Read(inspection.Predicate.DependencySet)
	if err != nil {
		return result, err
	}
	var manifest v1.Manifest
	if err = StrictJSON(raw, &manifest); err != nil {
		return result, err
	}
	foundLock := false
	for _, descriptor := range manifest.Layers {
		if descriptor.Digest == inspection.Predicate.DependencyEvidence.LockDigest {
			data, err := store.Read(descriptor)
			if err != nil {
				return result, err
			}
			var lock LayerSetLock
			if err = StrictJSON(data, &lock); err != nil {
				return result, err
			}
			result.Packages = lock.Entries
			foundLock = true
		}
	}
	refs, err := store.Referrers(inspection.Predicate.DependencySet)
	if err != nil {
		return result, err
	}
	for _, ref := range refs {
		if ref.ArtifactType != MediaTypeCycloneDX {
			continue
		}
		data, document, err := readReferrer(store, inspection.Predicate.DependencySet, ref, MediaTypeCycloneDX, MediaTypeCycloneDX)
		if err != nil {
			return result, err
		}
		if document.Digest != inspection.Predicate.DependencyEvidence.SBOMDigest {
			return result, fmt.Errorf("SBOM binding changed")
		}
		result.SBOM = data
	}
	if !foundLock || len(result.SBOM) == 0 {
		return result, fmt.Errorf("complete managed dependency evidence unavailable")
	}
	return result, nil
}
