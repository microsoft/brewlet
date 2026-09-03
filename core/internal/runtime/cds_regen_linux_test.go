// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

const (
	cdsWriterHelperEnv   = "BREWLET_TEST_CDS_WRITER_PATH"
	cdsWriterHelperToken = "brewlet-cds-writer-helper"
)

func TestCDSWriterHelperProcess(t *testing.T) {
	path := os.Getenv(cdsWriterHelperEnv)
	if path == "" || len(os.Args) == 0 || os.Args[len(os.Args)-1] != cdsWriterHelperToken {
		return
	}
	if err := os.WriteFile(path, []byte("archive"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestDecideCDSRegenWriterCanWriteAsProcessIdentity(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to exercise a distinct sandbox UID/GID")
	}
	cache, err := os.MkdirTemp("/tmp", "brewlet-cds-writer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(cache) })
	if err := os.Chmod(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	identity := ProcessIdentity{UID: DefaultProcessUID, GID: DefaultProcessGID}
	owner := RegenOwner{UID: int(identity.UID), GID: int(identity.GID)}
	dec, err := DecideCDSRegen(RegenParams{
		CacheDir:       cache,
		CacheScope:     "local",
		JDKRoot:        fakeJDK(t, "21.0.5"),
		ArtifactDigest: "sha256:non-root-writer",
		WriterOwner:    &owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Role != RegenWrite {
		t.Fatalf("role = %q, want write", dec.Role)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestCDSWriterHelperProcess$", "--", cdsWriterHelperToken)
	cmd.Env = append(os.Environ(), cdsWriterHelperEnv+"="+dec.HostArchive)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: identity.UID, Gid: identity.GID},
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sandbox identity could not write cache archive: %v\n%s", err, output)
	}
}

func TestDecideCDSRegenDoesNotFollowWorkloadSymlink(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to detect unintended ownership changes")
	}
	cache, err := os.MkdirTemp("/tmp", "brewlet-cds-symlink-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(cache) })
	if err := os.Chmod(cache, 0o755); err != nil {
		t.Fatal(err)
	}

	identity := ProcessIdentity{UID: DefaultProcessUID, GID: DefaultProcessGID}
	owner := RegenOwner{UID: int(identity.UID), GID: int(identity.GID)}
	params := RegenParams{
		CacheDir:       cache,
		CacheScope:     "local",
		JDKRoot:        fakeJDK(t, "21.0.5"),
		ArtifactDigest: "sha256:symlink",
		WriterOwner:    &owner,
		LockTTL:        time.Minute,
	}
	first, err := DecideCDSRegen(params)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(cache, "host-target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, first.HostArchive); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(filepath.Join(cache, first.Key+writerMarkerSuffix), old, old); err != nil {
		t.Fatal(err)
	}

	second, err := DecideCDSRegen(params)
	if err != nil {
		t.Fatal(err)
	}
	if second.Role != RegenWrite {
		t.Fatalf("role = %q, want write after removing a non-regular archive entry", second.Role)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if stat.Uid != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("symlink target changed to uid=%d mode=%#o", stat.Uid, info.Mode().Perm())
	}
}
