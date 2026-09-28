// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"github.com/microsoft/brewlet/internal/runnablestage"
	"github.com/prometheus/client_golang/prometheus"
)

type stageCollector struct {
	root  string
	bytes *prometheus.Desc
}

func newStageCollector(root string) *stageCollector {
	return &stageCollector{
		root: root,
		bytes: prometheus.NewDesc(
			"brewlet_runnable_stage_bytes",
			"Logical bytes remaining in runnable staging trees, including legacy and pending stages.",
			nil, nil,
		),
	}
}

func (c *stageCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.bytes
}

func (c *stageCollector) Collect(ch chan<- prometheus.Metric) {
	size, err := runnablestage.Bytes(c.root)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(c.bytes, err)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.bytes, prometheus.GaugeValue, float64(size))
}
