package provisioner

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestSharedDiskBytesDoNotFollowLinks(t *testing.T) {
	dir, err := os.MkdirTemp(".", ".disk-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.WriteFile(filepath.Join(dir, "bytes"), []byte("12345"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("bytes", filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	n, err := logicalBytes(dir)
	if err != nil || n != 5 {
		t.Fatal(n, err)
	}
	if n, err := logicalBytes(filepath.Join(dir, "absent")); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}

func TestInstallMetricsCountSuccessfulAndFailedAttemptsAndDuration(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry(), nil, "", nil)
	for _, outcome := range []string{"ready", "failed"} {
		m.install("static", outcome, time.Now().Add(-time.Second))
		sample := new(dto.Metric)
		if err := m.Requests.WithLabelValues("static", outcome).Write(sample); err != nil {
			t.Fatal(err)
		}
		if sample.GetCounter().GetValue() != 1 {
			t.Fatal("installation outcome was not counted", outcome, sample)
		}
	}
	samples := make(chan prometheus.Metric, 1)
	m.Duration.Collect(samples)
	close(samples)
	count := 0
	for sample := range samples {
		count++
		data := new(dto.Metric)
		if err := sample.Write(data); err != nil {
			t.Fatal(err)
		}
		if data.GetHistogram().GetSampleCount() != 2 || data.GetHistogram().GetSampleSum() < 2 {
			t.Fatal("installation duration omitted an actual attempt", data)
		}
	}
	if count != 1 {
		t.Fatal("duration metric lost or duplicated runtime identity", count)
	}
}
