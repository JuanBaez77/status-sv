package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// frame arma un bloque del stream multiplexado de Docker.
func frame(stream byte, text string) []byte {
	header := make([]byte, 8)
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(text))) //nolint:gosec // textos chicos de prueba
	return append(header, text...)
}

func logsServer(t *testing.T, body []byte) (*Client, *http.Request) {
	t.Helper()
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	client := newClient(srv.Client(), srv.URL)
	return client, got
}

func TestLogsDemultiplexes(t *testing.T) {
	var body bytes.Buffer
	body.Write(frame(1, "2026-09-28T10:00:00.000000001Z arrancando\n2026-09-28T10:00:02Z escuchando en :8000\n"))
	body.Write(frame(2, "2026-09-28T10:00:01Z aviso: "))
	// Un renglón partido entre dos bloques del mismo stream.
	body.Write(frame(2, "falta una variable\n"))
	body.Write(frame(1, "sin marca de tiempo\n"))

	var asked *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r
		_, _ = w.Write(body.Bytes())
	}))
	defer srv.Close()

	lines, err := newClient(srv.Client(), srv.URL).Logs(context.Background(), idWeb, 50, false)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	q := asked.URL.Query()
	if q.Get("tail") != "50" || q.Get("timestamps") != "1" || q.Get("stdout") != "1" || q.Get("stderr") != "1" {
		t.Errorf("pedido: %s", asked.URL.RawQuery)
	}

	var got []string
	for _, l := range lines {
		got = append(got, l.Stream+"|"+l.Text)
	}
	want := []string{
		// El renglón sin marca queda primero: no tiene contra qué ordenarse.
		"stdout|sin marca de tiempo",
		"stdout|arrancando",
		"stderr|aviso: falta una variable",
		"stdout|escuchando en :8000",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("renglones:\n%s\nesperaba:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !lines[1].TS.Equal(time.Date(2026, 9, 28, 10, 0, 0, 1, time.UTC)) {
		t.Errorf("marca de tiempo: %v", lines[1].TS)
	}
}

// Con terminal, Docker manda el texto tal cual, sin encabezados.
func TestLogsWithTTY(t *testing.T) {
	client, _ := logsServer(t, []byte("2026-09-28T10:00:00Z hola\r\n2026-09-28T10:00:01Z chau\r\n"))
	lines, err := client.Logs(context.Background(), idWeb, 10, true)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(lines) != 2 || lines[0].Text != "hola" || lines[1].Text != "chau" || lines[0].Stream != "stdout" {
		t.Errorf("renglones: %+v", lines)
	}
}

// Un stream cortado a la mitad (el tope de bytes) devuelve lo que se leyó entero.
func TestLogsTruncatedStream(t *testing.T) {
	body := append(frame(1, "2026-09-28T10:00:00Z completo\n"), frame(1, "2026-09-28T10:00:01Z cortado a la mitad\n")[:14]...)
	client, _ := logsServer(t, body)
	lines, err := client.Logs(context.Background(), idWeb, 10, false)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(lines) != 1 || lines[0].Text != "completo" {
		t.Errorf("renglones: %+v", lines)
	}
}
