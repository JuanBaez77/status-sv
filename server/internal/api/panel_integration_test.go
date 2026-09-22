//go:build integration

package api_test

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/metrics"
	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// Respuestas del panel, tal como las ve el front.
type serverListBody struct {
	Servers []struct {
		ID         uuid.UUID       `json:"id"`
		Name       string          `json:"name"`
		Status     string          `json:"status"`
		Labels     json.RawMessage `json:"labels"`
		LastSeenAt *time.Time      `json:"last_seen_at"`
		Sample     *struct {
			TS             time.Time `json:"ts"`
			CPUUsedPercent float64   `json:"cpu_used_percent"`
			MemUsedBytes   int64     `json:"mem_used_bytes"`
			WorstDisk      *struct {
				Mountpoint  string  `json:"mountpoint"`
				UsedPercent float64 `json:"used_percent"`
			} `json:"worst_disk"`
		} `json:"sample"`
	} `json:"servers"`
}

type seriesBody struct {
	ServerID   uuid.UUID `json:"server_id"`
	Resolution string    `json:"resolution"`
	Truncated  bool      `json:"truncated"`
	Samples    []struct {
		TS                time.Time `json:"ts"`
		CPUUsedPercent    float64   `json:"cpu_used_percent"`
		CPUUsedMaxPercent float64   `json:"cpu_used_max_percent"`
		MemUsedBytes      int64     `json:"mem_used_bytes"`
	} `json:"samples"`
	Disks []struct {
		Mountpoint string `json:"mountpoint"`
		Points     []struct {
			TS        time.Time `json:"ts"`
			UsedBytes int64     `json:"used_bytes"`
		} `json:"points"`
	} `json:"disks"`
	Networks []struct {
		Interface string `json:"interface"`
		Points    []struct {
			TS       time.Time `json:"ts"`
			RxPerSec float64   `json:"rx_bytes_per_sec"`
		} `json:"points"`
	} `json:"networks"`
}

// login abre una sesión y devuelve su cookie.
func (f *fixture) login(t *testing.T, ip string) *http.Cookie {
	t.Helper()
	f.createUser(t)
	resp := f.do(t, http.MethodPost, "/v1/auth/login",
		`{"username":"juan","password":"`+testPassword+`"}`, ip, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d: %s", resp.StatusCode, resp.body)
	}
	return sessionCookieOf(t, resp)
}

// enrolledServer crea un servidor con su latido y sus particiones de métricas.
func (f *fixture) enrolledServer(t *testing.T, name string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	if _, err := f.queries.CreateServer(t.Context(), store.CreateServerParams{
		ID: id, Name: name, Hostname: name + ".example", Labels: []byte(`{"entorno":"prod"}`), AgentVersion: "0.1.0",
	}); err != nil {
		t.Fatalf("crear el servidor: %v", err)
	}
	if _, err := metrics.NewPartitions(f.queries, slog.New(slog.DiscardHandler)).Maintain(t.Context()); err != nil {
		t.Fatalf("crear las particiones: %v", err)
	}
	return id
}

// ingest guarda muestras como si las hubiera mandado el agent.
func (f *fixture) ingest(t *testing.T, serverID uuid.UUID, samples ...proto.Sample) {
	t.Helper()
	if _, err := metrics.NewIngester(f.pool).Ingest(t.Context(), serverID, samples); err != nil {
		t.Fatalf("ingesta: %v", err)
	}
}

// sample arma una muestra con los valores que le interesan a cada test.
func sample(ts time.Time, cpu float64, memUsedGiB uint64, diskUsedGiB uint64, rx uint64) proto.Sample {
	return proto.Sample{
		TS:     ts,
		CPU:    proto.CPUSample{UsedPercent: cpu, IOWaitPercent: 1, StealPercent: 0, Cores: 4},
		Memory: proto.MemorySample{TotalBytes: 8 << 30, UsedBytes: memUsedGiB << 30, SwapTotalBytes: 2 << 30, SwapUsedBytes: 0},
		Load:   proto.LoadSample{Load1: 0.5, Load5: 0.4, Load15: 0.3},
		Disks: []proto.DiskSample{
			{Mountpoint: "/", TotalBytes: 100 << 30, UsedBytes: diskUsedGiB << 30, AvailableBytes: (90 - diskUsedGiB) << 30},
			{Mountpoint: "/boot", TotalBytes: 1 << 30, UsedBytes: 100 << 20, AvailableBytes: 800 << 20},
		},
		Networks: []proto.NetworkSample{{Interface: "eth0", RxBytes: rx, TxBytes: rx / 2}},
	}
}

func TestPanelNeedsASession(t *testing.T) {
	f := setup(t)
	for _, path := range []string{"/v1/servers", "/v1/servers/" + uuid.Nil.String() + "/metrics"} {
		if resp := f.do(t, http.MethodGet, path, "", "198.51.100.30", nil, nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s sin sesión = %d, se esperaba 401", path, resp.StatusCode)
		}
	}
}

func TestListServers(t *testing.T) {
	f := setup(t)
	cookie := f.login(t, "198.51.100.31")
	now := time.Now().UTC()

	withMetrics := f.enrolledServer(t, "vps-con-metricas")
	silent := f.enrolledServer(t, "vps-callado")

	// El agent late y manda dos muestras; el disco más lleno es "/".
	if _, err := f.queries.TouchServer(t.Context(), store.TouchServerParams{ID: withMetrics}); err != nil {
		t.Fatalf("latido: %v", err)
	}
	f.ingest(t, withMetrics,
		sample(now.Add(-20*time.Second), 10, 2, 40, 1000),
		sample(now.Add(-10*time.Second), 55, 3, 45, 3000),
	)

	resp := f.do(t, http.MethodGet, "/v1/servers", "", "198.51.100.31", cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/servers = %d: %s", resp.StatusCode, resp.body)
	}
	var body serverListBody
	if err := json.Unmarshal(resp.body, &body); err != nil {
		t.Fatalf("leer la respuesta: %v", err)
	}
	if len(body.Servers) != 2 {
		t.Fatalf("%d servidores, se esperaban 2", len(body.Servers))
	}

	// Vienen ordenados por nombre: primero "vps-callado".
	quiet, active := body.Servers[0], body.Servers[1]
	if quiet.ID != silent || active.ID != withMetrics {
		t.Fatalf("orden inesperado: %s, %s", quiet.Name, active.Name)
	}
	if quiet.Status != "never_seen" || quiet.Sample != nil {
		t.Errorf("servidor sin latido = %+v, se esperaba never_seen y sin muestra", quiet)
	}
	if active.Status != "online" || active.LastSeenAt == nil {
		t.Errorf("servidor con latido = estado %q", active.Status)
	}
	if active.Sample == nil {
		t.Fatal("el servidor con métricas no trae su última muestra")
	}
	// La última, no la primera.
	if active.Sample.CPUUsedPercent != 55 || active.Sample.MemUsedBytes != 3<<30 {
		t.Errorf("muestra = %+v; se esperaba la más nueva (CPU 55)", *active.Sample)
	}
	if active.Sample.WorstDisk == nil || active.Sample.WorstDisk.Mountpoint != "/" {
		t.Errorf("disco más lleno = %+v; se esperaba /", active.Sample.WorstDisk)
	}
	if got := active.Sample.WorstDisk.UsedPercent; got < 44.9 || got > 45.1 {
		t.Errorf("uso del disco = %v %%, se esperaba 45 %%", got)
	}
	if string(active.Labels) != `{"entorno": "prod"}` && string(active.Labels) != `{"entorno":"prod"}` {
		t.Errorf("etiquetas = %s", active.Labels)
	}
}

func TestServerMetricsRange(t *testing.T) {
	f := setup(t)
	cookie := f.login(t, "198.51.100.32")
	const ip = "198.51.100.32"
	now := time.Now().UTC().Truncate(time.Minute)

	server := f.enrolledServer(t, "vps-series")
	f.ingest(t, server,
		sample(now.Add(-40*time.Second), 10, 1, 40, 1000),
		sample(now.Add(-30*time.Second), 20, 2, 41, 2000),
		sample(now.Add(-20*time.Second), 90, 3, 42, 5000),
	)

	from := now.Add(-time.Hour).Format(time.RFC3339)
	to := now.Add(time.Minute).Format(time.RFC3339)

	// Crudo: una muestra por envío.
	raw := f.getSeries(t, server, "from="+from+"&to="+to, ip, cookie)
	if raw.Resolution != "raw" || len(raw.Samples) != 3 || raw.Truncated {
		t.Fatalf("crudo = %s con %d muestras", raw.Resolution, len(raw.Samples))
	}
	if raw.Samples[0].CPUUsedPercent != 10 || raw.Samples[2].CPUUsedPercent != 90 {
		t.Errorf("muestras fuera de orden o con otros valores: %+v", raw.Samples)
	}
	if len(raw.Disks) != 2 || raw.Disks[0].Mountpoint != "/" || len(raw.Disks[0].Points) != 3 {
		t.Errorf("discos = %+v; se esperaban / y /boot con 3 puntos", raw.Disks)
	}
	// Tres muestras dan dos pares de tasas: 100 y 300 bytes por segundo.
	if len(raw.Networks) != 1 || len(raw.Networks[0].Points) != 2 {
		t.Fatalf("red = %+v; se esperaban 2 puntos", raw.Networks)
	}
	if raw.Networks[0].Points[0].RxPerSec != 100 || raw.Networks[0].Points[1].RxPerSec != 300 {
		t.Errorf("tasas = %+v; se esperaba 100 y 300", raw.Networks[0].Points)
	}

	// Por minuto: el agregado que dejó la ingesta, con promedio y pico.
	minute := f.getSeries(t, server, "from="+from+"&to="+to+"&resolution=1m", ip, cookie)
	if minute.Resolution != "1m" || len(minute.Samples) == 0 {
		t.Fatalf("por minuto = %s con %d muestras", minute.Resolution, len(minute.Samples))
	}
	last := minute.Samples[len(minute.Samples)-1]
	if last.CPUUsedMaxPercent != 90 || last.CPUUsedPercent == last.CPUUsedMaxPercent {
		t.Errorf("minuto = promedio %v, pico %v; se esperaba pico 90 y promedio menor",
			last.CPUUsedPercent, last.CPUUsedMaxPercent)
	}

	// Un rango largo pasa solo al agregado.
	long := f.getSeries(t, server, "from="+now.Add(-48*time.Hour).Format(time.RFC3339)+"&to="+to, ip, cookie)
	if long.Resolution != "1m" {
		t.Errorf("rango de 48 h = %q, se esperaba 1m", long.Resolution)
	}

	// Un rango sin datos devuelve listas vacías, no null: el front las recorre.
	empty := f.getSeries(t, server,
		"from="+now.Add(-3*time.Hour).Format(time.RFC3339)+"&to="+now.Add(-2*time.Hour).Format(time.RFC3339), ip, cookie)
	if empty.Samples == nil || empty.Disks == nil || empty.Networks == nil {
		t.Errorf("un rango vacío devolvió null: %s", mustJSON(t, empty))
	}
}

func TestServerMetricsRejectsBadRequests(t *testing.T) {
	f := setup(t)
	const ip = "198.51.100.33"
	cookie := f.login(t, ip)
	server := f.enrolledServer(t, "vps-malos-pedidos")

	tests := map[string]struct {
		path string
		want int
	}{
		"ID que no es UUID":      {"/v1/servers/no-es-uuid/metrics", http.StatusBadRequest},
		"servidor inexistente":   {"/v1/servers/" + uuid.Must(uuid.NewV7()).String() + "/metrics", http.StatusNotFound},
		"fecha ilegible":         {"/v1/servers/" + server.String() + "/metrics?from=ayer", http.StatusBadRequest},
		"rango dado vuelta":      {"/v1/servers/" + server.String() + "/metrics?from=2026-09-22T12:00:00Z&to=2026-09-22T11:00:00Z", http.StatusBadRequest},
		"resolución inventada":   {"/v1/servers/" + server.String() + "/metrics?resolution=1s", http.StatusBadRequest},
		"rango de más de un mes": {"/v1/servers/" + server.String() + "/metrics?from=2026-01-01T00:00:00Z&to=2026-09-22T00:00:00Z", http.StatusBadRequest},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if resp := f.do(t, http.MethodGet, tt.path, "", ip, cookie, nil); resp.StatusCode != tt.want {
				t.Errorf("GET %s = %d, se esperaba %d", tt.path, resp.StatusCode, tt.want)
			}
		})
	}
}

func (f *fixture) getSeries(t *testing.T, serverID uuid.UUID, query, ip string, cookie *http.Cookie) seriesBody {
	t.Helper()
	path := fmt.Sprintf("/v1/servers/%s/metrics?%s", serverID, query)
	resp := f.do(t, http.MethodGet, path, "", ip, cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, resp.StatusCode, resp.body)
	}
	var body seriesBody
	if err := json.Unmarshal(resp.body, &body); err != nil {
		t.Fatalf("leer la respuesta: %v", err)
	}
	return body
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("serializar: %v", err)
	}
	return raw
}
