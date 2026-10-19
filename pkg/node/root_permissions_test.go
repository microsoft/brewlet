package node

import (
	"os"
	"path/filepath"
	"testing"
)

func runtimeRootFixture(t *testing.T) (string, Metadata) {
	t.Helper()
	root := testDirectory(t)
	for _, name := range []string{"usr/bin", "bin", "etc", "home/app", "var/local"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "etc/passwd"), []byte("app:x:1000:1000:app:/home/app:/bin/sh\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/group"), []byte("app:x:1000:\n"), 0644); err != nil {
		t.Fatal(err)
	}
	m := testMetadata()
	m.Platform.OS = "linux"
	m.Platform.Architecture = "amd64"
	return root, m
}

func TestRuntimeDirectorySGIDAllowedButSetIDRegularFileRejected(t *testing.T) {
	root, m := runtimeRootFixture(t)
	if err := os.Chmod(filepath.Join(root, "var/local"), os.ModeSetgid|0755); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRoot(root, &m); err != nil {
		t.Fatal("directory SGID rejected", err)
	}
	file := filepath.Join(root, "usr/bin/bad")
	if err := os.WriteFile(file, []byte("not executed"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, os.ModeSetuid|0755); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRoot(root, &m); err == nil {
		t.Fatal("setuid regular file accepted")
	}
}

func TestRuntimeRootValidationMatrix(t *testing.T) {
	tests := map[string]func(string, *Metadata) error{
		"duplicate environment": func(_ string, m *Metadata) error {
			m.Env = append(m.Env, "X=duplicate")
			return nil
		},
		"duplicate PATH": func(_ string, m *Metadata) error {
			m.Env = append(m.Env, "PATH=/usr/bin")
			return nil
		},
		"PATH regular file": func(root string, m *Metadata) error {
			m.Env = []string{"PATH=/usr/bin/file"}
			return os.WriteFile(filepath.Join(root, "usr/bin/file"), nil, 0755)
		},
		"PATH into app": func(root string, m *Metadata) error {
			m.Env = []string{"PATH=/alias"}
			if err := os.Mkdir(filepath.Join(root, "app"), 0755); err != nil {
				return err
			}
			return os.Symlink("/app", filepath.Join(root, "alias"))
		},
		"PATH symlink loop": func(root string, m *Metadata) error {
			m.Env = []string{"PATH=/alias"}
			return os.Symlink("/alias", filepath.Join(root, "alias"))
		},
		"PATH symlink root escape": func(root string, m *Metadata) error {
			m.Env = []string{"PATH=/alias"}
			return os.Symlink("../../outside", filepath.Join(root, "alias"))
		},
		"duplicate app account": func(root string, _ *Metadata) error {
			return os.WriteFile(filepath.Join(root, "etc/passwd"),
				[]byte("app:x:1000:1000::/home/app:/bin/sh\napp:x:1001:1000::/home/app:/bin/sh\n"), 0644)
		},
		"duplicate UID": func(root string, _ *Metadata) error {
			return os.WriteFile(filepath.Join(root, "etc/passwd"),
				[]byte("app:x:1000:1000::/home/app:/bin/sh\nother:x:1000:1000::/home/app:/bin/sh\n"), 0644)
		},
		"duplicate group name": func(root string, _ *Metadata) error {
			return os.WriteFile(filepath.Join(root, "etc/group"), []byte("app:x:1000:\napp:x:1001:\n"), 0644)
		},
		"setgid binary": func(root string, _ *Metadata) error {
			p := filepath.Join(root, "usr/bin/tool")
			if err := os.WriteFile(p, nil, 0755); err != nil {
				return err
			}
			return os.Chmod(p, os.ModeSetgid|0755)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			root, m := runtimeRootFixture(t)
			if err := mutate(root, &m); err != nil {
				t.Fatal("fixture setup:", err)
			}
			if err := ValidateRoot(root, &m); err == nil {
				t.Fatal("invalid runtime root accepted")
			}
		})
	}
}

func TestUpstreamRootDoesNotRequireNamedAccountPATHOrEmptyApplicationPrefix(t *testing.T) {
	for name, mutate := range map[string]func(string, *Metadata) error{
		"no PATH": func(_ string, m *Metadata) error { m.Env = nil; return nil },
		"missing search directories": func(_ string, m *Metadata) error {
			m.Env = []string{"PATH=/missing/bin:/usr/bin"}
			return nil
		},
		"numeric identity": func(_ string, m *Metadata) error { m.UID, m.GID = 65532, 65532; return nil },
		"other username": func(root string, _ *Metadata) error {
			return os.WriteFile(filepath.Join(root, "etc/passwd"), []byte("nonroot:x:1000:1000::/home/nonroot:/sbin/nologin\n"), 0644)
		},
		"no passwd":    func(root string, _ *Metadata) error { return os.Remove(filepath.Join(root, "etc/passwd")) },
		"no group":     func(root string, _ *Metadata) error { return os.Remove(filepath.Join(root, "etc/group")) },
		"missing home": func(root string, _ *Metadata) error { return os.Remove(filepath.Join(root, "home/app")) },
		"runtime app file": func(root string, _ *Metadata) error {
			return os.WriteFile(filepath.Join(root, "app"), []byte("hidden"), 0644)
		},
		"runtime app symlink": func(root string, _ *Metadata) error { return os.Symlink("/usr/bin", filepath.Join(root, "app")) },
	} {
		t.Run(name, func(t *testing.T) {
			root, m := runtimeRootFixture(t)
			if err := mutate(root, &m); err != nil {
				t.Fatal(err)
			}
			if err := ValidateRoot(root, &m); err != nil {
				t.Fatal(err)
			}
			if m.UID == 65532 && m.Home != "/tmp" {
				t.Fatal("numeric identity acquired an invented account", m)
			}
		})
	}
}

func TestRuntimePATHCannotTraverseHiddenApplicationPrefix(t *testing.T) {
	for _, target := range []string{"/app", "/app/missing", "/app/../usr/bin"} {
		t.Run(target, func(t *testing.T) {
			root, m := runtimeRootFixture(t)
			if err := os.Symlink("/usr/bin", filepath.Join(root, "app")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(root, "alias")); err != nil {
				t.Fatal(err)
			}
			m.Env = []string{"PATH=/alias"}
			if err := ValidateRoot(root, &m); err == nil {
				t.Fatal("PATH depends on shadowed runtime /app")
			}
		})
	}
}

func TestRuntimePATHResolvesAbsoluteSymlinkWithinRootNotHost(t *testing.T) {
	root, m := runtimeRootFixture(t)
	m.Env = []string{"PATH=/alias"}
	if err := os.Symlink("/usr/bin", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRoot(root, &m); err != nil {
		t.Fatal("valid container-absolute PATH symlink rejected:", err)
	}
}
