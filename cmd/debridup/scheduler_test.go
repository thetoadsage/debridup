package main

import (
	"testing"
	"time"
)

func TestParseMaxConcurrentChecks(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    int
		wantErr bool
	}{
		{name: "default", want: 4},
		{name: "minimum", value: "1", want: 1},
		{name: "maximum", value: "32", want: 32},
		{name: "zero", value: "0", wantErr: true},
		{name: "above maximum", value: "33", wantErr: true},
		{name: "not an integer", value: "many", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseMaxConcurrentChecks(test.value)
			if (err != nil) != test.wantErr {
				t.Fatalf("parseMaxConcurrentChecks(%q) error = %v, wantErr %v", test.value, err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("parseMaxConcurrentChecks(%q) = %d, want %d", test.value, got, test.want)
			}
		})
	}
}

func TestRunCoordinatorRejectsOverlap(t *testing.T) {
	c := newRunCoordinator(2)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if !c.Claim(1, now, 15*time.Second) {
		t.Fatal("first claim rejected")
	}
	if c.Claim(1, now.Add(20*time.Second), 15*time.Second) {
		t.Fatal("overlap accepted")
	}
	c.Release(1)
	if !c.Claim(1, now.Add(20*time.Second), 15*time.Second) {
		t.Fatal("claim after release rejected")
	}
}

func TestRunCoordinatorBoundsGlobalConcurrency(t *testing.T) {
	c := newRunCoordinator(1)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if !c.Claim(1, now, time.Second) {
		t.Fatal("first claim rejected")
	}
	if c.Claim(2, now, time.Second) {
		t.Fatal("global limit exceeded")
	}
}

func TestRunCoordinatorForgetsLastRun(t *testing.T) {
	c := newRunCoordinator(1)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if !c.Claim(1, now, time.Hour) {
		t.Fatal("first claim rejected")
	}
	c.Release(1)
	c.Forget(1)
	if !c.Claim(1, now.Add(time.Second), time.Hour) {
		t.Fatal("forgotten monitor remained delayed")
	}
}
