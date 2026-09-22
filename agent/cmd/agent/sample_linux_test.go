//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/JuanBaez77/status-sv/agent/internal/collector"
	"github.com/JuanBaez77/status-sv/proto"
)

// Lo que imprime `agent sample` tiene que ser una muestra que el control plane
// aceptaría, porque es lo que se usa para diagnosticar qué ve el agent.
func TestPrintSampleWritesAValidSample(t *testing.T) {
	var out, warnings bytes.Buffer

	err := printSample(context.Background(), collector.New(), 300*time.Millisecond, &out, &warnings)
	if err != nil {
		t.Fatalf("printSample: %v", err)
	}
	if warnings.Len() > 0 {
		t.Logf("avisos (dependen de la máquina):\n%s", warnings.String())
	}

	var sample proto.Sample
	decoder := json.NewDecoder(&out)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&sample); err != nil {
		t.Fatalf("la salida no es una muestra: %v\n%s", err, out.String())
	}
	if err := sample.Validate(); err != nil {
		t.Errorf("la muestra impresa no cumple el contrato: %v", err)
	}
}

func TestPrintSampleStopsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	var out, warnings bytes.Buffer
	start := time.Now()
	if err := printSample(ctx, collector.New(), time.Minute, &out, &warnings); err == nil {
		t.Fatal("se esperaba un error al cancelar")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("tardó %v en cortar: no respetó la cancelación", elapsed)
	}
}
