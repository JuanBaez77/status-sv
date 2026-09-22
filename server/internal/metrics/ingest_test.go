package metrics

import (
	"errors"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
)

var serverID = uuid.MustParse("01924f9c-0000-7000-8000-0000000000aa")

func sampleAt(ts time.Time) proto.Sample {
	return proto.Sample{
		TS:     ts,
		CPU:    proto.CPUSample{UsedPercent: 12.5, IOWaitPercent: 0.5, StealPercent: 1, Cores: 2},
		Memory: proto.MemorySample{TotalBytes: 8 << 30, UsedBytes: 3 << 30, SwapTotalBytes: 2 << 30, SwapUsedBytes: 1 << 20},
		Load:   proto.LoadSample{Load1: 0.4, Load5: 0.3, Load15: 0.2},
		Disks: []proto.DiskSample{
			{Mountpoint: "/", TotalBytes: 80 << 30, UsedBytes: 23 << 30, AvailableBytes: 53 << 30},
			{Mountpoint: "/boot", TotalBytes: 1 << 30, UsedBytes: 200 << 20, AvailableBytes: 700 << 20},
		},
		Networks: []proto.NetworkSample{{Interface: "eth0", RxBytes: 1000, TxBytes: 2000}},
	}
}

func TestToColumnsLaysOutEveryTable(t *testing.T) {
	first, second := now.Add(-20*time.Second), now.Add(-10*time.Second)
	// En desorden a propósito: LastTS es la más nueva, no la última del lote.
	c, err := toColumns(serverID, []proto.Sample{sampleAt(second), sampleAt(first)}, now)
	if err != nil {
		t.Fatalf("toColumns: %v", err)
	}

	if !c.lastTS.Equal(second) {
		t.Errorf("LastTS = %v, se esperaba la más nueva (%v)", c.lastTS, second)
	}
	if got := len(c.samples.Ts); got != 2 {
		t.Fatalf("%d filas de muestras, se esperaban 2", got)
	}
	if c.samples.ServerID != serverID || c.disks.ServerID != serverID || c.networks.ServerID != serverID {
		t.Error("alguna tabla quedó sin el ID del servidor")
	}
	if c.samples.CpuUsed[0] != 12.5 || c.samples.CpuSteal[0] != 1 || c.samples.CpuCores[0] != 2 {
		t.Errorf("CPU mal pasado: %+v", c.samples)
	}
	if c.samples.MemUsed[0] != 3<<30 || c.samples.SwapUsed[0] != 1<<20 {
		t.Errorf("memoria mal pasada: %+v", c.samples)
	}

	// Dos discos por muestra: cuatro filas, cada una con la marca de su muestra.
	wantMounts := []string{"/", "/boot", "/", "/boot"}
	if !slices.Equal(c.disks.Mountpoint, wantMounts) {
		t.Errorf("discos = %v, se esperaba %v", c.disks.Mountpoint, wantMounts)
	}
	if !c.disks.Ts[0].Equal(second) || !c.disks.Ts[2].Equal(first) {
		t.Errorf("los discos no quedaron con la marca de su muestra: %v", c.disks.Ts)
	}
	if c.disks.Available[1] != 700<<20 {
		t.Errorf("disponible = %d", c.disks.Available[1])
	}
	if len(c.networks.Interface) != 2 || c.networks.Tx[0] != 2000 {
		t.Errorf("interfaces mal pasadas: %+v", c.networks)
	}

	// Todos los arreglos de una tabla tienen el mismo largo: si no, la
	// consulta no podría recorrerlos en paralelo.
	for _, n := range []int{len(c.samples.CpuIowait), len(c.samples.Load15), len(c.samples.SwapTotal)} {
		if n != len(c.samples.Ts) {
			t.Errorf("columnas de largos distintos: %d y %d", n, len(c.samples.Ts))
		}
	}
}

func TestToColumnsRejectsInvalidBatches(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*proto.Sample)
	}{
		{name: "más vieja que la retención", mutate: func(s *proto.Sample) { s.TS = now.Add(-RawRetention - time.Minute) }},
		{name: "en el futuro", mutate: func(s *proto.Sample) { s.TS = now.Add(MaxClockSkew + time.Second) }},
		{name: "memoria que no entra en bigint", mutate: func(s *proto.Sample) {
			s.Memory.TotalBytes, s.Memory.UsedBytes = math.MaxUint64, 1
		}},
		{name: "disco que no entra en bigint", mutate: func(s *proto.Sample) {
			s.Disks[0] = proto.DiskSample{Mountpoint: "/", TotalBytes: math.MaxInt64 + 1}
		}},
		{name: "contador de red que no entra en bigint", mutate: func(s *proto.Sample) { s.Networks[0].RxBytes = math.MaxInt64 + 1 }},
		{name: "núcleos que no entran en smallint", mutate: func(s *proto.Sample) { s.CPU.Cores = math.MaxInt16 + 1 }},
		{name: "carga que no entra en real", mutate: func(s *proto.Sample) { s.Load.Load1 = math.MaxFloat64 }},
		{name: "muestra que no cumple el contrato", mutate: func(s *proto.Sample) { s.CPU.UsedPercent = 120 }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sample := sampleAt(now.Add(-10 * time.Second))
			tt.mutate(&sample)
			_, err := toColumns(serverID, []proto.Sample{sample}, now)
			if !errors.Is(err, ErrInvalidBatch) {
				t.Fatalf("devolvió %v, se esperaba ErrInvalidBatch", err)
			}
		})
	}
}

func TestToColumnsRejectsEmptyBatches(t *testing.T) {
	if _, err := toColumns(serverID, nil, now); !errors.Is(err, ErrInvalidBatch) {
		t.Fatalf("un lote vacío devolvió %v, se esperaba ErrInvalidBatch", err)
	}
}

// El borde de la ventana: el reloj del agent puede estar hasta MaxClockSkew
// adelantado, y lo guardado llega hasta RawRetention para atrás.
func TestToColumnsAcceptsTheEdgesOfTheWindow(t *testing.T) {
	samples := []proto.Sample{
		sampleAt(now.Add(-RawRetention)),
		sampleAt(now.Add(MaxClockSkew)),
	}
	if _, err := toColumns(serverID, samples, now); err != nil {
		t.Fatalf("los bordes de la ventana se rechazaron: %v", err)
	}
}
