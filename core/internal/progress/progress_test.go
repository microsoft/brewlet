// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package progress

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestFormatElapsed(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "0s", 59 * time.Second: "59s", 61 * time.Second: "1m01s", 10*time.Minute + 5*time.Second: "10m05s"} {
		if got := FormatElapsed(d); got != want {
			t.Errorf("FormatElapsed(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestAwaitPlainReportsStatusChangesAndHeartbeats(t *testing.T) {
	savedPoll, savedBeat := PollInterval, HeartbeatInterval
	PollInterval, HeartbeatInterval = 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { PollInterval, HeartbeatInterval = savedPoll, savedBeat })
	var buf bytes.Buffer
	want := errors.New("boom")
	err := Plain(&buf).Await("working", func(context.Context) string { return "1/2 ready" }, func() error {
		time.Sleep(80 * time.Millisecond)
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("Await must return work's error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "... working (") || !strings.Contains(out, "): 1/2 ready") || strings.Contains(out, "\r") {
		t.Fatalf("unexpected plain progress: %q", out)
	}
	if strings.Count(out, "\n") < 2 {
		t.Fatalf("expected a heartbeat after the status line: %q", out)
	}
}

func TestEmitDoesNotRedrawWithoutTerminal(t *testing.T) {
	var stderr, stdout bytes.Buffer
	p := New(&stderr)
	p.Logf("step %d", 1)
	p.Emit(&stdout, "result")
	if stderr.String() != "step 1\n" || stdout.String() != "result\n" {
		t.Fatalf("stderr=%q stdout=%q", stderr.String(), stdout.String())
	}
}
