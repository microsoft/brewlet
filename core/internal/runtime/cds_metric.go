// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtime

import "github.com/microsoft/brewlet/internal/telemetry"

// recordRegenMetric sends the AppCDS decision to the node exporter over its Unix
// socket. Telemetry failures must never affect workload launch.
func recordRegenMetric(role RegenRole) {
	_ = telemetry.Emit(telemetry.Event{Kind: telemetry.KindCDS, CDSRole: string(role)})
}
