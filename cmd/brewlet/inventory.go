package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
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

func runInventory(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("inventory", flag.ContinueOnError)
	kubeconfig := fs.String("kubeconfig", "", "explicit Kubernetes configuration")
	contextName := fs.String("context", "", "explicit context")
	policyFile := fs.String("policy", "", "release trust policy")
	output := fs.String("output", "", "JSON report file; stdout if omitted")
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
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if *output != "" {
		if err = atomicReleaseResult(*output, data); err != nil {
			return err
		}
	} else if _, err = os.Stdout.Write(data); err != nil {
		return err
	}
	if !report.Complete {
		return fmt.Errorf("inventory incomplete: consult unverified containers and scope warnings")
	}
	return nil
}
