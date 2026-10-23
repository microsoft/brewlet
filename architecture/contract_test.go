package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
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

func TestProductionBuildersKeepCompilerCacheOutsideSnapshots(t *testing.T) {
	for _, name := range []string{"kubernetes", "provisioner"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", name, "Dockerfile"))
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			if !strings.Contains(text, "RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath") {
				t.Fatal("target-aware compilation must keep its cache outside source snapshots")
			}
			if !strings.Contains(text, "FROM --platform=$BUILDPLATFORM golang:") ||
				!strings.Contains(text, "go run ./scripts/notice --goos $TARGETOS --goarch $TARGETARCH") {
				t.Fatal("native builders must generate notices for the selected target, not execute a cross-built generator")
			}
		})
	}
}

func TestCIKeepsNativeResolverCacheIsolatedAndExportsOnlyPublicResults(t *testing.T) {
	contracts, err := os.ReadFile("../.github/workflows/v2-ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	native, err := os.ReadFile("../acceptance/native_contracts.py")
	if err != nil {
		t.Fatal(err)
	}
	producers, err := os.ReadFile("../acceptance/run_check.py")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contracts), "python3 acceptance/run_check.py native-contracts") ||
		!strings.Contains(string(producers), `"command": ["python3", "acceptance/native_contracts.py"]`) ||
		!strings.Contains(string(native), `"/var/lib/brewlet/layers:rw,exec,size=64m"`) ||
		!strings.Contains(string(native), `re.search(r"--- SKIP:", output)`) ||
		!strings.Contains(string(native), "validate_output(run(suite_command(") {
		t.Fatal("native resolver suite lacks an isolated writable cache or mandatory zero-skip gate")
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Uses string
				Run  string
				With map[string]string
			}
		}
	}
	data, err := os.ReadFile("../.github/workflows/e2e.yml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	runner, uploaded := false, false
	for _, job := range workflow.Jobs {
		jobRunner, jobUpload := false, false
		for _, step := range job.Steps {
			jobRunner = jobRunner || strings.Contains(step.Run, "python3 acceptance/qualify.py --run")
			path, ok := step.With["path"]
			if !ok {
				continue
			}
			if strings.HasPrefix(step.Uses, "actions/cache@") {
				for _, private := range []string{"integration-tests", "acceptance-export", "run-pr-", "work"} {
					if strings.Contains(path, private) {
						t.Fatalf("real-node workflow caches private observations: %s", path)
					}
				}
				continue
			}
			if path != "acceptance-export/*.json" {
				t.Fatalf("real-node workflow exposes private observations: %s", path)
			}
			jobUpload = true
		}
		if jobRunner {
			runner = true
			uploaded = uploaded || jobUpload
		}
	}
	qualification, err := os.ReadFile("../acceptance/qualify.py")
	if err != nil {
		t.Fatal(err)
	}
	if !runner || !uploaded || !strings.Contains(string(qualification), `"activation.py"`) ||
		!strings.Contains(string(qualification), "json.dumps(export(result)") {
		t.Fatal("real-node CI omits verified lifecycle companion or public-only result export")
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
			if strings.Contains(path, "/charts/") {
				if _, err := exec.LookPath("helm"); err != nil {
					if os.Getenv("CI") != "" {
						t.Fatal("helm is required in CI for chart contract tests")
					}
					t.Skip("helm unavailable")
				}
				data, err = exec.Command("helm", "template", "test", "../kubernetes/charts/brewlet",
					"--show-only", "templates/runtimeclass.yaml").CombinedOutput()
				if err != nil {
					t.Fatalf("render RuntimeClass: %v: %s", err, data)
				}
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
