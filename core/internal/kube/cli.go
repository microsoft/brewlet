// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/microsoft/brewlet/internal/doctor"
	"github.com/microsoft/brewlet/internal/inventory"
)

const help = `Brewlet Kubernetes operations

USAGE:
  brewlet k8s [connection flags] <command> [flags]

COMMANDS:
  jdk list                       advertised node JDKs (table, wide, json)
  launcher list                  advertised optional launchers (table, wide, json)
  profile list                   desired inventories and profile readiness
  profile inspect NAME           profile configuration and its claimed nodes
  inspect app NAME               application, owned workloads, pods and events
  status                         control-plane rollouts, profiles and node failures
  doctor                         developer and cluster readiness checks
  install --version X.Y.Z -f FILE install the released Helm chart on a fresh cluster
  jdk add --profile NAME --distribution NAME --feature N --image REF --java-home PATH
  launcher add --profile NAME --name NAME --image REF --path PATH

COMMON FLAGS (before the command or after its full name):
  --kubeconfig FILE   kubeconfig path (otherwise kubectl/Helm defaults)
  --context NAME     context to use without changing kubeconfig
  --namespace NAME   application namespace (current context); doctor defaults to default; install to brewlet
  --timeout 30s      per-kubectl-command deadline

READ FLAGS:
  --output table|json|yaml  default table (inventory also supports wide, not yaml)
  --selector LABELS        node selector for inventory
  --system-namespace NAME  status control-plane namespace (default brewlet)

PROFILE ADD FLAGS:
  --profile NAME     existing target profile
  --image REF        digest-pinned runtime source
  JDK: --distribution NAME --feature N --java-home PATH
  Launcher: --name NAME --path PATH
  --output yaml|json default yaml; prints the full updated declaration
  --replace          explicitly replace an existing source with the same identity

ADD:
  Updates the live profile by default, even when stdout is redirected.
  --dry-run          validate locally and print the proposed declaration (same as client)
  --dry-run=client   local validation only; no patch is sent to the API server
  --dry-run=server   validate through the API server and print the result without saving
  Failed dry runs return an error and print no rendered YAML/JSON.
  Helm/GitOps-owned live profiles cannot be patched. Edit their source of truth.

OFFLINE INPUT (requires --dry-run or --dry-run=client):
  By default add reads the live profile. These options read files instead:
  --file FILE        read a single NodeProfile YAML/JSON file instead of the cluster
  --values FILE      read complete Helm values and emit updated values
  Input files are never overwritten.

INSTALL FLAGS:
  --values FILE, -f FILE  complete Helm values (repeatable, at least one required)
  --release NAME         Helm release name (default brewlet)
  --wait-timeout 5m      Helm rollout deadline (node provisioning is separate)
  --dry-run              render chart manifests locally without installing

Installation and profile changes never create a cluster or debug a workload.
`

// Run dispatches the Kubernetes command group. Reads and generated declarations
// go to stdout; mutation notices and diagnostic hints go to stderr.
func Run(ctx context.Context, args []string, out, stderr io.Writer) error {
	return run(ctx, args, out, stderr, execute)
}

func commonFlags(fs *flag.FlagSet, opts *options) {
	fs.StringVar(&opts.kubeconfig, "kubeconfig", opts.kubeconfig, "kubeconfig path")
	fs.StringVar(&opts.context, "context", opts.context, "Kubernetes context")
	fs.StringVar(&opts.namespace, "namespace", opts.namespace, "namespace (defaults to current context)")
	fs.DurationVar(&opts.timeout, "timeout", opts.timeout, "per-command kubectl timeout")
}

func run(ctx context.Context, args []string, out, stderr io.Writer, exec executor) error {
	opts := options{timeout: 30 * time.Second, systemNamespace: "brewlet"}
	root := flag.NewFlagSet("k8s", flag.ContinueOnError)
	root.SetOutput(stderr)
	// Keep usage/validation failures off stdout so redirected previews stay empty.
	root.Usage = func() {}
	commonFlags(root, &opts)
	if err := root.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err := fmt.Fprint(out, help)
			return err
		}
		return err
	}
	args = root.Args()
	if len(args) == 0 || args[0] == "help" {
		_, err := fmt.Fprint(out, help)
		return err
	}
	command := args[0]
	args = args[1:]
	if command == "jdk" || command == "launcher" || command == "profile" || command == "inspect" {
		if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
			_, err := fmt.Fprint(out, help)
			return err
		}
		command += " " + args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("k8s "+command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = root.Usage
	commonFlags(fs, &opts)
	var update updateOptions
	var install installOptions
	switch command {
	case "jdk list", "launcher list":
		fs.StringVar(&opts.output, "output", "table", "table, wide, or json")
		fs.StringVar(&opts.selector, "selector", "", "node label selector")
	case "status":
		fs.StringVar(&opts.systemNamespace, "system-namespace", "brewlet", "Brewlet control-plane namespace")
		fallthrough
	case "profile list", "profile inspect", "inspect app", "doctor":
		fs.StringVar(&opts.output, "output", "table", "table, json, or yaml")
	case "jdk add", "launcher add":
		update.flags(fs, command == "jdk add")
		fs.StringVar(&opts.output, "output", "yaml", "yaml or json")
	case "install":
		install.flags(fs)
	default:
		return fmt.Errorf("unknown Kubernetes command %q; see brewlet k8s --help", command)
	}
	pos, err := parseFlags(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		_, err := fmt.Fprint(out, help)
		return err
	}
	if err != nil {
		return err
	}
	expected := 0
	if command == "profile inspect" || command == "inspect app" {
		expected = 1
	}
	if len(pos) != expected {
		return fmt.Errorf("%s expects %d positional arguments; see brewlet k8s --help", command, expected)
	}
	if opts.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	if expected == 1 {
		if err := validateName(pos[0], 253); err != nil {
			return err
		}
	}
	allowed := []string{"table", "json", "yaml"}
	switch command {
	case "jdk list", "launcher list":
		allowed = []string{"table", "wide", "json"}
	case "jdk add", "launcher add":
		allowed = []string{"yaml", "json"}
	case "install":
		allowed = []string{""}
	}
	if !oneOf(opts.output, allowed...) {
		return fmt.Errorf("invalid --output %q (want %s)", opts.output, strings.Join(allowed, ", "))
	}
	c := &client{ctx: ctx, opts: opts, exec: exec, out: out, err: stderr}
	switch command {
	case "jdk list":
		return c.jdks()
	case "launcher list":
		return c.launchers()
	case "profile list":
		return c.profiles()
	case "profile inspect":
		return c.inspectProfile(pos[0])
	case "inspect app":
		return c.inspectApp(pos[0])
	case "status":
		return c.status()
	case "doctor":
		return c.doctor()
	case "jdk add", "launcher add":
		return c.updateProfile(update)
	case "install":
		return c.install(install)
	}
	panic("unreachable command")
}

func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for len(args) > 0 {
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) > 0 {
			pos = append(pos, args[0])
			args = args[1:]
		}
	}
	return pos, nil
}

func oneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}

func (c *client) jdks() error {
	objects, err := c.list("nodes", c.nodeArgs()...)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(struct {
		Items []object `json:"items"`
	}{objects})
	if err != nil {
		return err
	}
	nodes, err := inventory.ParseNodes(raw)
	if err != nil {
		return err
	}
	switch c.opts.output {
	case "table":
		inventory.RenderTable(c.out, nodes)
	case "wide":
		inventory.RenderByNode(c.out, nodes)
	case "json":
		return inventory.RenderJSON(c.out, nodes)
	}
	return nil
}

func (c *client) doctor() error {
	report := doctor.Run(func(args ...string) ([]byte, error) {
		return c.kubectl(nil, args...)
	}, doctor.Options{Context: c.opts.context, Namespace: c.opts.namespace})
	if c.opts.output == "table" {
		for _, check := range report.Checks {
			fmt.Fprintf(c.out, "[%-4s] %-20s %s\n", strings.ToUpper(string(check.Status)), check.Name, check.Detail)
			if check.Remediation != "" && check.Status != doctor.Pass {
				fmt.Fprintf(c.out, "       fix: %s\n", check.Remediation)
			}
		}
	} else if err := encode(c.out, report, c.opts.output); err != nil {
		return err
	}
	if !report.OK() {
		return fmt.Errorf("doctor found one or more blocking checks")
	}
	return nil
}
