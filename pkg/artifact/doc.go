// Package artifact defines Brewlet 2 application and layer-set artifacts.
//
// Producers supply explicit launch bytes, platforms and source/destination
// mappings. Build preserves launch bytes and source modes/mtimes, rejects all
// source xattrs, and uses sorted root-owned tar entries and fixed compression.
// ParseLaunch, ParseImage, ValidateMirror and RunFixtures are the shared wire
// validators. Resolve accepts a local BlobOpener and never contacts registries.
//
// MaterializeLayers provides portable flattened composition in a new destination.
// LayerCache provides digest-and-policy-keyed persistent overlay lower directories;
// Linux whiteout conversion requires the corresponding host privileges. Callers
// run materializers as root to obtain ownership 0:0, validate cache-parent
// ownership, and own durable cache leases and mount-aware collection. The package
// does not implement node cache GC, CRI accounting, or runtime authorization.
//
// Each layer is extracted independently. Its implicit upper directories shadow
// inherited directory symlinks, matching the reverse-ordered overlay lowers.
// Visible link targets are checked against the composed namespace without
// lexically collapsing ".." before preceding symlinks are resolved.
//
// Every layer set has exactly one CycloneDX 1.5 SBOM referrer. With an explicit
// lock, components mirror its ecosystem/name/version/path/hash and optional
// author-supplied group/purl. Without a lock, components are file inventories,
// not inferred dependency identities. Maven owns its BOM/GAV graph and supplies
// its group and Maven package URL explicitly; generic code never detects Java.
//
// Optional layer-set provenance uses ECDSA-P256 DSSE and an in-toto Statement v1.
// Its predicate type is LayerSetPredicateType and LayerSetPredicate binds the
// manifest, config, ordered layer/diff-ID pairs, optional lock, SBOM blob and
// builder identity. Presence of any provenance requires explicit public keys and
// expected signer identity; at least one complete candidate must validate.
// Referrers are discovered through OCI layout indexes, the paginated registry
// Referrers API, or paginated deterministic per-referrer digest tags.
package artifact
