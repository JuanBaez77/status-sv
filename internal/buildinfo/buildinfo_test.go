package buildinfo

import (
	"strings"
	"testing"
)

func TestShortCommit(t *testing.T) {
	original := Commit
	t.Cleanup(func() { Commit = original })

	tests := []struct {
		name   string
		commit string
		want   string
	}{
		{name: "commit largo se trunca a siete", commit: "aeddbdc1234567890", want: "aeddbdc"},
		{name: "commit corto se devuelve entero", commit: "abc12", want: "abc12"},
		{name: "commit de siete queda igual", commit: "aeddbdc", want: "aeddbdc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Commit = tt.commit
			if got := ShortCommit(); got != tt.want {
				t.Errorf("ShortCommit() = %q, se esperaba %q", got, tt.want)
			}
		})
	}
}

func TestBuildDateWithoutInjectedValue(t *testing.T) {
	original := Date
	t.Cleanup(func() { Date = original })

	Date = ""
	if got := BuildDate(); got != unknown {
		t.Errorf("BuildDate() = %q, se esperaba %q", got, unknown)
	}
}

func TestLineIncludesNameAndVersion(t *testing.T) {
	originalVersion, originalCommit := Version, Commit
	t.Cleanup(func() { Version, Commit = originalVersion, originalCommit })

	Version, Commit = "1.2.3", "aeddbdc1234567890"

	line := Line("status-sv-agent")
	for _, want := range []string{"status-sv-agent", "1.2.3", "aeddbdc"} {
		if !strings.Contains(line, want) {
			t.Errorf("Line() = %q, no contiene %q", line, want)
		}
	}
}
