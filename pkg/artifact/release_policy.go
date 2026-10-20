package artifact

import (
	"fmt"
	"slices"
	"time"

	digest "github.com/opencontainers/go-digest"
)

type ReleaseKey struct {
	PublicKey string    `json:"publicKey"`
	NotBefore time.Time `json:"notBefore"`
	NotAfter  time.Time `json:"notAfter"`
	Revoked   bool      `json:"revoked,omitempty"`
}

type ReleaseAuthority struct {
	Identity        string          `json:"identity"`
	Keys            []ReleaseKey    `json:"keys"`
	Subjects        []digest.Digest `json:"subjects,omitempty"`
	RevokedSubjects []digest.Digest `json:"revokedSubjects,omitempty"`
}

// ReleasePolicy separates publisher authorization from release authorization.
// Public PEMs are embedded; private keys are never part of this document.
type ReleasePolicy struct {
	SchemaVersion int              `json:"schemaVersion"`
	Mode          string           `json:"mode"`
	NotAfter      time.Time        `json:"notAfter"`
	Publisher     ReleaseAuthority `json:"publisher"`
	Composer      ReleaseAuthority `json:"composer"`
}

func ParseReleasePolicy(data []byte) (ReleasePolicy, error) {
	var policy ReleasePolicy
	if err := StrictJSON(data, &policy); err != nil {
		return policy, fail("InvalidReleasePolicy", "%v", err)
	}
	return policy, policy.Validate(time.Now())
}

func (p ReleasePolicy) Validate(now time.Time) error {
	if p.SchemaVersion != 1 || p.Mode != "production" && p.Mode != "development" ||
		p.NotAfter.IsZero() || !now.Before(p.NotAfter) {
		return fail("InvalidReleasePolicy", "version, explicit mode and unexpired policy required")
	}
	for _, authority := range []ReleaseAuthority{p.Publisher, p.Composer} {
		if _, err := authority.trust(now, p.Mode == "production"); err != nil {
			return err
		}
	}
	return nil
}

func (a ReleaseAuthority) trust(now time.Time, required bool) (LayerSetTrust, error) {
	trust := LayerSetTrust{ExpectedIdentity: a.Identity, RequireProvenance: required}
	seen := map[string]bool{}
	for _, candidate := range a.Keys {
		key, err := ParseTrustKey([]byte(candidate.PublicKey))
		if err != nil {
			return trust, fail("InvalidReleasePolicy", "%v", err)
		}
		id, err := signerKeyID(key)
		if err != nil {
			return trust, err
		}
		if seen[id] || candidate.NotBefore.IsZero() || !candidate.NotAfter.After(candidate.NotBefore) {
			return trust, fail("InvalidReleasePolicy", "duplicate key or invalid validity interval")
		}
		seen[id] = true
		if !candidate.Revoked && !now.Before(candidate.NotBefore) && now.Before(candidate.NotAfter) {
			trust.PublicKeys = append(trust.PublicKeys, key)
		}
	}
	for _, d := range append(slices.Clone(a.Subjects), a.RevokedSubjects...) {
		if d.Validate() != nil || d.Algorithm() != digest.SHA256 {
			return trust, fail("InvalidReleasePolicy", "sha256 subject required")
		}
	}
	if (required || len(a.Keys) > 0) && (len(trust.PublicKeys) == 0 || a.Identity == "") {
		return trust, fail("InvalidReleasePolicy", "active keys and explicit identity required")
	}
	return trust, nil
}

func (a ReleaseAuthority) authorize(subject digest.Digest) error {
	if subject.Validate() != nil || subject.Algorithm() != digest.SHA256 ||
		slices.Contains(a.RevokedSubjects, subject) ||
		len(a.Subjects) > 0 && !slices.Contains(a.Subjects, subject) {
		return fail("UnauthorizedReleaseSubject", "%s", subject)
	}
	return nil
}

func authorizeSigner(signer *LayerSetSigner, trust LayerSetTrust) error {
	if signer == nil || signer.Key == nil || signer.Identity != trust.ExpectedIdentity {
		return fmt.Errorf("authorized composer signer and identity required")
	}
	id, err := signerKeyID(&signer.Key.PublicKey)
	if err != nil {
		return err
	}
	for _, key := range trust.PublicKeys {
		candidate, err := signerKeyID(key)
		if err != nil {
			return err
		}
		if id == candidate {
			return nil
		}
	}
	return fmt.Errorf("composer signing key is not authorized by release policy")
}
