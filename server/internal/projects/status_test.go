package projects

import "testing"

func TestDerive(t *testing.T) {
	tests := []struct {
		name           string
		sourceOK       bool
		total, healthy int
		health         string
		want           Status
	}{
		{name: "todo bien", sourceOK: true, total: 2, healthy: 2, want: StatusOK},
		{name: "todo bien con healthcheck", sourceOK: true, total: 1, healthy: 1, health: "passing", want: StatusOK},
		{name: "healthcheck falla", sourceOK: true, total: 2, healthy: 2, health: "failing", want: StatusDegraded},
		{name: "falta uno", sourceOK: true, total: 3, healthy: 2, want: StatusDegraded},
		{name: "nada anda", sourceOK: true, total: 2, healthy: 0, want: StatusDown},
		{name: "no hay nada", sourceOK: true, total: 0, healthy: 0, want: StatusDown},
		{name: "no hay nada y el healthcheck pasa", sourceOK: true, total: 0, healthy: 0, health: "passing", want: StatusDown},
		{name: "fuente caída", sourceOK: false, total: 2, healthy: 2, want: StatusUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Derive(tt.sourceOK, tt.total, tt.healthy, tt.health); got != tt.want {
				t.Errorf("Derive = %q, se esperaba %q", got, tt.want)
			}
		})
	}
}

func TestContainerCounts(t *testing.T) {
	tests := []struct {
		c               Container
		counts, healthy bool
	}{
		{Container{State: "running"}, true, true},
		{Container{State: "running", Health: "healthy"}, true, true},
		{Container{State: "running", Health: "unhealthy"}, true, false},
		{Container{State: "running", Health: "starting"}, true, false},
		{Container{State: "restarting"}, true, false},
		{Container{State: "dead"}, true, false},
		{Container{State: "paused"}, true, false},
		// Una tarea de una sola vez que terminó no cuenta.
		{Container{State: "exited"}, false, false},
		{Container{State: "created"}, false, false},
	}
	for _, tt := range tests {
		counts, healthy := tt.c.Counts()
		if counts != tt.counts || healthy != tt.healthy {
			t.Errorf("%+v: cuenta %v, bien %v; se esperaba %v, %v", tt.c, counts, healthy, tt.counts, tt.healthy)
		}
	}
}

func TestServiceHealthy(t *testing.T) {
	for state, want := range map[string]bool{"active": true, "reloading": true, "failed": false, "inactive": false, "activating": false} {
		if got := ServiceHealthy(state); got != want {
			t.Errorf("ServiceHealthy(%q) = %v", state, got)
		}
	}
}
