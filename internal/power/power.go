package power

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// Source reports the host's power source.
type Source interface {
	// OnBattery is true when the host is running on battery power.
	OnBattery(ctx context.Context) (bool, error)
}

// ACSource always reports AC power. It is the no-op used off darwin.
type ACSource struct{}

func (ACSource) OnBattery(context.Context) (bool, error) { return false, nil }

// Pmset reads `pmset -g batt`. Run is the command runner (nil = exec).
type Pmset struct {
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func (p Pmset) OnBattery(ctx context.Context) (bool, error) {
	run := p.Run
	if run == nil {
		run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output()
		}
	}
	out, err := run(ctx, "pmset", "-g", "batt")
	if err != nil {
		return false, fmt.Errorf("pmset -g batt: %w", err)
	}
	return ParsePmset(string(out))
}

// New returns the Source for the current OS: pmset on darwin, else ACSource.
func New() Source { return newFor(runtime.GOOS) }

func newFor(goos string) Source {
	if goos == "darwin" {
		return Pmset{}
	}
	return ACSource{}
}

// ParsePmset interprets `pmset -g batt` output. The first line names the
// source: "Now drawing from 'Battery Power'" or "... 'AC Power'" (also
// 'UPS Power', which is not the laptop battery and counts as AC).
func ParsePmset(out string) (onBattery bool, err error) {
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	switch {
	case strings.Contains(first, "'Battery Power'"):
		return true, nil
	case strings.Contains(first, "'AC Power'"), strings.Contains(first, "'UPS Power'"):
		return false, nil
	}
	return false, fmt.Errorf("unrecognised pmset output %q", first)
}
