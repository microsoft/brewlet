package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func TestCoreHasNoLanguageProducerOrLaunchVocabulary(t *testing.T) {
	root := ".."
	forbiddenNames := map[string]bool{
		"JavaApplication": true, "SelectedJdk": true, "JVMArgs": true,
		"MainJar": true, "JavaHome": true, "JDKFeature": true,
	}
	forbiddenEnv := map[string]bool{
		"JAVA_HOME": true, "JAVA_TOOL_OPTIONS": true, "JDK_JAVA_OPTIONS": true,
		"PYTHONPATH": true, "NODE_OPTIONS": true, "DOTNET_ROOT": true,
	}
	for _, directory := range []string{"cmd", "pkg", "provisioner", "shim", "kubernetes"} {
		err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if strings.HasPrefix(entry.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			for _, dependency := range file.Imports {
				name, err := strconv.Unquote(dependency.Path.Value)
				if err != nil {
					return err
				}
				if strings.Contains(name, "/maven") || strings.Contains(name, "/java") {
					t.Errorf("%s imports language-specific producer %s", path, name)
				}
			}
			ast.Inspect(file, func(node ast.Node) bool {
				switch value := node.(type) {
				case *ast.Ident:
					if forbiddenNames[value.Name] {
						t.Errorf("%s restores language-specific symbol %s", path, value.Name)
					}
				case *ast.BasicLit:
					if value.Kind != token.STRING {
						break
					}
					text, err := strconv.Unquote(value.Value)
					if err != nil {
						t.Error(err)
						break
					}
					if forbiddenEnv[text] || strings.HasPrefix(text, "-XX:") ||
						strings.HasPrefix(text, "-Xmx") || strings.HasPrefix(text, "-Xms") ||
						text == "--add-opens" || text == "--enable-preview" {
						t.Errorf("%s contains language-specific launch rewriting vocabulary %q", path, text)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestOnlyInventoryPlacementAndRetirementCRDs(t *testing.T) {
	files, err := filepath.Glob("../kubernetes/deploy/crds/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"RuntimeImage": true, "NodeProfile": true, "NodeRetirementEvidence": true}
	if len(files) != len(want) {
		t.Fatalf("unexpected CRD count: %d", len(files))
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var crd ext.CustomResourceDefinition
		if err := yaml.UnmarshalStrict(data, &crd); err != nil {
			t.Fatal(err)
		}
		if !want[crd.Spec.Names.Kind] || crd.Spec.Group != "node.brewlet.sh" || crd.Spec.Scope != ext.ClusterScoped {
			t.Errorf("unexpected workload or non-inventory API in %s", file)
		}
		delete(want, crd.Spec.Names.Kind)
	}
	if len(want) != 0 {
		t.Fatalf("missing expected APIs: %v", want)
	}
}
