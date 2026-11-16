package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	"github.com/microsoft/brewlet/pkg/artifact"
	"github.com/microsoft/brewlet/pkg/inventory"
	"github.com/microsoft/brewlet/pkg/node"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const inventorySynopsis = `Usage: brewlet inventory --kubeconfig FILE --context NAME --namespace NS... --policy POLICY [flags]

Reports which dependency layer sets running workloads were built from, with the
verification evidence behind each container. It observes and exports facts; it
does not scan for vulnerabilities or change workloads. Incomplete evidence
prints a partial report and exits nonzero. Unverified containers are unknown,
not clean.

Use -o json for the full machine-readable report, or --report FILE to write it
to a file as well.`

func runInventory(ctx context.Context, args []string, stdout, _ io.Writer) error {
	fs := newFlagSet("inventory", inventorySynopsis, stdout)
	kubeconfig := fs.String("kubeconfig", "", "explicit Kubernetes configuration")
	contextName := fs.String("context", "", "explicit context")
	policyFile := fs.String("policy", "", "release trust policy")
	reportFile := fs.String("report", "", "also write the full JSON report to FILE")
	output := addOutputFlag(fs)
	authFile := fs.String("registry-auth", "", "origin-scoped registry credentials")
	maxAge := fs.Duration("max-age", 2*time.Minute, "maximum node observation age")
	insecure := fs.Bool("insecure", false, "allow HTTP registries")
	https := fs.Bool("registry-https", false, "require HTTPS including loopback")
	var namespaces, workloads repeated
	fs.Var(&namespaces, "namespace", "explicit namespace (repeatable)")
	fs.Var(&workloads, "workload", "Kind/namespace/name (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := checkTextOutput("inventory", *output); err != nil {
		return err
	}
	if fs.NArg() != 0 || *kubeconfig == "" || *contextName == "" || *policyFile == "" || len(namespaces) == 0 ||
		*maxAge <= 0 || *maxAge > 2*time.Minute || *insecure && *https {
		return fmt.Errorf("inventory requires --kubeconfig, --context, --policy, --namespace and observation age in (0,2m]")
	}
	policy, err := readReleasePolicy(*policyFile)
	if err != nil {
		return err
	}
	registries, err := loadReleaseRegistries(*authFile, *insecure, *https)
	if err != nil {
		return err
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: *kubeconfig}, &clientcmd.ConfigOverrides{CurrentContext: *contextName}).ClientConfig()
	if err != nil {
		return err
	}
	config.Timeout = 30 * time.Second
	endpoint, err := url.Parse(config.Host)
	if err != nil || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return fmt.Errorf("inventory endpoint must not embed credentials, a query or fragment")
	}
	scheme := runtime.NewScheme()
	if err = clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err = api.AddToScheme(scheme); err != nil {
		return err
	}
	reader, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	temp, err := os.MkdirTemp("", "brewlet-inventory-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	store := artifact.Store{Dir: temp}
	report, err := inventory.Capture(ctx, reader, inventory.Options{Cluster: config.Host, Namespaces: namespaces, Workloads: workloads,
		MaxAge: *maxAge, ReadImage: func(ctx context.Context, identity node.ApplicationIdentity) (artifact.DependencyReport, error) {
			target, err := pinnedReleaseInput(ctx, store, identity.Reference, registries)
			if err != nil {
				return artifact.DependencyReport{}, err
			}
			return artifact.InspectDependencies(ctx, store, target, identity.Platform, policy)
		}})
	if err != nil {
		return err
	}
	if *reportFile != "" {
		data, err := json.Marshal(report)
		if err != nil {
			return err
		}
		if err = atomicReleaseResult(*reportFile, append(data, '\n')); err != nil {
			return err
		}
	}
	if *output == outputJSON {
		err = writeJSON(stdout, report)
	} else {
		err = writeInventory(stdout, report)
	}
	if err != nil {
		return err
	}
	if !report.Complete {
		return fmt.Errorf("inventory incomplete: consult unverified containers and scope warnings")
	}
	return nil
}

func writeInventory(w io.Writer, r inventory.Report) error {
	fmt.Fprintf(w, "Inventory of %s\n", r.Cluster)
	if err := writeFields(w, "  ",
		field{"Namespaces", strings.Join(r.Namespaces, ", ")},
		field{"Workloads", strings.Join(r.Workloads, ", ")},
		field{"Observed", r.FinishedAt.UTC().Format(time.RFC3339)},
		field{"Complete", yesNo(r.Complete)}); err != nil {
		return err
	}
	fmt.Fprintf(w, "\nContainers (%d):\n", len(r.Containers))
	rows := make([][]string, 0, len(r.Containers))
	for _, c := range r.Containers {
		rows = append(rows, []string{c.Pod.Namespace + "/" + c.Pod.Name, c.Name, c.Workload.Kind + "/" + c.Workload.Name, orDash(c.NodeName), c.Status, c.Reason})
	}
	if err := writeTable(w, "  ", []string{"POD", "CONTAINER", "WORKLOAD", "NODE", "STATUS", "REASON"}, rows); err != nil {
		return err
	}
	keys := make([]string, 0, len(r.Artifacts))
	for key := range r.Artifacts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fmt.Fprintf(w, "\nArtifacts (%d):\n", len(keys))
	rows = rows[:0]
	for _, key := range keys {
		facts := r.Artifacts[key]
		rows = append(rows, []string{key, fmt.Sprint(len(facts.Packages)), orDash(facts.Verification.Predicate.DependencySet.Digest.String())})
	}
	if err := writeTable(w, "  ", []string{"APPLICATION", "PACKAGES", "DEPENDENCY SET"}, rows); err != nil {
		return err
	}
	if len(r.Warnings) > 0 {
		fmt.Fprintln(w, "\nWarnings:")
		for _, warning := range r.Warnings {
			fmt.Fprintf(w, "  - %s\n", warning)
		}
	}
	return nil
}
