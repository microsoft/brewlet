package node

import "time"

const BindingsAnnotation = "brewlet.sh/runtime-bindings"

type RuntimeBinding struct {
	ContainerID         string `json:"containerID"`
	ContainerdNamespace string `json:"containerdNamespace"`
	PodUID              string `json:"podUID"`
	PodName             string `json:"podName"`
	PodNamespace        string `json:"podNamespace"`
	ContainerName       string `json:"containerName"`
	ID                  string `json:"id"`
	InventoryUID        string `json:"inventoryUID"`
	Generation          int64  `json:"generation"`
	Digest              string `json:"digest"`
	State               string `json:"state"`
}

type BindingObservation struct {
	NodeUID           string           `json:"nodeUID"`
	ProfileUID        string           `json:"profileUID"`
	ProfileGeneration int64            `json:"profileGeneration"`
	ObservedAt        time.Time        `json:"observedAt"`
	Complete          bool             `json:"complete"`
	Bindings          []RuntimeBinding `json:"bindings"`
}
