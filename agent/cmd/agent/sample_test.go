package main

import (
	"context"
	"testing"
)

func TestRunSampleRejectsIntervalsOutOfRange(t *testing.T) {
	for _, interval := range []string{"10ms", "2m", "-1s"} {
		t.Run(interval, func(t *testing.T) {
			if err := runSample(context.Background(), []string{"--interval", interval}); err == nil {
				t.Fatalf("se aceptó --interval %s", interval)
			}
		})
	}
}
