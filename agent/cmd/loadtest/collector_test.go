package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/JuanBaez77/status-sv/agent/internal/collector"
)

func TestLaPrimeraLecturaEsSoloLaBase(t *testing.T) {
	t.Parallel()
	c := newSyntheticCollector(1, 2, 2)

	// El colector de verdad necesita dos lecturas de /proc/stat para el CPU y
	// devuelve ErrNoBaseline en la primera: el simulado hace lo mismo, así el
	// muestreo recorre el mismo camino.
	if _, err := c.Collect(context.Background()); !errors.Is(err, collector.ErrNoBaseline) {
		t.Fatalf("la primera lectura devolvió %v, se esperaba ErrNoBaseline", err)
	}
	if c.collected() != 0 {
		t.Errorf("la lectura de base contó como muestra")
	}
}

func TestLasMuestrasInventadasPasanLaValidacion(t *testing.T) {
	t.Parallel()
	c := newSyntheticCollector(7, 3, 2)
	ctx := context.Background()
	_, _ = c.Collect(ctx)

	// Lo que no pasa Validate lo rechaza el control plane: la prueba de carga no
	// mediría nada.
	for i := range 50 {
		sample, err := c.Collect(ctx)
		if err != nil {
			t.Fatalf("muestra %d: %v", i, err)
		}
		if err := sample.Validate(); err != nil {
			t.Fatalf("muestra %d no es válida: %v", i, err)
		}
		if len(sample.Disks) != 3 || len(sample.Networks) != 2 {
			t.Fatalf("muestra %d trae %d discos y %d interfaces", i, len(sample.Disks), len(sample.Networks))
		}
	}
	if c.collected() != 50 {
		t.Errorf("collected() = %d, se esperaban 50", c.collected())
	}
}

func TestLosContadoresDeRedSoloSuben(t *testing.T) {
	t.Parallel()
	c := newSyntheticCollector(3, 1, 2)
	ctx := context.Background()
	_, _ = c.Collect(ctx)

	// El control plane calcula las tasas restando muestras: un contador que baja
	// es un reinicio del servidor, y descarta el punto.
	previous := map[string]uint64{}
	for i := range 30 {
		sample, err := c.Collect(ctx)
		if err != nil {
			t.Fatalf("muestra %d: %v", i, err)
		}
		for _, network := range sample.Networks {
			if network.RxBytes < previous[network.Interface] {
				t.Fatalf("%s bajó de %d a %d", network.Interface, previous[network.Interface], network.RxBytes)
			}
			previous[network.Interface] = network.RxBytes
		}
	}
}

func TestElDiscoNoSePasaDelTotal(t *testing.T) {
	t.Parallel()
	c := newSyntheticCollector(5, 2, 1)
	ctx := context.Background()
	_, _ = c.Collect(ctx)

	for i := range 200 {
		sample, err := c.Collect(ctx)
		if err != nil {
			t.Fatalf("muestra %d: %v", i, err)
		}
		for _, disk := range sample.Disks {
			if disk.UsedBytes > disk.TotalBytes {
				t.Fatalf("%s: usado %d de %d", disk.Mountpoint, disk.UsedBytes, disk.TotalBytes)
			}
			if disk.UsedBytes+disk.AvailableBytes != disk.TotalBytes {
				t.Fatalf("%s: usado %d + libre %d no dan %d",
					disk.Mountpoint, disk.UsedBytes, disk.AvailableBytes, disk.TotalBytes)
			}
		}
	}
}

func TestCadaDiscoTieneSuPuntoDeMontaje(t *testing.T) {
	t.Parallel()
	c := newSyntheticCollector(2, 3, 3)
	ctx := context.Background()
	_, _ = c.Collect(ctx)

	sample, err := c.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}

	vistos := map[string]bool{}
	for _, disk := range sample.Disks {
		if vistos[disk.Mountpoint] {
			t.Fatalf("el punto de montaje %q está repetido", disk.Mountpoint)
		}
		vistos[disk.Mountpoint] = true
	}
	if !vistos["/"] {
		t.Error("ningún disco es la raíz")
	}
	for _, network := range sample.Networks {
		if !strings.HasPrefix(network.Interface, "eth") {
			t.Errorf("interfaz inesperada: %q", network.Interface)
		}
	}
}

func TestLosTokensSeLeenDeLaEntrada(t *testing.T) {
	t.Parallel()

	tokens, err := readTokens(strings.NewReader("  svenr_uno \n\n# un comentario\nsvenr_dos\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 2 || tokens[0] != "svenr_uno" || tokens[1] != "svenr_dos" {
		t.Fatalf("tokens = %q", tokens)
	}

	if _, err := readTokens(strings.NewReader("\n# nada\n")); err == nil {
		t.Error("sin tokens tendría que fallar")
	}
}
