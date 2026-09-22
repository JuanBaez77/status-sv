package metrics

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// fakeStore anota las llamadas y puede fallar a propósito.
type fakeStore struct {
	mu      sync.Mutex
	ensured []store.EnsureMetricPartitionsParams
	dropped []store.DropMetricPartitionsParams
	failFor map[string]error
	calls   int
}

func (f *fakeStore) EnsureMetricPartitions(_ context.Context, arg store.EnsureMetricPartitionsParams) (int32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if err := f.failFor[arg.Parent]; err != nil {
		return 0, err
	}
	f.ensured = append(f.ensured, arg)
	return 2, nil
}

func (f *fakeStore) DropMetricPartitions(_ context.Context, arg store.DropMetricPartitionsParams) (int32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropped = append(f.dropped, arg)
	return 1, nil
}

func (f *fakeStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

var now = time.Date(2026, 9, 21, 15, 0, 0, 0, time.UTC)

func newTestPartitions(s PartitionStore) *Partitions {
	p := NewPartitions(s, slog.New(slog.DiscardHandler))
	p.now = func() time.Time { return now }
	return p
}

func TestMaintainCoversEveryTableWithItsRetention(t *testing.T) {
	fake := &fakeStore{}
	result, err := newTestPartitions(fake).Maintain(t.Context())
	if err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if result != (Result{Created: 12, Dropped: 6}) {
		t.Errorf("resultado = %+v, se esperaba 12 creadas y 6 borradas", result)
	}

	want := map[string]struct{ from, to, before time.Time }{
		"metrics_raw":          {now.Add(-48 * time.Hour), now.Add(48 * time.Hour), now.Add(-48 * time.Hour)},
		"metrics_raw_disks":    {now.Add(-48 * time.Hour), now.Add(48 * time.Hour), now.Add(-48 * time.Hour)},
		"metrics_raw_networks": {now.Add(-48 * time.Hour), now.Add(48 * time.Hour), now.Add(-48 * time.Hour)},
		"metrics_1m":           {now.AddDate(0, 0, -30), now.AddDate(0, 0, 32), now.AddDate(0, 0, -30)},
		"metrics_1m_disks":     {now.AddDate(0, 0, -30), now.AddDate(0, 0, 32), now.AddDate(0, 0, -30)},
		"metrics_1m_networks":  {now.AddDate(0, 0, -30), now.AddDate(0, 0, 32), now.AddDate(0, 0, -30)},
	}
	if len(fake.ensured) != len(want) || len(fake.dropped) != len(want) {
		t.Fatalf("se mantuvieron %d y %d tablas, se esperaban %d", len(fake.ensured), len(fake.dropped), len(want))
	}
	for i, ensured := range fake.ensured {
		w, ok := want[ensured.Parent]
		if !ok {
			t.Fatalf("se tocó una tabla que no es de métricas: %q", ensured.Parent)
		}
		if !ensured.FromTs.Equal(w.from) || !ensured.ToTs.Equal(w.to) {
			t.Errorf("%s: se crearon particiones de %v a %v, se esperaba de %v a %v",
				ensured.Parent, ensured.FromTs, ensured.ToTs, w.from, w.to)
		}
		if dropped := fake.dropped[i]; dropped.Parent != ensured.Parent || !dropped.BeforeTs.Equal(w.before) {
			t.Errorf("%s: se borró antes de %v, se esperaba antes de %v", dropped.Parent, dropped.BeforeTs, w.before)
		}
	}
}

// Una tabla que falla no deja a las demás sin mantenimiento.
func TestMaintainKeepsGoingWhenATableFails(t *testing.T) {
	errBroken := errors.New("relation does not exist")
	fake := &fakeStore{failFor: map[string]error{"metrics_raw_disks": errBroken}}

	result, err := newTestPartitions(fake).Maintain(t.Context())
	if !errors.Is(err, errBroken) {
		t.Fatalf("el error no deja ver la causa: %v", err)
	}
	if result.Created != 10 || len(fake.dropped) != 5 {
		t.Errorf("con una tabla rota, se mantuvieron %d y se borró en %d", result.Created/2, len(fake.dropped))
	}
	var tables []string
	for _, e := range fake.ensured {
		tables = append(tables, e.Parent)
	}
	if slices.Contains(tables, "metrics_raw_disks") || !slices.Contains(tables, "metrics_1m_networks") {
		t.Errorf("tablas mantenidas: %v", tables)
	}
}

// Si falla (por ejemplo, porque faltan las migraciones), reintenta pronto en
// vez de esperar una hora.
func TestRunRetriesSoonAfterAFailure(t *testing.T) {
	fake := &fakeStore{failFor: map[string]error{"metrics_raw": errors.New("todavía no hay tablas")}}
	p := newTestPartitions(fake)
	p.every = time.Hour
	p.retry = time.Millisecond

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	deadline := time.Now().Add(5 * time.Second)
	for fake.callCount() < 3*len(policies()) {
		if time.Now().After(deadline) {
			t.Fatalf("no reintentó: %d llamadas", fake.callCount())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}

func TestRunStopsWhenCanceled(t *testing.T) {
	p := newTestPartitions(&fakeStore{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("el mantenimiento no se detuvo al cancelar")
	}
}
