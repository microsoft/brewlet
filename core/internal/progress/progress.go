// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package progress reports long-running CLI steps on stderr.
package progress

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Cadence of status polling and non-terminal output. Tests may shorten these.
var (
	PollInterval      = 3 * time.Second
	HeartbeatInterval = 30 * time.Second
	spinnerInterval   = 120 * time.Millisecond
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Reporter reports long-running steps on stderr. On a terminal it redraws one
// transient status line with a spinner and elapsed time; otherwise (CI, pipes,
// log files) it prints a plain line whenever the status changes and a
// heartbeat at least every HeartbeatInterval so the command never looks hung.
type Reporter struct {
	mu        sync.Mutex
	w         io.Writer
	tty       bool
	transient bool
}

// New returns a Reporter that animates a status line when w is a terminal.
func New(w io.Writer) *Reporter {
	return &Reporter{w: w, tty: isTerminal(w)}
}

// Plain returns a Reporter that never redraws lines. Use it when a child
// process shares the terminal, so a spinner would corrupt the child's output.
func Plain(w io.Writer) *Reporter {
	return &Reporter{w: w}
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok || os.Getenv("TERM") == "dumb" {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// Logf prints a permanent line, first clearing any transient status line.
func (p *Reporter) Logf(format string, args ...any) {
	p.Emit(p.w, format, args...)
}

// Emit prints a permanent line to w (for example stdout results) without
// interleaving it with a transient status line on the same terminal.
func (p *Reporter) Emit(w io.Writer, format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
	fmt.Fprintf(w, format+"\n", args...)
}

func (p *Reporter) clearLocked() {
	if p.transient {
		fmt.Fprint(p.w, "\r\033[K")
		p.transient = false
	}
}

func (p *Reporter) redraw(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprint(p.w, "\r\033[K"+line)
	p.transient = true
}

// Await runs work while reporting elapsed time and, when poll is non-nil, the
// latest status it returns. poll runs off the caller's goroutine and its
// context is cancelled as soon as work returns.
func (p *Reporter) Await(label string, poll func(context.Context) string, work func() error) error {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var mu sync.Mutex
	status := ""
	if poll != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(PollInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				if s := poll(ctx); s != "" && ctx.Err() == nil {
					mu.Lock()
					status = s
					mu.Unlock()
				}
			}
		}()
	}
	start := time.Now()
	wg.Add(1)
	go func() {
		defer wg.Done()
		interval := spinnerInterval
		if !p.tty {
			interval = PollInterval
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		frame, printed, lastPrint := 0, "", start
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			mu.Lock()
			s := status
			mu.Unlock()
			elapsed := FormatElapsed(time.Since(start))
			if p.tty {
				line := fmt.Sprintf("    %s %s (%s)", spinnerFrames[frame%len(spinnerFrames)], label, elapsed)
				if s != "" {
					line += " — " + s
				}
				p.redraw(line)
				frame++
				continue
			}
			if s != printed || time.Since(lastPrint) >= HeartbeatInterval {
				if s == "" {
					p.Logf("    ... %s (%s)", label, elapsed)
				} else {
					p.Logf("    ... %s (%s): %s", label, elapsed, s)
				}
				printed, lastPrint = s, time.Now()
			}
		}
	}()
	err := work()
	cancel()
	wg.Wait()
	p.mu.Lock()
	p.clearLocked()
	p.mu.Unlock()
	return err
}

// FormatElapsed renders a duration as 42s or 1m05s.
func FormatElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}
