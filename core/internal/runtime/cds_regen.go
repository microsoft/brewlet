// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtime

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Node-side AppCDS regeneration (https://github.com/microsoft/brewlet/blob/main/docs/appcds.md §4.3). The node maintains a
// per-(isolation scope, artifact digest, JDK build) archive cache and launches
// with -XX:+AutoCreateSharedArchive so the archive self-heals on every central
// JDK patch. Each workload sees only its private cache-entry directory; the
// node-global cache root and writer-election markers are never mounted.
const (
	// DefaultCDSCacheDir is the node-local directory the regeneration cache lives
	// in. The provisioner creates it; entries are private hashed directories.
	DefaultCDSCacheDir = "/opt/brewlet/cds"
	// InSandboxCDSDir is where the shim/bundle bind-mounts one private cache entry
	// inside the sandbox.
	InSandboxCDSDir = "/run/brewlet/cds"
	// DefaultWriterTTL bounds how long a writer-election marker is trusted without
	// a heartbeat before the writer is assumed dead and the key is re-elected.
	DefaultWriterTTL = 10 * time.Minute
	// DefaultEvictTTL is the max age (by mtime) a cached archive is kept before a
	// best-effort prune removes it — old JDK-build keys accumulate after patches.
	DefaultEvictTTL = 14 * 24 * time.Hour
	// minRegenFeature is the lowest JDK feature version that ships
	// -XX:+AutoCreateSharedArchive (JDK 19). Below it, emitting the flag would be
	// a *fatal* unrecognized-option error, so regeneration is skipped instead.
	minRegenFeature = 19

	cacheArchiveName   = "archive.jsa"
	writerMarkerSuffix = ".writer"
	writerStateLock    = ".writer-state.lock"
)

// RegenRole is the launch treatment the node chose for an artifact that opted
// into node-side AppCDS regeneration.
type RegenRole string

const (
	// RegenSkip: regeneration does not apply (feature disabled, unsupported JDK,
	// or JDK identity undeterminable). Launch as if no regeneration were set.
	RegenSkip RegenRole = "skip"
	// RegenConsume: a valid cached archive exists for this
	// (scope, artifact, JDK-build); map it read-only with
	// -Xshare:auto -XX:SharedArchiveFile.
	RegenConsume RegenRole = "consume"
	// RegenWrite: this launch was elected to (re)generate the archive; it runs
	// with -XX:+AutoCreateSharedArchive and writes the archive to the node cache
	// at JVM exit (https://github.com/microsoft/brewlet/blob/main/docs/appcds.md §4.3 — the win lands on the next rollout).
	RegenWrite RegenRole = "write"
	// RegenDefer: another launch is already generating this key; run on base CDS
	// this boot and pick up the app archive on a later restart (thundering-herd
	// control).
	RegenDefer RegenRole = "defer"
)

// RegenParams are the host-side inputs to a node-side regeneration decision.
type RegenParams struct {
	// CacheDir is the host cache directory; "" => DefaultCDSCacheDir.
	CacheDir string
	// CacheScope is the isolation boundary for cache sharing. The production shim
	// passes the trusted CRI sandbox namespace; local commands use a fixed local
	// scope.
	CacheScope string
	// JDKRoot is the selected node JDK root, read for build identity + the
	// feature-version gate.
	JDKRoot string
	// ArtifactDigest is the verified resolved platform-manifest digest. It must be
	// stable across replicas and change whenever the artifact changes.
	ArtifactDigest string
	// SeedArchive is the host path of a shipped `.jsa` to seed a fresh cache
	// entry with, or "". A seed gives the very first boot a warm archive;
	// -XX:+AutoCreateSharedArchive transparently recreates it if JDK-stale.
	SeedArchive string
	// WriterOwner is the UID/GID that must own a fresh private entry so a
	// non-root workload can create archive.jsa. Nil keeps the invoking user's
	// ownership (the local run/default-root case).
	WriterOwner *RegenOwner
	// ArchiveArgDir is the directory the JVM sees the archive under (the
	// in-sandbox mount point). "" means the JVM uses the host CacheDir directly
	// (the local `run` path, which is not sandboxed).
	ArchiveArgDir string
	// MetricsDir, when set, receives a best-effort node-local role record the
	// metrics exporter (https://github.com/microsoft/brewlet/blob/main/docs/metrics-exporter.md, Option A) can aggregate.
	MetricsDir string
	// Now is an injectable clock for tests; zero => time.Now().
	Now time.Time
	// LockTTL / EvictTTL override the defaults when non-zero.
	LockTTL  time.Duration
	EvictTTL time.Duration
}

// RegenOwner is the filesystem identity of an elected regeneration writer.
type RegenOwner struct {
	UID int
	GID int
}

// CDSWriterLease identifies the writer-election marker owned by one launch.
// Call StartHeartbeat while the writer workload is active so cache maintenance
// cannot mistake a long-running JVM for a crashed writer.
type CDSWriterLease struct {
	marker string
	token  string
}

// RegenDecision is the outcome of DecideCDSRegen.
type RegenDecision struct {
	Role RegenRole
	// Key is the per-(scope, artifact, JDK-build) cache key, or "" when skipped.
	Key string
	// HostMount is the private host directory that may be bind-mounted into the
	// workload. It is never the node-global cache root.
	HostMount string
	// HostArchive is the archive within HostMount, or "" for skip/defer.
	HostArchive string
	// ArgArchive is the archive path passed to -XX:SharedArchiveFile (the
	// in-sandbox path when ArchiveArgDir is set, else the host path).
	ArgArchive string
	// Args are the CDS launch args to prepend to the JVM argv (nil for
	// skip/defer).
	Args []string
	// MountRW reports whether the cache mount must be writable (writer only).
	MountRW bool
	// WriterLease is set only for an elected writer. Production callers keep it
	// alive for the task lifetime; bundle-only callers may leave the marker to
	// expire naturally if the generated task is never started.
	WriterLease *CDSWriterLease
}

// DecideCDSRegen resolves how a regeneration-enabled artifact should launch on
// this node. Expected host conditions (unsupported JDK, unwritable cache, lost
// election) degrade to RegenSkip/RegenDefer. Missing identity inputs are errors.
func DecideCDSRegen(p RegenParams) (RegenDecision, error) {
	if p.JDKRoot == "" || p.CacheScope == "" || p.ArtifactDigest == "" {
		return RegenDecision{}, fmt.Errorf("cds regen: JDKRoot, CacheScope, and ArtifactDigest are required")
	}
	if p.WriterOwner != nil && (p.WriterOwner.UID < 0 || p.WriterOwner.GID < 0) {
		return RegenDecision{}, fmt.Errorf("cds regen: writer UID/GID must be non-negative")
	}
	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}
	cacheDir := p.CacheDir
	if cacheDir == "" {
		cacheDir = DefaultCDSCacheDir
	}
	lockTTL := p.LockTTL
	if lockTTL <= 0 {
		lockTTL = DefaultWriterTTL
	}
	evictTTL := p.EvictTTL
	if evictTTL <= 0 {
		evictTTL = DefaultEvictTTL
	}

	// JDK build identity + feature gate. Any failure => skip (base CDS).
	feature, buildID, err := readJDKIdentity(p.JDKRoot)
	if err != nil || feature < minRegenFeature {
		return decisionSkip(p), nil
	}

	key := regenKey(p.CacheScope, p.ArtifactDigest, buildID)
	hostMount := filepath.Join(cacheDir, key)
	hostArchive := filepath.Join(hostMount, cacheArchiveName)
	writerMarker := filepath.Join(cacheDir, key+writerMarkerSuffix)
	argArchive := hostArchive
	if p.ArchiveArgDir != "" {
		// Nodes are Linux; join with "/" so the in-sandbox path is correct
		// regardless of the dev OS generating a bundle.
		argArchive = strings.TrimRight(p.ArchiveArgDir, "/") + "/" + cacheArchiveName
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		// Can't manage a cache here — fall back to base CDS.
		return decisionSkip(p), nil
	}
	evictStaleEntries(cacheDir, key, evictTTL, lockTTL, now)

	// A valid cached archive already exists -> consume its private directory
	// read-only. Both the entry and final archive component are checked without
	// following symlinks.
	if _, ok := validCacheArchive(hostMount, hostArchive); ok {
		d := RegenDecision{
			Role:        RegenConsume,
			Key:         key,
			HostMount:   hostMount,
			HostArchive: hostArchive,
			ArgArchive:  argArchive,
			Args:        []string{"-Xshare:auto", "-XX:SharedArchiveFile=" + argArchive},
		}
		recordRegenMetric(p.MetricsDir, key, d.Role, now)
		return d, nil
	}

	// No archive yet: elect a single writer per key; others defer to base CDS.
	if writerLease := claimWriter(writerMarker, now, lockTTL); writerLease != nil {
		// The previous writer controlled the old directory contents. Replace the
		// whole entry before any privileged host-side seed copy, so planted links
		// or special files cannot be followed.
		if err := resetCacheEntry(hostMount, p.WriterOwner); err != nil {
			writerLease.Release()
			return decisionSkip(p), nil
		}
		// Seed a fresh cache entry from the shipped archive if one is available.
		// -XX:+AutoCreateSharedArchive validates it and recreates at exit if the
		// seed is JDK-stale, so seeding is always safe.
		if p.SeedArchive != "" {
			_ = seedArchive(p.SeedArchive, hostArchive, p.WriterOwner)
		}
		d := RegenDecision{
			Role:        RegenWrite,
			Key:         key,
			HostMount:   hostMount,
			HostArchive: hostArchive,
			ArgArchive:  argArchive,
			Args:        []string{"-XX:+AutoCreateSharedArchive", "-XX:SharedArchiveFile=" + argArchive, "-Xshare:auto"},
			MountRW:     true,
			WriterLease: writerLease,
		}
		recordRegenMetric(p.MetricsDir, key, d.Role, now)
		return d, nil
	}

	d := RegenDecision{Role: RegenDefer, Key: key}
	recordRegenMetric(p.MetricsDir, key, d.Role, now)
	return d, nil
}

func decisionSkip(p RegenParams) RegenDecision {
	d := RegenDecision{Role: RegenSkip}
	recordRegenMetric(p.MetricsDir, "", d.Role, timeOrNow(p.Now))
	return d
}

func timeOrNow(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}

// claimWriter atomically becomes the sole generator for a key with O_EXCL. A
// stale marker is removed and then competed for again; refreshing it in place
// would allow multiple reclaimers to believe they won.
func claimWriter(marker string, now time.Time, ttl time.Duration) *CDSWriterLease {
	unlock, err := acquireCDSWriterStateLock(filepath.Join(filepath.Dir(marker), writerStateLock))
	if err != nil {
		return nil
	}
	defer unlock()

	for range 3 {
		f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			tokenBytes := make([]byte, 16)
			if _, err := rand.Read(tokenBytes); err != nil {
				_ = f.Close()
				_ = os.Remove(marker)
				return nil
			}
			token := hex.EncodeToString(tokenBytes)
			_, writeErr := fmt.Fprintf(f, "token=%s\npid=%d\ntime=%s\n", token, os.Getpid(), now.UTC().Format(time.RFC3339))
			closeErr := f.Close()
			if writeErr != nil || closeErr != nil {
				_ = os.Remove(marker)
				return nil
			}
			return &CDSWriterLease{marker: marker, token: token}
		}
		if !errors.Is(err, os.ErrExist) {
			return nil
		}
		fi, statErr := os.Lstat(marker)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			return nil
		}
		if fi.Mode().IsRegular() && now.Sub(fi.ModTime()) <= ttl {
			return nil
		}
		if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil
		}
	}
	return nil
}

// StartHeartbeat refreshes the lease marker until the returned stop function is
// called. Stopping also releases this launch's marker, but never removes a
// marker that another writer replaced after stale recovery.
func (l *CDSWriterLease) StartHeartbeat(ttl time.Duration) func() {
	if l == nil {
		return func() {}
	}
	if ttl <= 0 {
		ttl = DefaultWriterTTL
	}
	interval := ttl / 3
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case now := <-ticker.C:
				if !l.refresh(now) {
					return
				}
			case <-stop:
				return
			}
		}
	}()
	return func() {
		once.Do(func() {
			close(stop)
			<-done
			l.Release()
		})
	}
}

// Release removes this lease's marker when it still belongs to this launch.
func (l *CDSWriterLease) Release() {
	if l == nil {
		return
	}
	unlock, err := acquireCDSWriterStateLock(filepath.Join(filepath.Dir(l.marker), writerStateLock))
	if err != nil {
		return
	}
	defer unlock()
	if !l.matchesMarker() {
		return
	}
	_ = os.Remove(l.marker)
}

func (l *CDSWriterLease) refresh(now time.Time) bool {
	unlock, err := acquireCDSWriterStateLock(filepath.Join(filepath.Dir(l.marker), writerStateLock))
	if err != nil {
		// Reclaimers use the same lock, so a transient lock failure cannot let
		// another writer remove this lease. Retry on the next heartbeat.
		return true
	}
	defer unlock()
	if !l.matchesMarker() {
		return false
	}
	return os.Chtimes(l.marker, now, now) == nil
}

func (l *CDSWriterLease) matchesMarker() bool {
	info, err := os.Lstat(l.marker)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	raw, err := os.ReadFile(l.marker)
	if err != nil {
		return false
	}
	return strings.HasPrefix(string(raw), "token="+l.token+"\n")
}

func resetCacheEntry(entryDir string, owner *RegenOwner) error {
	if err := os.RemoveAll(entryDir); err != nil {
		return err
	}
	if err := os.Mkdir(entryDir, 0o700); err != nil {
		return err
	}
	if owner == nil {
		return nil
	}
	return os.Chown(entryDir, owner.UID, owner.GID)
}

// seedArchive publishes a complete seed atomically into a freshly reset entry.
func seedArchive(src, dst string, owner *RegenOwner) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".seed-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if owner != nil {
		if err := tmp.Chown(owner.UID, owner.GID); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

// evictStaleEntries removes stale private entries, orphan markers, and legacy
// flat cache files. It recognizes only Brewlet's hashed names and never follows
// symlinks.
func evictStaleEntries(cacheDir, keepKey string, ttl, lockTTL time.Duration, now time.Time) {
	unlock, err := acquireCDSWriterStateLock(filepath.Join(cacheDir, writerStateLock))
	if err != nil {
		return
	}
	defer unlock()

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(cacheDir, name)

		if isLegacyCacheName(name) {
			_ = os.RemoveAll(path)
			continue
		}

		if name == keepKey || name == keepKey+writerMarkerSuffix {
			continue
		}

		if _, ok := markerKey(name); ok {
			info, err := os.Lstat(path)
			if err == nil && (!info.Mode().IsRegular() || now.Sub(info.ModTime()) > lockTTL) {
				_ = os.Remove(path)
			}
			continue
		}

		if !isCacheKey(name) {
			continue
		}

		marker := filepath.Join(cacheDir, name+writerMarkerSuffix)
		if info, err := os.Lstat(marker); err == nil &&
			info.Mode().IsRegular() &&
			now.Sub(info.ModTime()) <= lockTTL {
			continue
		}
		archive := filepath.Join(path, cacheArchiveName)
		if info, ok := validCacheArchive(path, archive); ok && now.Sub(info.ModTime()) <= ttl {
			continue
		}
		_ = os.RemoveAll(path)
		_ = os.Remove(marker)
	}
}

func validCacheArchive(entryDir, archive string) (os.FileInfo, bool) {
	dirInfo, err := os.Lstat(entryDir)
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 {
		return nil, false
	}
	info, err := os.Lstat(archive)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		return nil, false
	}
	return info, true
}

func isCacheKey(name string) bool {
	return len(name) == sha256.Size*2 && isLowerHex(name)
}

func markerKey(name string) (string, bool) {
	if !strings.HasSuffix(name, writerMarkerSuffix) {
		return "", false
	}
	key := strings.TrimSuffix(name, writerMarkerSuffix)
	return key, isCacheKey(key)
}

func isLegacyCacheName(name string) bool {
	if strings.HasSuffix(name, ".jsa.writer") {
		return len(strings.TrimSuffix(name, ".jsa.writer")) == 32 &&
			isLowerHex(strings.TrimSuffix(name, ".jsa.writer"))
	}
	if strings.HasSuffix(name, ".jsa") {
		return len(strings.TrimSuffix(name, ".jsa")) == 32 &&
			isLowerHex(strings.TrimSuffix(name, ".jsa"))
	}
	return false
}

func isLowerHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// regenKey derives the per-(scope, artifact, JDK-build) cache key.
func regenKey(cacheScope, artifactDigest, buildID string) string {
	h := sha256.Sum256([]byte(cacheScope + "\x00" + artifactDigest + "\x00" + buildID))
	return hex.EncodeToString(h[:])
}

// readJDKIdentity returns the JDK feature version and a build-identity token for
// the JDK rooted at jdkRoot. It prefers the `release` file (present in every
// standard distribution); when that is missing/unparseable it falls back to
// `bin/java -Xinternalversion`, whose output is exactly the string HotSpot
// records in a CDS archive header. The build token need only *change* on any
// JDK patch (that is what re-keys the cache); AutoCreateSharedArchive itself
// enforces the exact-build match at map time.
func readJDKIdentity(jdkRoot string) (feature int, buildID string, err error) {
	rel := filepath.Join(jdkRoot, "release")
	if kv, rerr := parseReleaseFile(rel); rerr == nil {
		jv := kv["JAVA_VERSION"]
		if f := parseFeature(jv); f > 0 {
			parts := []string{kv["IMPLEMENTOR"], kv["IMPLEMENTOR_VERSION"], firstNonEmpty(kv["JAVA_RUNTIME_VERSION"], jv), kv["OS_ARCH"]}
			return f, strings.Join(parts, "|"), nil
		}
	}
	// Fallback: ask the JVM directly.
	ident, ierr := javaInternalVersion(jdkRoot)
	if ierr != nil {
		return 0, "", ierr
	}
	f := parseFeatureFromInternal(ident)
	if f == 0 {
		return 0, "", fmt.Errorf("cds regen: cannot determine JDK feature for %q", jdkRoot)
	}
	return f, ident, nil
}

func javaInternalVersion(jdkRoot string) (string, error) {
	bin := filepath.Join(jdkRoot, "bin", "java")
	out, err := exec.Command(bin, "-Xinternalversion").Output()
	if err != nil {
		return "", fmt.Errorf("cds regen: %s -Xinternalversion: %w", bin, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// parseReleaseFile reads a JDK `release` file into a map, stripping the
// surrounding double quotes from values (KEY="VALUE" per JEP 220).
func parseReleaseFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	kv := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		kv[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return kv, nil
}

// parseFeature extracts the JDK feature version from a version string such as
// "21.0.5" (=> 21), "1.8.0_412" (=> 8), or "17" (=> 17).
func parseFeature(v string) int {
	v = strings.Trim(strings.TrimSpace(v), `"`)
	if v == "" {
		return 0
	}
	comps := strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '_' || r == '+' || r == '-' })
	if len(comps) == 0 {
		return 0
	}
	if comps[0] == "1" && len(comps) > 1 {
		// Legacy 1.x.y scheme (1.8 => 8).
		if n, err := strconv.Atoi(leadingDigits(comps[1])); err == nil {
			return n
		}
		return 0
	}
	if n, err := strconv.Atoi(leadingDigits(comps[0])); err == nil {
		return n
	}
	return 0
}

// parseFeatureFromInternal pulls the feature version out of a HotSpot
// -Xinternalversion string, which contains a parenthesized version like
// "(21.0.5+11-LTS)".
func parseFeatureFromInternal(s string) int {
	open := strings.IndexByte(s, '(')
	if open < 0 {
		return 0
	}
	rest := s[open+1:]
	end := strings.IndexByte(rest, ')')
	if end < 0 {
		return parseFeature(rest)
	}
	return parseFeature(rest[:end])
}

func leadingDigits(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// SupportsCDSRegen reports whether the JDK at jdkRoot supports node-side AppCDS
// regeneration (-XX:+AutoCreateSharedArchive, JDK feature >= 19). It is a
// convenience wrapper over the identity read DecideCDSRegen performs internally;
// callers that already invoke DecideCDSRegen do not need it. Whether to
// regenerate at all is a deployment decision (the brewlet.sh/cds-regenerate
// annotation / --appcds-regenerate flag), not a property of the artifact.
func SupportsCDSRegen(jdkRoot string) bool {
	feature, _, err := readJDKIdentity(jdkRoot)
	if err != nil {
		return false
	}
	return feature >= minRegenFeature
}
