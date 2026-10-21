// Command notice collects license text from modules linked into selected binaries.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

type module struct {
	Path, Version, Dir string
	Main               bool
	Replace            *module
}

func licenseFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	hasLicense := false
	for _, entry := range entries {
		name := strings.ToLower(entry.Name())
		license := strings.HasPrefix(name, "license") || strings.HasPrefix(name, "copying")
		attribution := strings.HasPrefix(name, "notice") || name == "copyright" || name == "authors"
		if !license && !attribution {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			err = filepath.WalkDir(path, func(p string, d os.DirEntry, e error) error {
				if e != nil {
					return e
				}
				if !d.IsDir() {
					files = append(files, p)
					hasLicense = hasLicense || license
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		} else {
			files = append(files, path)
			hasLicense = hasLicense || license
		}
	}
	if !hasLicense {
		return nil, fmt.Errorf("no license text in %s", dir)
	}
	sort.Strings(files)
	return files, nil
}

func generate(packages []string, goos, goarch string) ([]byte, error) {
	cmd := exec.Command("go", append([]string{"list", "-mod=readonly", "-deps", "-json"}, packages...)...)
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	cmd.Stderr = os.Stderr
	data, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	modules := map[string]module{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var pkg struct{ Module *module }
		if err := decoder.Decode(&pkg); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if pkg.Module == nil || pkg.Module.Main {
			continue
		}
		m := *pkg.Module
		if m.Replace != nil {
			return nil, fmt.Errorf("unregistered replacement for %s", m.Path)
		}
		modules[m.Path] = m
	}
	names := make([]string, 0, len(modules))
	for name := range modules {
		names = append(names, name)
	}
	sort.Strings(names)
	var out bytes.Buffer
	fmt.Fprintf(&out, "Brewlet third-party notices\nTarget: %s/%s\n\n", goos, goarch)
	toolchainLicense, err := os.ReadFile(filepath.Join(runtime.GOROOT(), "LICENSE"))
	if os.IsNotExist(err) {
		// Homebrew installs the upstream license beside libexec, its GOROOT.
		toolchainLicense, err = os.ReadFile(filepath.Join(filepath.Dir(runtime.GOROOT()), "LICENSE"))
	}
	if err != nil || len(bytes.TrimSpace(toolchainLicense)) == 0 {
		return nil, fmt.Errorf("Go toolchain license unavailable: %v", err)
	}
	fmt.Fprintf(&out, "Go runtime %s\n%s\n", runtime.Version(), toolchainLicense)
	for _, name := range names {
		m := modules[name]
		files, err := licenseFiles(m.Dir)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", name, m.Version, err)
		}
		fmt.Fprintf(&out, "\n%s %s\n", name, m.Version)
		for _, file := range files {
			text, err := os.ReadFile(file)
			if err != nil || len(bytes.TrimSpace(text)) == 0 {
				return nil, fmt.Errorf("empty/unavailable license or notice %s: %v", file, err)
			}
			relative, _ := filepath.Rel(m.Dir, file)
			fmt.Fprintf(&out, "--- %s ---\n%s\n", filepath.ToSlash(relative), text)
		}
	}
	return out.Bytes(), nil
}

func main() {
	output := flag.String("output", "", "required output notice path")
	goos := flag.String("goos", runtime.GOOS, "target operating system")
	goarch := flag.String("goarch", runtime.GOARCH, "target architecture")
	flag.Parse()
	if *output == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/notice --output FILE PACKAGE...")
		os.Exit(2)
	}
	data, err := generate(flag.Args(), *goos, *goarch)
	if err == nil {
		err = os.WriteFile(*output, data, 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
