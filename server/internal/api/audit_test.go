package api

import (
	"fmt"
	"testing"
	"time"
)

func TestSparseNotesOncePerPeriod(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	notes := newSparseNotes(15*time.Minute, 10)
	notes.now = func() time.Time { return now }

	if !notes.first("auth.login|203.0.113.7") {
		t.Fatal("la primera vez no se anotó")
	}
	if notes.first("auth.login|203.0.113.7") {
		t.Error("la segunda vez, enseguida, se anotó de nuevo")
	}
	// Otra IP y otra acción son otra cosa.
	if !notes.first("auth.login|203.0.113.8") || !notes.first("agent.enroll|203.0.113.7") {
		t.Error("otra IP u otra acción no se anotaron")
	}

	now = now.Add(15 * time.Minute)
	if !notes.first("auth.login|203.0.113.7") {
		t.Error("pasado el período no se volvió a anotar")
	}
}

// Con el mapa lleno de IPs distintas, las que sobran no se anotan: la tabla
// de auditoría no crece más rápido que ese tope por período.
func TestSparseNotesIsBounded(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	notes := newSparseNotes(15*time.Minute, 3)
	notes.now = func() time.Time { return now }

	for i := range 3 {
		if !notes.first(fmt.Sprintf("auth.login|198.51.100.%d", i)) {
			t.Fatalf("la IP %d no se anotó", i)
		}
	}
	if notes.first("auth.login|198.51.100.99") {
		t.Error("se anotó con el mapa lleno")
	}

	// Cuando vencen las anteriores, hay lugar de nuevo.
	now = now.Add(16 * time.Minute)
	if !notes.first("auth.login|198.51.100.99") {
		t.Error("no se anotó después de vencer las anteriores")
	}
	if len(notes.last) != 1 {
		t.Errorf("quedaron %d anotaciones, se esperaba 1", len(notes.last))
	}
}
