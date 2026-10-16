package provisioner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/microsoft/brewlet/pkg/node"
	"github.com/prometheus/client_golang/prometheus"
)

type Metrics struct {
	Requests *prometheus.CounterVec
	Duration *prometheus.HistogramVec
}

func NewMetrics(registry prometheus.Registerer, store *node.Store, cache string, c *client.Client) *Metrics {
	m := &Metrics{
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "brewlet_runtime_image_install_requests_total", Help: "Runtime installation requests by validated ID and outcome."}, []string{"id", "outcome"}),
		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "brewlet_runtime_image_install_duration_seconds", Help: "Runtime installation and validation duration.", Buckets: prometheus.DefBuckets}, []string{"id"}),
	}
	registry.MustRegister(m.Requests, m.Duration, &inventoryCollector{store: store, cache: cache, client: c})
	return m
}
func (m *Metrics) install(id, outcome string, start time.Time) {
	if m == nil {
		return
	}
	m.Requests.WithLabelValues(id, outcome).Inc()
	m.Duration.WithLabelValues(id).Observe(time.Since(start).Seconds())
}

type inventoryCollector struct {
	store  *node.Store
	cache  string
	client *client.Client
}

var generationsDesc = prometheus.NewDesc("brewlet_runtime_image_generations", "Installed runtime generations.", []string{"id", "state"}, nil)
var diskDesc = prometheus.NewDesc("brewlet_shared_storage_bytes", "Logical shared runtime and application-layer storage bytes.", []string{"store"}, nil)
var runtimeDesc = prometheus.NewDesc("brewlet_container_runtime_image_info", "Live container/Pod bindings to approved runtime inventory generations.", []string{"namespace", "pod", "pod_uid", "container", "container_id", "id", "inventory_uid", "generation", "digest"}, nil)

func (c *inventoryCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- generationsDesc
	ch <- diskDesc
	ch <- runtimeDesc
}
func logicalBytes(root string) (int64, error) {
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	var total int64
	err := filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !e.Type().IsRegular() {
			return nil
		}
		st, err := e.Info()
		if err != nil {
			return err
		}
		total += st.Size()
		return nil
	})
	return total, err
}
func (c *inventoryCollector) Collect(ch chan<- prometheus.Metric) {
	invalid := func(err error) { ch <- prometheus.NewInvalidMetric(diskDesc, err) }
	auths, err := c.store.Authorizations()
	if err != nil {
		invalid(err)
		return
	}
	var runtimeBytes int64
	for _, a := range auths {
		base := filepath.Join(c.store.Root, a.ID)
		current, err := os.Readlink(filepath.Join(base, "current"))
		if err != nil && !os.IsNotExist(err) {
			invalid(err)
			return
		}
		epochs, err := os.ReadDir(base)
		if err != nil {
			invalid(err)
			return
		}
		counts := map[string]int{"current": 0, "retired": 0}
		for _, epoch := range epochs {
			if !epoch.IsDir() {
				continue
			}
			generations, err := os.ReadDir(filepath.Join(base, epoch.Name()))
			if err != nil {
				invalid(err)
				return
			}
			for _, generation := range generations {
				if !generation.IsDir() {
					continue
				}
				if _, err := strconv.ParseInt(generation.Name(), 10, 64); err != nil {
					invalid(fmt.Errorf("invalid runtime generation directory"))
					return
				}
				state := "retired"
				if filepath.Join(epoch.Name(), generation.Name()) == current {
					state = "current"
				}
				counts[state]++
				n, err := logicalBytes(filepath.Join(base, epoch.Name(), generation.Name(), "rootfs"))
				if err != nil {
					invalid(err)
					return
				}
				runtimeBytes += n
			}
		}
		for state, count := range counts {
			ch <- prometheus.MustNewConstMetric(generationsDesc, prometheus.GaugeValue, float64(count), a.ID, state)
		}
	}
	layerBytes, err := logicalBytes(c.cache)
	if err != nil {
		invalid(err)
		return
	}
	ch <- prometheus.MustNewConstMetric(diskDesc, prometheus.GaugeValue, float64(runtimeBytes), "runtimes")
	ch <- prometheus.MustNewConstMetric(diskDesc, prometheus.GaugeValue, float64(layerBytes), "layers")
	if c.client == nil {
		return
	}
	leases, err := c.store.Leases()
	if err != nil {
		invalid(err)
		return
	}
	inspection, cancelInspection := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelInspection()
	for _, lease := range leases {
		ctx := namespaces.WithNamespace(inspection, lease.Namespace)
		container, err := c.client.LoadContainer(ctx, lease.TaskID)
		if errdefs.IsNotFound(err) {
			continue
		} else if err != nil {
			invalid(err)
			return
		}
		task, err := container.Task(ctx, nil)
		if errdefs.IsNotFound(err) {
			continue
		} else if err != nil {
			invalid(err)
			return
		}
		pids, err := task.Pids(ctx)
		if err != nil {
			invalid(err)
			return
		}
		if len(pids) == 0 {
			continue
		}
		spec, err := container.Spec(ctx)
		if err != nil {
			invalid(err)
			return
		}
		binding, err := bindingFromAnnotations(lease, spec.Annotations, "running")
		if err != nil {
			invalid(err)
			return
		}
		ch <- runtimeBindingMetric(binding)
	}
}

func runtimeBindingMetric(binding node.RuntimeBinding) prometheus.Metric {
	return prometheus.MustNewConstMetric(runtimeDesc, prometheus.GaugeValue, 1, binding.PodNamespace, binding.PodName, binding.PodUID, binding.ContainerName, binding.ContainerID, binding.ID, binding.InventoryUID, strconv.FormatInt(binding.Generation, 10), binding.Digest)
}
