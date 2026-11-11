package power

import (
	"context"
	"errors"
	"testing"
)

func TestParsePmset(t *testing.T) {
	cases := []struct {
		name, out string
		battery   bool
		wantErr   bool
	}{
		{"battery", "Now drawing from 'Battery Power'\n -InternalBattery-0 (id=1)\t80%; discharging; 4:00 remaining present: true\n", true, false},
		{"ac", "Now drawing from 'AC Power'\n -InternalBattery-0 (id=1)\t80%; charging; present: true\n", false, false},
		{"ups", "Now drawing from 'UPS Power'\n", false, false},
		{"desktop", "Now drawing from 'AC Power'\n", false, false},
		{"empty", "", false, true},
		{"garbage", "nope\n", false, true},
	}
	for _, c := range cases {
		got, err := ParsePmset(c.out)
		if (err != nil) != c.wantErr || got != c.battery {
			t.Errorf("%s: got (%v, %v), want (%v, err=%v)", c.name, got, err, c.battery, c.wantErr)
		}
	}
}

func TestPmsetSource(t *testing.T) {
	ran := ""
	p := Pmset{Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		ran = name + " " + args[0] + " " + args[1]
		return []byte("Now drawing from 'Battery Power'\n"), nil
	}}
	b, err := p.OnBattery(context.Background())
	if err != nil || !b || ran != "pmset -g batt" {
		t.Fatalf("got (%v, %v) ran %q", b, err, ran)
	}
	p.Run = func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("boom") }
	if _, err := p.OnBattery(context.Background()); err == nil {
		t.Fatal("want error")
	}
}

func TestNewFor(t *testing.T) {
	if _, ok := newFor("linux").(ACSource); !ok {
		t.Error("linux should be ACSource")
	}
	if _, ok := newFor("darwin").(Pmset); !ok {
		t.Error("darwin should be Pmset")
	}
	if b, err := (ACSource{}).OnBattery(context.Background()); b || err != nil {
		t.Error("ACSource must report AC")
	}
}
