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
		"missing PATH": func(_ string, m *Metadata) error { m.Env = []string{"OTHER=value"}; return nil },
		"duplicate environment": func(_ string, m *Metadata) error {
			m.Env = append(m.Env, "X=duplicate")
			return nil
		},
		"duplicate PATH": func(_ string, m *Metadata) error {
			m.Env = append(m.Env, "PATH=/usr/bin")
			return nil
		},
		"missing PATH directory": func(_ string, m *Metadata) error {
			m.Env = []string{"PATH=/missing"}
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
		"app regular file": func(root string, _ *Metadata) error {
			return os.WriteFile(filepath.Join(root, "app"), nil, 0644)
		},
		"app symlink": func(root string, _ *Metadata) error {
			return os.Symlink("/home/app", filepath.Join(root, "app"))
		},
		"app UID mismatch": func(_ string, m *Metadata) error { m.UID++; return nil },
		"app GID mismatch": func(_ string, m *Metadata) error { m.GID++; return nil },
		"missing app account": func(root string, _ *Metadata) error {
			return os.WriteFile(filepath.Join(root, "etc/passwd"), []byte("other:x:1000:1000::/home/app:/bin/sh\n"), 0644)
		},
		"duplicate app account": func(root string, _ *Metadata) error {
			return os.WriteFile(filepath.Join(root, "etc/passwd"),
				[]byte("app:x:1000:1000::/home/app:/bin/sh\napp:x:1001:1000::/home/app:/bin/sh\n"), 0644)
		},
		"duplicate UID": func(root string, _ *Metadata) error {
			return os.WriteFile(filepath.Join(root, "etc/passwd"),
				[]byte("app:x:1000:1000::/home/app:/bin/sh\nother:x:1000:1000::/home/app:/bin/sh\n"), 0644)
		},
		"missing primary group": func(root string, _ *Metadata) error {
			return os.WriteFile(filepath.Join(root, "etc/group"), []byte("other:x:1001:\n"), 0644)
		},
		"ambiguous primary group": func(root string, _ *Metadata) error {
			return os.WriteFile(filepath.Join(root, "etc/group"), []byte("app:x:1000:\nother:x:1000:\n"), 0644)
		},
		"missing home": func(root string, _ *Metadata) error {
			return os.Remove(filepath.Join(root, "home/app"))
		},
		"home regular file": func(root string, _ *Metadata) error {
			if err := os.Remove(filepath.Join(root, "home/app")); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(root, "home/app"), nil, 0644)
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
