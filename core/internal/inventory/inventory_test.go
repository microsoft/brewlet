// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package inventory

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

const nodesJSON = `{
  "items": [
    {
      "metadata": {
        "name": "node-a",
        "annotations": {
          "brewlet.sh/jdks-info": "[{\"distribution\":\"temurin\",\"vendor\":\"Eclipse Adoptium\",\"feature\":21,\"version\":\"21.0.5\",\"arch\":\"amd64\"},{\"distribution\":\"microsoft\",\"vendor\":\"Microsoft\",\"feature\":25,\"version\":\"25\",\"arch\":\"amd64\"}]"
        }
      }
    },
    {
      "metadata": {
        "name": "node-b",
        "annotations": {
          "brewlet.sh/jdks-info": "[{\"distribution\":\"temurin\",\"vendor\":\"Eclipse Adoptium\",\"feature\":21,\"version\":\"21.0.5\",\"arch\":\"amd64\"}]"
        }
      }
    },
    {
      "metadata": {
        "name": "node-compact",
        "annotations": {
          "brewlet.sh/jdks": "temurin-17,microsoft-25"
        }
      }
    },
    {
      "metadata": {
        "name": "node-none",
        "annotations": {}
      }
    }
  ]
}`

func TestParseNodes(t *testing.T) {
	nodes, err := ParseNodes([]byte(nodesJSON))
	if err != nil {
		t.Fatalf("ParseNodes: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("want 2 nodes with structured inventory, got %d: %+v", len(nodes), nodes)
	}

	byName := map[string][]JDKInfo{}
	for _, n := range nodes {
		byName[n.Node] = n.JDKs
	}
	for _, name := range []string{"node-none", "node-compact"} {
		if _, ok := byName[name]; ok {
			t.Errorf("%s should be omitted", name)
		}
	}

	a := byName["node-a"]
	if len(a) != 2 {
		t.Fatalf("node-a: want 2 jdks, got %d", len(a))
	}
	if a[0].Vendor != "Eclipse Adoptium" || a[0].Version != "21.0.5" || a[0].Arch != "amd64" || a[0].Feature != 21 {
		t.Errorf("node-a[0] unexpected: %+v", a[0])
	}
}

func TestAggregate(t *testing.T) {
	nodes, err := ParseNodes([]byte(nodesJSON))
	if err != nil {
		t.Fatalf("ParseNodes: %v", err)
	}
	agg := Aggregate(nodes)

	if len(agg) != 2 {
		t.Fatalf("want 2 distinct jdks, got %d: %+v", len(agg), agg)
	}

	// temurin-21 must be provided by node-a AND node-b.
	var found bool
	for _, a := range agg {
		if a.Distribution == "temurin" && a.Feature == 21 && a.Arch == "amd64" {
			found = true
			if len(a.Nodes) != 2 {
				t.Errorf("temurin-21 want 2 nodes, got %v", a.Nodes)
			}
			if a.Nodes[0] != "node-a" || a.Nodes[1] != "node-b" {
				t.Errorf("temurin-21 nodes not sorted: %v", a.Nodes)
			}
		}
	}
	if !found {
		t.Errorf("temurin-21 amd64 not found in aggregate")
	}

	// Sorted by distribution first: microsoft entries precede temurin entries.
	if agg[0].Distribution != "microsoft" {
		t.Errorf("expected microsoft first, got %q", agg[0].Distribution)
	}
}

func TestRenderTable(t *testing.T) {
	nodes, _ := ParseNodes([]byte(nodesJSON))
	var buf bytes.Buffer
	RenderTable(&buf, nodes)
	out := buf.String()
	for _, want := range []string{"VENDOR", "DISTRIBUTION", "MAJOR", "VERSION", "ARCH", "NODES", "Eclipse Adoptium", "21.0.5", "amd64"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q\n%s", want, out)
		}
	}
}

func TestRenderEmpty(t *testing.T) {
	for _, render := range []func(io.Writer, []NodeJDKs){RenderTable, RenderByNode} {
		var buf bytes.Buffer
		render(&buf, nil)
		for _, want := range []string{"No Brewlet JDK inventory", Annotation, "node-provisioner", "docs/jdk-management.md"} {
			if !strings.Contains(buf.String(), want) {
				t.Errorf("empty render missing %q: %s", want, buf.String())
			}
		}
	}
	var buf bytes.Buffer
	if err := RenderJSON(&buf, nil); err != nil || buf.String() != "[]\n" {
		t.Fatalf("empty JSON = %q, %v", buf.String(), err)
	}
}

func TestRenderByNode(t *testing.T) {
	nodes, _ := ParseNodes([]byte(nodesJSON))
	var buf bytes.Buffer
	RenderByNode(&buf, nodes)
	out := buf.String()
	if !strings.Contains(out, "node-a") || !strings.Contains(out, "node-b") || strings.Contains(out, "node-compact") {
		t.Errorf("by-node output must include only nodes with structured inventory:\n%s", out)
	}
}

func TestRenderJSON(t *testing.T) {
	nodes, _ := ParseNodes([]byte(nodesJSON))
	var buf bytes.Buffer
	if err := RenderJSON(&buf, nodes); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	var agg []AggregatedJDK
	if err := json.Unmarshal(buf.Bytes(), &agg); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if len(agg) != 2 {
		t.Errorf("want 2 aggregated jdks in JSON, got %d", len(agg))
	}
}

func TestParseNodesBadJSON(t *testing.T) {
	if _, err := ParseNodes([]byte("not json")); err == nil {
		t.Errorf("expected error for invalid JSON")
	}
}

func TestParseNodesStructuredInventory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		present bool
		want    int
		wantErr bool
	}{
		{name: "compact only"},
		{name: "blank", present: true},
		{name: "whitespace", present: true, raw: " \n\t"},
		{name: "empty array", present: true, raw: "[]"},
		{name: "null", present: true, raw: "null"},
		{name: "structured overrides compact", present: true, raw: `[{"distribution":"microsoft","vendor":"Microsoft","feature":25,"version":"25.0.1","arch":"arm64"}]`, want: 1},
		{name: "malformed", present: true, raw: "{not-an-array", wantErr: true},
		{name: "object", present: true, raw: "{}", wantErr: true},
		{name: "wrong field type", present: true, raw: `[{"feature":"21"}]`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ann := map[string]string{"brewlet.sh/jdks": "temurin-21"}
			if tc.present {
				ann[Annotation] = tc.raw
			}
			annotations, err := json.Marshal(ann)
			if err != nil {
				t.Fatal(err)
			}
			items := []json.RawMessage{
				json.RawMessage(`{"metadata":{"name":"worker","annotations":` + string(annotations) + `}}`),
			}
			if tc.wantErr {
				items = append([]json.RawMessage{
					json.RawMessage(`{"metadata":{"name":"valid","annotations":{"brewlet.sh/jdks-info":"[{\"distribution\":\"temurin\",\"feature\":21,\"vendor\":\"Adoptium\",\"version\":\"21.0.5\",\"arch\":\"amd64\"}]"}}}`),
				}, items...)
			}
			raw, err := json.Marshal(struct {
				Items []json.RawMessage `json:"items"`
			}{items})
			if err != nil {
				t.Fatal(err)
			}
			nodes, err := ParseNodes(raw)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), `node "worker"`) || !strings.Contains(err.Error(), Annotation) || nodes != nil {
					t.Fatalf("want contextual error and no partial inventory, got %v, %v", nodes, err)
				}
				return
			}
			if err != nil || len(nodes) != tc.want {
				t.Fatalf("inventory = %+v, %v", nodes, err)
			}
			if tc.want != 0 {
				if len(nodes[0].JDKs) != 1 || nodes[0].JDKs[0] != (JDKInfo{
					Distribution: "microsoft", Vendor: "Microsoft", Feature: 25, Version: "25.0.1", Arch: "arm64",
				}) {
					t.Fatalf("structured metadata changed: %+v", nodes)
				}
			}
		})
	}
}
