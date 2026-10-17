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

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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

func TestSharedRuntimeClassRetainsSelectorAndLanguageNeutralOverhead(t *testing.T) {
	for _, path := range []string{
		"../kubernetes/deploy/runtimeclass.yaml",
		"../kubernetes/charts/brewlet/templates/runtimeclass.yaml",
	} {
		t.Run(path, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var class nodev1.RuntimeClass
			if err := yaml.UnmarshalStrict(data, &class); err != nil {
				t.Fatal(err)
			}
			if class.APIVersion != "node.k8s.io/v1" || class.Kind != "RuntimeClass" ||
				class.Name != "brewlet" || class.Handler != "brewlet" {
				t.Fatal("runtime selection no longer uses the shared brewlet class/handler", class)
			}
			if class.Scheduling == nil || len(class.Scheduling.NodeSelector) != 1 ||
				class.Scheduling.NodeSelector["brewlet.sh/runtime"] != "ready" ||
				len(class.Scheduling.Tolerations) != 0 {
				t.Fatal("shared RuntimeClass selector or startup-fence behavior changed", class.Scheduling)
			}
			if class.Overhead == nil || len(class.Overhead.PodFixed) != 2 {
				t.Fatal("shared shim/staging overhead missing or runtime-specific", class.Overhead)
			}
			for name, value := range map[corev1.ResourceName]string{
				corev1.ResourceCPU: "50m", corev1.ResourceMemory: "64Mi",
			} {
				actual := class.Overhead.PodFixed[name]
				if actual.Cmp(resource.MustParse(value)) != 0 {
					t.Fatalf("shared %s overhead: got %s, want %s", name, actual.String(), value)
				}
			}
		})
	}
}
