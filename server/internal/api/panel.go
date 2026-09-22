package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/JuanBaez77/status-sv/server/internal/metrics"
	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// Estados de un servidor. No se guardan: se derivan de last_seen_at, como dice
// docs/DISENO.md.
const (
	statusOnline    = "online"
	statusOffline   = "offline"
	statusNeverSeen = "never_seen"
)

// recentWindow es cuánto se mira hacia atrás para la última muestra de la
// lista. Con muestras cada 10 s alcanza de sobra, y evita recorrer particiones
// viejas por un servidor que está caído hace días.
const recentWindow = 15 * time.Minute

// Límites de una consulta de métricas.
const (
	// defaultRange es lo que muestra el panel al abrir un servidor.
	defaultRange = time.Hour
	// maxRange es el rango más largo que se puede pedir de una vez: más que
	// esto no entra en un gráfico ni en la retención del agregado.
	maxRange = 31 * 24 * time.Hour
	// rawRangeLimit es hasta dónde se contesta con muestras crudas. Más allá,
	// con el agregado por minuto.
	rawRangeLimit = 3 * time.Hour
	// maxRows acota cada serie. Un rango de 3 h en crudo son 1080 muestras.
	maxRows = 20000
)

type serverListResponse struct {
	Servers []serverSummary `json:"servers"`
}

type serverSummary struct {
	ID           uuid.UUID     `json:"id"`
	Name         string        `json:"name"`
	Hostname     string        `json:"hostname"`
	Labels       rawJSON       `json:"labels"`
	AgentVersion string        `json:"agent_version"`
	Status       string        `json:"status"`
	LastSeenAt   *time.Time    `json:"last_seen_at"`
	EnrolledAt   time.Time     `json:"enrolled_at"`
	Sample       *latestSample `json:"sample"`
}

// latestSample es el resumen que muestra la lista: lo justo para ver de un
// vistazo si un servidor está en problemas.
type latestSample struct {
	TS             time.Time  `json:"ts"`
	CPUUsedPercent float32    `json:"cpu_used_percent"`
	CPUCores       int16      `json:"cpu_cores"`
	MemTotalBytes  int64      `json:"mem_total_bytes"`
	MemUsedBytes   int64      `json:"mem_used_bytes"`
	Load1          float32    `json:"load1"`
	WorstDisk      *diskUsage `json:"worst_disk"`
}

type diskUsage struct {
	Mountpoint     string  `json:"mountpoint"`
	TotalBytes     int64   `json:"total_bytes"`
	UsedBytes      int64   `json:"used_bytes"`
	AvailableBytes int64   `json:"available_bytes"`
	UsedPercent    float64 `json:"used_percent"`
}

// rawJSON es un JSON que ya viene armado de la base (las etiquetas) y se copia
// tal cual a la respuesta.
type rawJSON []byte

func (r rawJSON) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("{}"), nil
	}
	return r, nil
}

// handleListServers devuelve todos los servidores con su estado y su última
// muestra. Es la vista principal del panel.
func (s *Server) handleListServers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	noStore(w)

	servers, err := s.queries.ListServers(ctx)
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudieron listar los servidores", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}

	now := time.Now().UTC()
	since := now.Add(-recentWindow)

	samples, err := s.queries.ListLatestSamples(ctx, since)
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudieron leer las últimas muestras", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}
	disks, err := s.queries.ListLatestWorstDisks(ctx, since)
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudieron leer los discos", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}

	byServer := make(map[uuid.UUID]store.MetricsRaw, len(samples))
	for _, sample := range samples {
		byServer[sample.ServerID] = sample
	}
	diskByServer := make(map[uuid.UUID]store.MetricsRawDisk, len(disks))
	for _, disk := range disks {
		diskByServer[disk.ServerID] = disk
	}

	list := make([]serverSummary, 0, len(servers))
	for _, server := range servers {
		summary := serverSummary{
			ID:           server.ID,
			Name:         server.Name,
			Hostname:     server.Hostname,
			Labels:       server.Labels,
			AgentVersion: server.AgentVersion,
			Status:       serverStatus(server.LastSeenAt, now, s.cfg.OfflineAfter),
			LastSeenAt:   server.LastSeenAt,
			EnrolledAt:   server.EnrolledAt,
		}
		if sample, ok := byServer[server.ID]; ok {
			summary.Sample = &latestSample{
				TS:             sample.Ts,
				CPUUsedPercent: sample.CpuUsedPercent,
				CPUCores:       sample.CpuCores,
				MemTotalBytes:  sample.MemTotalBytes,
				MemUsedBytes:   sample.MemUsedBytes,
				Load1:          sample.Load1,
			}
			// El disco se muestra solo si es de la misma muestra: uno viejo
			// diría algo que ya no es cierto.
			if disk, ok := diskByServer[server.ID]; ok && disk.Ts.Equal(sample.Ts) {
				summary.Sample.WorstDisk = &diskUsage{
					Mountpoint:     disk.Mountpoint,
					TotalBytes:     disk.TotalBytes,
					UsedBytes:      disk.UsedBytes,
					AvailableBytes: disk.AvailableBytes,
					UsedPercent:    percent(disk.UsedBytes, disk.TotalBytes),
				}
			}
		}
		list = append(list, summary)
	}

	writeJSON(ctx, w, http.StatusOK, serverListResponse{Servers: list})
}

// serverStatus deriva el estado del último contacto.
func serverStatus(lastSeen *time.Time, now time.Time, offlineAfter time.Duration) string {
	switch {
	case lastSeen == nil:
		return statusNeverSeen
	case now.Sub(*lastSeen) > offlineAfter:
		return statusOffline
	default:
		return statusOnline
	}
}

func percent(part, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}

// handleServerMetrics devuelve las series de un servidor en un rango.
//
// La resolución se elige sola: hasta 3 horas, las muestras crudas; más allá, el
// agregado por minuto, que es lo único que queda pasadas las 48 h.
func (s *Server) handleServerMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	noStore(w)

	serverID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: "el ID del servidor no es un UUID"})
		return
	}
	if _, err := s.queries.GetServerByID(ctx, serverID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(ctx, w, http.StatusNotFound, errorResponse{Error: "not_found", Message: "no existe ese servidor"})
			return
		}
		s.log.ErrorContext(ctx, "no se pudo buscar el servidor", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}

	query, err := parseRange(r, time.Now().UTC())
	if err != nil {
		writeJSON(ctx, w, http.StatusBadRequest, errorResponse{Error: "invalid_request", Message: err.Error()})
		return
	}

	series, err := s.readSeries(ctx, serverID, query)
	if err != nil {
		s.log.ErrorContext(ctx, "no se pudieron leer las métricas", "error", err)
		writeJSON(ctx, w, http.StatusInternalServerError, errorResponse{Error: "internal", Message: "error interno"})
		return
	}

	writeJSON(ctx, w, http.StatusOK, series)
}

// MaxRateGap se reexporta desde metrics: las tasas del panel se calculan con la
// misma regla que el agregado por minuto.
const maxRateGap = metrics.MaxRateGap
