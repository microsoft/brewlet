// Package node implements the language-independent node execution contract.
package node

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

var (
	idPattern     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	uidPattern    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,127}$`)
	digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

type Error struct{ Reason, Detail string }

func (e *Error) Error() string            { return e.Reason + ": " + e.Detail }
func failure(reason, detail string) error { return &Error{reason, detail} }
func ValidateID(id string) error {
	if len(id) > 55 || !idPattern.MatchString(id) {
		return failure("InvalidRuntimeImageID", id)
	}
	return nil
}

type Metadata struct {
	ID             string            `json:"id"`
	InventoryUID   string            `json:"inventoryUID"`
	Generation     int64             `json:"generation"`
	SourceDigest   string            `json:"sourceDigest"`
	ManifestDigest string            `json:"manifestDigest"`
	Platform       ocispec.Platform  `json:"platform"`
	Env            []string          `json:"env"`
	UID            uint32            `json:"uid"`
	GID            uint32            `json:"gid"`
	Home           string            `json:"home"`
	Homes          map[uint32]string `json:"homes"`
	ValidatedAt    time.Time         `json:"validatedAt"`
}

func (m Metadata) Validate() error {
	if err := ValidateID(m.ID); err != nil {
		return err
	}
	if !uidPattern.MatchString(m.InventoryUID) || m.Generation < 1 ||
		!digestPattern.MatchString(m.SourceDigest) || !digestPattern.MatchString(m.ManifestDigest) ||
		m.Platform.OS != "linux" || m.Platform.Architecture == "" || m.UID == 0 || m.GID == 0 || m.UID == ^uint32(0) || m.GID == ^uint32(0) {
		return fmt.Errorf("invalid runtime metadata")
	}
	_, err := runtimeEnvironment(m.Env)
	return err
}

type Authorization struct {
	ID                string    `json:"id"`
	InventoryUID      string    `json:"inventoryUID"`
	Generation        int64     `json:"generation"`
	Digest            string    `json:"digest"`
	ProfileUID        string    `json:"profileUID"`
	ProfileGeneration int64     `json:"profileGeneration"`
	ExpiresAt         time.Time `json:"expiresAt"`
	Revoked           bool      `json:"revoked"`
	Install           string    `json:"install"`
	State             string    `json:"state"`
}

type Barrier struct {
	ID           string `json:"id"`
	InventoryUID string `json:"inventoryUID"`
	Generation   int64  `json:"generation"`
}

func ParseBarrier(raw, id string) (*Barrier, error) {
	if raw == "" {
		return nil, nil
	}
	var b Barrier
	if err := strictJSON([]byte(raw), &b); err != nil || b.ID != id || !uidPattern.MatchString(b.InventoryUID) || b.Generation <= 0 {
		return nil, failure("InvalidRuntimeGeneration", "invalid minimum-generation annotation")
	}
	return &b, nil
}

type Lease struct {
	TaskID     string    `json:"taskID"`
	Namespace  string    `json:"namespace"`
	Metadata   Metadata  `json:"runtime"`
	AcquiredAt time.Time `json:"acquiredAt"`
}

type InitialState struct {
	Lease        Lease         `json:"lease"`
	Process      specs.Process `json:"process"`
	LayerRoots   []string      `json:"layerRoots,omitempty"`
	ContentRoots []string      `json:"contentRoots,omitempty"`
}

type EvidenceRequest struct {
	AllRuntime bool   `json:"allRuntime"`
	Lease      Lease  `json:"lease"`
	Path       string `json:"path,omitempty"`
}
type EvidenceResponse struct {
	Gone  bool   `json:"gone"`
	Error string `json:"error,omitempty"`
}

const RevocationsAnnotation = "brewlet.sh/runtime-revocations"

type RevocationReceipt struct {
	InventoryUID           string    `json:"inventoryUID"`
	Generation             int64     `json:"generation"`
	Digest                 string    `json:"digest"`
	ProfileUID             string    `json:"profileUID"`
	ProfileGeneration      int64     `json:"profileGeneration"`
	NodeUID                string    `json:"nodeUID"`
	RevokedAt              time.Time `json:"revokedAt"`
	AuthorizationExpiresAt time.Time `json:"authorizationExpiresAt"`
	RootsCleaned           bool      `json:"rootsCleaned"`
}

func strictJSON(data []byte, v any) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	// The decoder below also rejects unknown fields and trailing documents.
	return decodeJSON(data, v)
}
