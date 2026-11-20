// Package artifact defines Brewlet application artifacts.
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
// Libraries are ordinary application payload files in app layers.
// Referrers are discovered through OCI layout indexes, the paginated registry
// Referrers API, or paginated deterministic per-referrer digest tags.
//
// VerifyGraph and the push CLI command are byte-preserving OCI transport,
// not Brewlet semantic validators. They can publish ordinary approved-runtime
// inputs and negative conformance fixtures. InspectApplication and runtime
// consumers independently enforce the launch and layer contract.
package artifact
