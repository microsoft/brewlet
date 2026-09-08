// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"brewlet-operator/internal/uninstall"

	"k8s.io/client-go/rest"
)

func TestUninstallFlags(t *testing.T) {
	valid := []string{"--uninstall-release-name=brewlet", "--uninstall-release-namespace=releases"}
	for _, tc := range []struct {
		name string
		args []string
		ns   string
		mode bool
		fail bool
	}{
		{name: "normal manager", ns: "operator"},
		{name: "cleanup", args: valid, ns: "operator", mode: true},
		{name: "custom timeout", args: append(append([]string{}, valid...), "--uninstall-timeout=2m"), ns: "operator", mode: true},
		{name: "only name", args: valid[:1], ns: "operator", fail: true},
		{name: "only release ns", args: valid[1:], ns: "operator", fail: true},
		{name: "only timeout", args: []string{"--uninstall-timeout=1m"}, ns: "operator", fail: true},
		{name: "empty name", args: []string{"--uninstall-release-name=", valid[1]}, ns: "operator", fail: true},
		{name: "empty ns", args: []string{valid[0], "--uninstall-release-namespace="}, ns: "operator", fail: true},
		{name: "malformed name", args: []string{"--uninstall-release-name=Brewlet", valid[1]}, ns: "operator", fail: true},
		{name: "bad duration", args: append(append([]string{}, valid...), "--uninstall-timeout=forever"), ns: "operator", fail: true},
		{name: "zero timeout", args: append(append([]string{}, valid...), "--uninstall-timeout=0"), ns: "operator", fail: true},
		{name: "negative timeout", args: append(append([]string{}, valid...), "--uninstall-timeout=-1s"), ns: "operator", fail: true},
		{name: "empty operator namespace", args: valid, fail: true},
		{name: "malformed operator namespace", args: valid, ns: "../wrong", fail: true},
		{name: "positional argument", args: append(append([]string{}, valid...), "unexpected"), ns: "operator", fail: true},
		{name: "hidden flags", args: append([]string{"unexpected"}, valid...), ns: "operator", fail: true},
		{name: "unknown flag", args: []string{"--uninstall-release=typo"}, ns: "operator", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("manager", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			f := bindUninstallFlags(fs)
			err := fs.Parse(tc.args)
			var mode bool
			var o uninstall.Options
			if err == nil {
				o, mode, err = f.options(fs, tc.ns)
			}
			if (err != nil) != tc.fail {
				t.Fatalf("err=%v, want failure=%v", err, tc.fail)
			}
			if tc.fail {
				return
			}
			if mode != tc.mode {
				t.Fatalf("cleanup mode=%v, want %v", mode, tc.mode)
			}
			if mode && (o.Namespace != "operator" || o.ReleaseNamespace != "releases" || o.ReleaseName != "brewlet") {
				t.Fatalf("wrong release/operator scope: %+v", o)
			}
			want := uninstall.DefaultTimeout
			if tc.name == "custom timeout" {
				want = 2 * time.Minute
			}
			if o.Timeout != want {
				t.Fatalf("timeout=%v, want %v", o.Timeout, want)
			}
		})
	}
}

func TestUninstallDirectClientUsesClusterWideFreshLists(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Query().Get("resourceVersion") == "0" || r.URL.Query().Get("watch") != "" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		var apiVersion, kind string
		switch r.URL.Path {
		case "/apis/node.brewlet.sh/v1alpha1/nodeprofiles":
			apiVersion, kind = "node.brewlet.sh/v1alpha1", "NodeProfileList"
		case "/apis/apps/v1/daemonsets":
			apiVersion, kind = "apps/v1", "DaemonSetList"
		case "/api/v1/pods":
			apiVersion, kind = "v1", "PodList"
		default:
			t.Errorf("unexpected discovery, informer or namespace-filtered API request: %s", r.URL)
			http.Error(w, "not allowed", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"apiVersion":%q,"kind":%q,"metadata":{"resourceVersion":"1"},"items":[]}`, apiVersion, kind)
	}))
	defer server.Close()
	c, err := newUninstallClient(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	o := uninstall.Options{
		ReleaseName: "brewlet", ReleaseNamespace: "releases", Namespace: "operator",
		Timeout: time.Second, PollInterval: time.Millisecond,
	}
	if err := uninstall.Run(context.Background(), c, o); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 8 {
		t.Fatalf("requests=%d, want two fresh complete scans (8 reads)", requests.Load())
	}
}

func TestUninstallDirectClientRequestDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	c, err := newUninstallClient(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	o := uninstall.Options{
		ReleaseName: "brewlet", ReleaseNamespace: "releases", Namespace: "operator",
		Timeout: 20 * time.Millisecond,
	}
	err = uninstall.Run(context.Background(), c, o)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "keep the operator") {
		t.Fatalf("hung API request did not respect uninstall deadline: %v", err)
	}
}
