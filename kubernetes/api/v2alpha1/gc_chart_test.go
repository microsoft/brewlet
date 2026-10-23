package v2alpha1

import (
	"bytes"
	"os/exec"
	"testing"
)

func TestChartWiresOperatorGCPolicyWithoutRecordPatches(t *testing.T) {
	requireHelm(t)
	for _, override := range []bool{false, true} {
		args := []string{"template", "test", "../../charts/brewlet", "--namespace", "brewlet-system"}
		grace, sweep := "1h", "1m"
		if override {
			args = append(args, "--set", "provisioner.gc.grace=20s", "--set", "provisioner.gc.sweepInterval=2s")
			grace = "20s"
			sweep = "2s"
		}
		out, err := exec.Command("helm", args...).CombinedOutput()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(out, []byte("--gc-grace="+grace)) || !bytes.Contains(out, []byte("--gc-sweep-interval="+sweep)) {
			t.Fatal("chart dropped configured production GC durations")
		}
	}
}
