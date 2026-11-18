package workload

import (
	"reflect"
	"testing"
)

func TestActiveSelectionsAndReservations(t *testing.T) {
	annotations := map[string]string{
		RuntimeImageAnnotationPrefix + "app":    "java-25",
		RuntimeImageAnnotationPrefix + "worker": "python-3.13",
		RuntimeImageAnnotationPrefix + "debug":  "debug-runtime",
	}
	active, err := ActiveSelections(annotations, []string{"worker", "app", "telemetry"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Selection{{ContainerName: "app", RuntimeID: "java-25"}, {ContainerName: "worker", RuntimeID: "python-3.13"}}
	if !reflect.DeepEqual(active, want) {
		t.Fatalf("active selections = %#v, want %#v", active, want)
	}
	if got := DistinctRuntimeIDs(active); !reflect.DeepEqual(got, []string{"java-25", "python-3.13"}) {
		t.Fatalf("runtime IDs = %#v", got)
	}
}

func TestParseSelectionsRejectsMalformedKeysAndValues(t *testing.T) {
	for _, annotations := range []map[string]string{
		{RuntimeImageAnnotationPrefix + "Bad_Name": "java-25"},
		{RuntimeImageAnnotationPrefix + "app": ""},
		{RuntimeImageAnnotationPrefix + "app": "java-25:latest"},
	} {
		if _, err := ParseSelections(annotations); err == nil {
			t.Fatalf("accepted malformed selection: %#v", annotations)
		}
	}
}

func TestRuntimeIDForContainerDoesNotMatchOtherPodNames(t *testing.T) {
	annotations := map[string]string{RuntimeImageAnnotationPrefix + "app": "java-25"}
	for _, name := range []string{"app", "sidecar"} {
		id, selected, err := RuntimeIDForContainer(annotations, name)
		if err != nil {
			t.Fatal(err)
		}
		if selected != (name == "app") || (selected && id != "java-25") {
			t.Fatalf("selection for %q = %q, %t", name, id, selected)
		}
	}

}

func TestSameContainerNameIsScopedToEachPod(t *testing.T) {
	for _, tc := range []struct {
		annotations map[string]string
		want        string
	}{
		{map[string]string{RuntimeImageAnnotationPrefix + "app": "java-25"}, "java-25"},
		{map[string]string{RuntimeImageAnnotationPrefix + "app": "python-3.13"}, "python-3.13"},
	} {
		id, selected, err := RuntimeIDForContainer(tc.annotations, "app")
		if err != nil || !selected || id != tc.want {
			t.Fatalf("same-named container resolved to %q, selected=%t err=%v; want %q", id, selected, err, tc.want)
		}
	}
}
