package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMissingLicenseFailsEvenWithNotice(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "NOTICE"), []byte("attribution"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := licenseFiles(dir); err == nil {
		t.Fatal("notice without license accepted")
	}
}

func TestLicenseAndNoticeCollectedDeterministically(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"NOTICE", "LICENSE", "README.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, err := licenseFiles(dir)
	if err != nil || len(files) != 2 || filepath.Base(files[0]) != "LICENSE" || filepath.Base(files[1]) != "NOTICE" {
		t.Fatalf("files=%v error=%v", files, err)
	}
}
