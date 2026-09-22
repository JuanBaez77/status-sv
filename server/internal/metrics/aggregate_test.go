package metrics

import (
	"testing"
	"time"
)

func TestMinuteSpanCoversWholeMinutesInUTC(t *testing.T) {
	// Buenos Aires está en UTC−3: el corte tiene que ser el mismo minuto UTC.
	art := time.FixedZone("ART", -3*60*60)

	tests := []struct {
		name        string
		first, last time.Time
		from, to    time.Time
	}{
		{
			name:  "dentro de un minuto",
			first: time.Date(2026, 9, 21, 12, 0, 10, 0, time.UTC),
			last:  time.Date(2026, 9, 21, 12, 0, 50, 0, time.UTC),
			from:  time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC),
			to:    time.Date(2026, 9, 21, 12, 1, 0, 0, time.UTC),
		},
		{
			name:  "cruza el minuto",
			first: time.Date(2026, 9, 21, 12, 0, 50, 0, time.UTC),
			last:  time.Date(2026, 9, 21, 12, 1, 0, 0, time.UTC),
			from:  time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC),
			to:    time.Date(2026, 9, 21, 12, 2, 0, 0, time.UTC),
		},
		{
			name:  "otra zona horaria",
			first: time.Date(2026, 9, 21, 9, 0, 30, 500, art),
			last:  time.Date(2026, 9, 21, 9, 0, 30, 500, art),
			from:  time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC),
			to:    time.Date(2026, 9, 21, 12, 1, 0, 0, time.UTC),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			from, to := minuteSpan(tt.first, tt.last)
			if !from.Equal(tt.from) || !to.Equal(tt.to) {
				t.Errorf("minuteSpan = [%v, %v), se esperaba [%v, %v)", from, to, tt.from, tt.to)
			}
			if from.Location() != time.UTC {
				t.Errorf("el corte quedó en %v, se esperaba UTC", from.Location())
			}
		})
	}
}
