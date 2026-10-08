// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/microsoft/brewlet/internal/artifact"
)

// AppCDSTrainingArgs builds the JVM argv (everything after the `java` binary) for
// a self-terminating dynamic-CDS training run of a fat-JAR artifact. It mirrors
// the app-intrinsic launch knobs BuildJVMArgs emits (--enable-preview,
// --add-modules, --add-opens, --add-exports, -D<k>=<v> sorted) so the training
// classloading matches production, then appends
// -XX:ArchiveClassesAtExit=<archivePath> and -jar <jarName>, then trainingArgs
// (workload arguments driving class loading). It deliberately does NOT add the
// -Xshare/-XX:SharedArchiveFile consumption flags — this run PRODUCES the archive.
//
// Fat-JAR (entry.mode "" or "jar") only. Layered class-path / JPMS module CLI
// training is a follow-up; use the Maven brewlet:appcds goal (which stages lib/
// and mods/) for those, or pass a prebuilt archive with --appcds-archive.
func AppCDSTrainingArgs(cfg artifact.JVMConfig, jarName, archivePath string, trainingArgs []string) ([]string, error) {
	return trainingArgsWith(cfg, jarName, "-XX:ArchiveClassesAtExit="+archivePath, trainingArgs)
}

// AOTCacheTrainingArgs is the JDK AOT-cache (JEP 514, JDK 25+) twin of
// AppCDSTrainingArgs: -XX:AOTCacheOutput=<cachePath> alone makes the launcher do
// the training run and then the cache-creating sub-invocation itself.
func AOTCacheTrainingArgs(cfg artifact.JVMConfig, jarName, cachePath string, trainingArgs []string) ([]string, error) {
	return trainingArgsWith(cfg, jarName, "-XX:AOTCacheOutput="+cachePath, trainingArgs)
}

// trainingArgsWith is the shared core: app-intrinsic knobs, outputOption, -jar.
func trainingArgsWith(cfg artifact.JVMConfig, jarName, outputOption string, trainingArgs []string) ([]string, error) {
	switch cfg.Entry.Mode {
	case "", "jar":
	default:
		return nil, fmt.Errorf("training supports fat-JAR (entry.mode=jar) only, got %q; use the Maven brewlet:appcds goal for layered/module training, or ship a prebuilt archive with --appcds-archive", cfg.Entry.Mode)
	}
	var args []string
	if cfg.EnablePreview {
		args = append(args, "--enable-preview")
	}
	if len(cfg.AddModules) > 0 {
		args = append(args, "--add-modules", strings.Join(cfg.AddModules, ","))
	}
	for _, o := range cfg.AddOpens {
		args = append(args, "--add-opens", o)
	}
	for _, x := range cfg.AddExports {
		args = append(args, "--add-exports", x)
	}
	for _, k := range sortedKeys(cfg.SystemProperties) {
		args = append(args, "-D"+k+"="+cfg.SystemProperties[k])
	}
	args = append(args, outputOption, "-jar", jarName)
	args = append(args, trainingArgs...)
	return args, nil
}

// GenerateAppCDSArchive runs a self-terminating dynamic-CDS training JVM and
// writes the resulting archive to outArchive. It stages a canonical-mtime copy of
// jarPath (named after cfg.MainJar, defaulting to app.jar) into a
// scratch directory and runs the training JVM there with that directory as its
// working dir, so the JAR classpath entry is recorded as a bare relative token —
// exactly how the shim presents /app/<jar> at runtime (see CDSModTime and
// https://github.com/microsoft/brewlet/blob/main/docs/appcds.md §4.4). The archive is written directly to outArchive (its parent
// is created if needed); the scratch directory is removed on return.
//
// javaBin is the java executable to invoke. timeout bounds the run: a
// well-behaved fat JAR that exercises its startup path and exits on its own
// should finish well within it; if it does not, that is reported as an error
// (long-running servers need the Maven signal mode, not this CLI path).
func GenerateAppCDSArchive(cfg artifact.JVMConfig, jarPath, javaBin, outArchive string, timeout time.Duration, trainingArgs []string) error {
	return runTraining(cfg, jarPath, javaBin, outArchive, "-XX:ArchiveClassesAtExit=", "brewlet-appcds-", "--appcds (long-running servers: use the Maven brewlet:appcds goal with -Dbrewlet.appcds.mode=signal)", timeout, trainingArgs)
}

// GenerateAOTCache runs a self-terminating JEP 514 AOT-cache training JVM and
// writes the cache to outCache, staging the JAR exactly as GenerateAppCDSArchive
// does. Requires JDK 25+; cache creation needs roughly twice the heap.
func GenerateAOTCache(cfg artifact.JVMConfig, jarPath, javaBin, outCache string, timeout time.Duration, trainingArgs []string) error {
	return runTraining(cfg, jarPath, javaBin, outCache, "-XX:AOTCacheOutput=", "brewlet-aot-", "--aot (long-running servers: use the Maven brewlet:aotcache goal with -Dbrewlet.aotcache.mode=signal)", timeout, trainingArgs)
}

// runTraining is the shared core of the two Generate functions; outputOptionPrefix
// is the -XX option up to and including "=" and scratchPrefix names the temp dir; timeoutHint is the caller-specific advice appended to a timeout error.
func runTraining(cfg artifact.JVMConfig, jarPath, javaBin, outArchive, outputOptionPrefix, scratchPrefix, timeoutHint string, timeout time.Duration, trainingArgs []string) error {
	jarName, err := artifact.MainJarName(cfg)
	if err != nil {
		return err
	}
	absOut, err := filepath.Abs(outArchive)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(absOut), 0o755); err != nil {
		return err
	}
	// A stale archive would make -XX:ArchiveClassesAtExit refuse to overwrite in
	// some builds; start clean so a failed run can't masquerade as success.
	if err := os.Remove(absOut); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	scratch, err := os.MkdirTemp("", scratchPrefix)
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)

	if _, err := StageCDSJar(jarPath, scratch, jarName); err != nil {
		return fmt.Errorf("stage training jar: %w", err)
	}

	jvmArgs, err := trainingArgsWith(cfg, jarName, outputOptionPrefix+absOut, trainingArgs)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, javaBin, jvmArgs...)
	cmd.Dir = scratch
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()

	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("training run did not exit within %s; a fat JAR must self-terminate for %s", timeout, timeoutHint)
	}
	// A dynamic-CDS training JVM writes the archive on any clean shutdown; the
	// app's own exit code is not authoritative, so treat a produced, non-empty
	// archive as success even if the workload returned non-zero.
	if fi, statErr := os.Stat(absOut); statErr == nil && fi.Size() > 0 {
		return nil
	}
	if runErr != nil {
		return fmt.Errorf("training run failed and produced no archive: %w", runErr)
	}
	return fmt.Errorf("training run completed but wrote no archive at %s", absOut)
}
