package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/server/internal/store"
)

// Resoluciones que entiende la API.
const (
	resolutionRaw    = "raw"
	resolutionMinute = "1m"
	resolutionHour   = "1h"
)

// rangeQuery es un pedido de métricas ya validado.
type rangeQuery struct {
	from, to   time.Time
	resolution string
}

// seriesResponse son las series de un servidor en un rango.
//
// En resolución 1m cada punto es el promedio del minuto, y los campos *_max son
// el pico: un pico corto no desaparece en el promedio. En 1h es igual, y además
// viajan los *_min de CPU y memoria, el promedio por minuto más bajo de la hora.
// En crudo, promedio y pico son el mismo valor, el de la muestra.
type seriesResponse struct {
	ServerID   uuid.UUID `json:"server_id"`
	Resolution string    `json:"resolution"`
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`
	// Truncated dice que el rango tenía más puntos de los que se devuelven.
	Truncated bool            `json:"truncated"`
	Samples   []samplePoint   `json:"samples"`
	Disks     []diskSeries    `json:"disks"`
	Networks  []networkSeries `json:"networks"`
}

type samplePoint struct {
	TS                  time.Time `json:"ts"`
	CPUUsedPercent      float32   `json:"cpu_used_percent"`
	CPUUsedMaxPercent   float32   `json:"cpu_used_max_percent"`
	CPUUsedMinPercent   *float32  `json:"cpu_used_min_percent,omitempty"`
	CPUIOWaitPercent    float32   `json:"cpu_iowait_percent"`
	CPUIOWaitMaxPercent float32   `json:"cpu_iowait_max_percent"`
	CPUStealPercent     float32   `json:"cpu_steal_percent"`
	CPUCores            int16     `json:"cpu_cores"`
	MemTotalBytes       int64     `json:"mem_total_bytes"`
	MemUsedBytes        int64     `json:"mem_used_bytes"`
	MemUsedMaxBytes     int64     `json:"mem_used_max_bytes"`
	MemUsedMinBytes     *int64    `json:"mem_used_min_bytes,omitempty"`
	SwapTotalBytes      int64     `json:"swap_total_bytes"`
	SwapUsedBytes       int64     `json:"swap_used_bytes"`
	SwapUsedMaxBytes    int64     `json:"swap_used_max_bytes"`
	Load1               float32   `json:"load1"`
	Load5               float32   `json:"load5"`
	Load15              float32   `json:"load15"`
}

type diskSeries struct {
	Mountpoint string      `json:"mountpoint"`
	Points     []diskPoint `json:"points"`
}

// diskPoint, en resolución 1m, es el peor momento del minuto: lo más usado y lo
// menos disponible.
type diskPoint struct {
	// Mountpoint solo viaja en los mensajes en vivo del WebSocket: en las
	// series de la API REST ya lo lleva diskSeries.
	Mountpoint     string    `json:"mountpoint,omitempty"`
	TS             time.Time `json:"ts"`
	TotalBytes     int64     `json:"total_bytes"`
	UsedBytes      int64     `json:"used_bytes"`
	AvailableBytes int64     `json:"available_bytes"`
}

type networkSeries struct {
	Interface string         `json:"interface"`
	Points    []networkPoint `json:"points"`
}

// networkPoint son tasas, no contadores: el panel grafica bytes por segundo.
type networkPoint struct {
	// Interface, igual que Mountpoint en diskPoint, solo viaja en vivo.
	Interface   string    `json:"interface,omitempty"`
	TS          time.Time `json:"ts"`
	RxPerSec    float64   `json:"rx_bytes_per_sec"`
	RxMaxPerSec float64   `json:"rx_max_bytes_per_sec"`
	TxPerSec    float64   `json:"tx_bytes_per_sec"`
	TxMaxPerSec float64   `json:"tx_max_bytes_per_sec"`
}

// parseRange lee from, to y resolution del pedido y los valida.
func parseRange(r *http.Request, now time.Time) (rangeQuery, error) {
	q := rangeQuery{to: now, from: now.Add(-defaultRange)}

	if raw := r.URL.Query().Get("from"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return rangeQuery{}, fmt.Errorf("from tiene que ser una fecha RFC 3339, llegó %q", raw)
		}
		q.from = parsed.UTC()
	}
	if raw := r.URL.Query().Get("to"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return rangeQuery{}, fmt.Errorf("to tiene que ser una fecha RFC 3339, llegó %q", raw)
		}
		q.to = parsed.UTC()
	}

	switch {
	case !q.from.Before(q.to):
		return rangeQuery{}, fmt.Errorf("from (%s) tiene que ser anterior a to (%s)",
			q.from.Format(time.RFC3339), q.to.Format(time.RFC3339))
	case q.to.Sub(q.from) > maxRange:
		return rangeQuery{}, fmt.Errorf("el rango no puede pasar de %v", maxRange)
	}

	switch raw := r.URL.Query().Get("resolution"); raw {
	case "":
		q.resolution = autoResolution(q.to.Sub(q.from))
	case resolutionRaw, resolutionMinute, resolutionHour:
		q.resolution = raw
	default:
		return rangeQuery{}, fmt.Errorf("resolution tiene que ser %q, %q o %q, llegó %q",
			resolutionRaw, resolutionMinute, resolutionHour, raw)
	}

	return q, nil
}

// autoResolution elige la resolución por el largo del rango: hasta 3 h en
// crudo, hasta 48 h por minuto y más por hora. La usan el panel y el MCP.
func autoResolution(span time.Duration) string {
	switch {
	case span <= rawRangeLimit:
		return resolutionRaw
	case span <= minuteRangeLimit:
		return resolutionMinute
	default:
		return resolutionHour
	}
}

// readSeries arma la respuesta leyendo las tres tablas de la resolución pedida.
func (s *Server) readSeries(ctx context.Context, serverID uuid.UUID, q rangeQuery) (seriesResponse, error) {
	out := seriesResponse{
		ServerID:   serverID,
		Resolution: q.resolution,
		From:       q.from,
		To:         q.to,
		Samples:    []samplePoint{},
		Disks:      []diskSeries{},
		Networks:   []networkSeries{},
	}

	switch q.resolution {
	case resolutionRaw:
		return s.readRawSeries(ctx, serverID, q, out)
	case resolutionHour:
		return s.readHourSeries(ctx, serverID, q, out)
	default:
		return s.readMinuteSeries(ctx, serverID, q, out)
	}
}

func (s *Server) readRawSeries(ctx context.Context, serverID uuid.UUID, q rangeQuery, out seriesResponse) (seriesResponse, error) {
	samples, err := s.queries.ListRawSamples(ctx, store.ListRawSamplesParams{
		ServerID: serverID, FromTs: q.from, ToTs: q.to, MaxRows: maxRows,
	})
	if err != nil {
		return seriesResponse{}, fmt.Errorf("leer las muestras: %w", err)
	}
	for _, row := range samples {
		out.Samples = append(out.Samples, samplePoint{
			TS:                  row.Ts,
			CPUUsedPercent:      row.CpuUsedPercent,
			CPUUsedMaxPercent:   row.CpuUsedPercent,
			CPUIOWaitPercent:    row.CpuIowaitPercent,
			CPUIOWaitMaxPercent: row.CpuIowaitPercent,
			CPUStealPercent:     row.CpuStealPercent,
			CPUCores:            row.CpuCores,
			MemTotalBytes:       row.MemTotalBytes,
			MemUsedBytes:        row.MemUsedBytes,
			MemUsedMaxBytes:     row.MemUsedBytes,
			SwapTotalBytes:      row.SwapTotalBytes,
			SwapUsedBytes:       row.SwapUsedBytes,
			SwapUsedMaxBytes:    row.SwapUsedBytes,
			Load1:               row.Load1,
			Load5:               row.Load5,
			Load15:              row.Load15,
		})
	}

	disks, err := s.queries.ListRawDisks(ctx, store.ListRawDisksParams{
		ServerID: serverID, FromTs: q.from, ToTs: q.to, MaxRows: maxRows,
	})
	if err != nil {
		return seriesResponse{}, fmt.Errorf("leer los discos: %w", err)
	}
	for _, row := range disks {
		out.Disks = appendDiskPoint(out.Disks, row.Mountpoint, diskPoint{
			TS: row.Ts, TotalBytes: row.TotalBytes, UsedBytes: row.UsedBytes, AvailableBytes: row.AvailableBytes,
		})
	}

	networks, err := s.queries.ListRawNetworks(ctx, store.ListRawNetworksParams{
		ServerID: serverID, FromTs: q.from, ToTs: q.to, MaxRows: maxRows,
	})
	if err != nil {
		return seriesResponse{}, fmt.Errorf("leer las interfaces: %w", err)
	}
	out.Networks = ratesFromCounters(networks)

	out.Truncated = len(samples) == maxRows || len(disks) == maxRows || len(networks) == maxRows
	return out, nil
}

func (s *Server) readMinuteSeries(ctx context.Context, serverID uuid.UUID, q rangeQuery, out seriesResponse) (seriesResponse, error) {
	samples, err := s.queries.ListMinuteSamples(ctx, store.ListMinuteSamplesParams{
		ServerID: serverID, FromTs: q.from, ToTs: q.to, MaxRows: maxRows,
	})
	if err != nil {
		return seriesResponse{}, fmt.Errorf("leer las muestras por minuto: %w", err)
	}
	for _, row := range samples {
		out.Samples = append(out.Samples, samplePoint{
			TS:                  row.Bucket,
			CPUUsedPercent:      row.CpuUsedAvg,
			CPUUsedMaxPercent:   row.CpuUsedMax,
			CPUIOWaitPercent:    row.CpuIowaitAvg,
			CPUIOWaitMaxPercent: row.CpuIowaitMax,
			CPUStealPercent:     row.CpuStealAvg,
			CPUCores:            row.CpuCores,
			MemTotalBytes:       row.MemTotalBytes,
			MemUsedBytes:        row.MemUsedAvgBytes,
			MemUsedMaxBytes:     row.MemUsedMaxBytes,
			SwapTotalBytes:      row.SwapTotalBytes,
			SwapUsedBytes:       row.SwapUsedAvgBytes,
			SwapUsedMaxBytes:    row.SwapUsedMaxBytes,
			Load1:               row.Load1Avg,
			Load5:               row.Load5Avg,
			Load15:              row.Load15Avg,
		})
	}

	disks, err := s.queries.ListMinuteDisks(ctx, store.ListMinuteDisksParams{
		ServerID: serverID, FromTs: q.from, ToTs: q.to, MaxRows: maxRows,
	})
	if err != nil {
		return seriesResponse{}, fmt.Errorf("leer los discos por minuto: %w", err)
	}
	for _, row := range disks {
		out.Disks = appendDiskPoint(out.Disks, row.Mountpoint, diskPoint{
			TS: row.Bucket, TotalBytes: row.TotalBytes, UsedBytes: row.UsedMaxBytes, AvailableBytes: row.AvailableMinBytes,
		})
	}

	networks, err := s.queries.ListMinuteNetworks(ctx, store.ListMinuteNetworksParams{
		ServerID: serverID, FromTs: q.from, ToTs: q.to, MaxRows: maxRows,
	})
	if err != nil {
		return seriesResponse{}, fmt.Errorf("leer las interfaces por minuto: %w", err)
	}
	for _, row := range networks {
		out.Networks = appendNetworkPoint(out.Networks, row.Interface, networkPoint{
			TS:          row.Bucket,
			RxPerSec:    row.RxAvgPerSec,
			RxMaxPerSec: row.RxMaxPerSec,
			TxPerSec:    row.TxAvgPerSec,
			TxMaxPerSec: row.TxMaxPerSec,
		})
	}

	out.Truncated = len(samples) == maxRows || len(disks) == maxRows || len(networks) == maxRows
	return out, nil
}

func (s *Server) readHourSeries(ctx context.Context, serverID uuid.UUID, q rangeQuery, out seriesResponse) (seriesResponse, error) {
	samples, err := s.queries.ListHourSamples(ctx, store.ListHourSamplesParams{
		ServerID: serverID, FromTs: q.from, ToTs: q.to, MaxRows: maxRows,
	})
	if err != nil {
		return seriesResponse{}, fmt.Errorf("leer las muestras por hora: %w", err)
	}
	for _, row := range samples {
		out.Samples = append(out.Samples, samplePoint{
			TS:                  row.Bucket,
			CPUUsedPercent:      row.CpuUsedAvg,
			CPUUsedMaxPercent:   row.CpuUsedMax,
			CPUUsedMinPercent:   &row.CpuUsedMin,
			CPUIOWaitPercent:    row.CpuIowaitAvg,
			CPUIOWaitMaxPercent: row.CpuIowaitMax,
			CPUStealPercent:     row.CpuStealAvg,
			CPUCores:            row.CpuCores,
			MemTotalBytes:       row.MemTotalBytes,
			MemUsedBytes:        row.MemUsedAvgBytes,
			MemUsedMaxBytes:     row.MemUsedMaxBytes,
			MemUsedMinBytes:     &row.MemUsedMinBytes,
			SwapTotalBytes:      row.SwapTotalBytes,
			SwapUsedBytes:       row.SwapUsedAvgBytes,
			SwapUsedMaxBytes:    row.SwapUsedMaxBytes,
			Load1:               row.Load1Avg,
			Load5:               row.Load5Avg,
			Load15:              row.Load15Avg,
		})
	}

	disks, err := s.queries.ListHourDisks(ctx, store.ListHourDisksParams{
		ServerID: serverID, FromTs: q.from, ToTs: q.to, MaxRows: maxRows,
	})
	if err != nil {
		return seriesResponse{}, fmt.Errorf("leer los discos por hora: %w", err)
	}
	for _, row := range disks {
		out.Disks = appendDiskPoint(out.Disks, row.Mountpoint, diskPoint{
			TS: row.Bucket, TotalBytes: row.TotalBytes, UsedBytes: row.UsedMaxBytes, AvailableBytes: row.AvailableMinBytes,
		})
	}

	networks, err := s.queries.ListHourNetworks(ctx, store.ListHourNetworksParams{
		ServerID: serverID, FromTs: q.from, ToTs: q.to, MaxRows: maxRows,
	})
	if err != nil {
		return seriesResponse{}, fmt.Errorf("leer las interfaces por hora: %w", err)
	}
	for _, row := range networks {
		out.Networks = appendNetworkPoint(out.Networks, row.Interface, networkPoint{
			TS:          row.Bucket,
			RxPerSec:    row.RxAvgPerSec,
			RxMaxPerSec: row.RxMaxPerSec,
			TxPerSec:    row.TxAvgPerSec,
			TxMaxPerSec: row.TxMaxPerSec,
		})
	}

	out.Truncated = len(samples) == maxRows || len(disks) == maxRows || len(networks) == maxRows
	return out, nil
}

// ratesFromCounters pasa los contadores acumulados del crudo a tasas. Las filas
// vienen ordenadas por interfaz y por tiempo.
//
// Cada punto es la tasa entre esa muestra y la anterior. Un contador que baja
// (el servidor se reinició) o un hueco de más de maxRateGap (el agent estuvo
// caído) no son una tasa, y ese punto no sale: es la misma regla que usa el
// agregado por minuto (ADR-009).
func ratesFromCounters(rows []store.ListRawNetworksRow) []networkSeries {
	series := []networkSeries{}

	var previous *store.ListRawNetworksRow
	for i := range rows {
		row := rows[i]
		if previous != nil && previous.Interface == row.Interface {
			seconds := row.Ts.Sub(previous.Ts).Seconds()
			rx, tx := row.RxBytes-previous.RxBytes, row.TxBytes-previous.TxBytes
			if seconds > 0 && seconds <= maxRateGap.Seconds() && rx >= 0 && tx >= 0 {
				rate := float64(rx) / seconds
				txRate := float64(tx) / seconds
				series = appendNetworkPoint(series, row.Interface, networkPoint{
					TS: row.Ts, RxPerSec: rate, RxMaxPerSec: rate, TxPerSec: txRate, TxMaxPerSec: txRate,
				})
			}
		}
		previous = &row
	}
	return series
}

func appendDiskPoint(series []diskSeries, mountpoint string, point diskPoint) []diskSeries {
	if n := len(series); n > 0 && series[n-1].Mountpoint == mountpoint {
		series[n-1].Points = append(series[n-1].Points, point)
		return series
	}
	return append(series, diskSeries{Mountpoint: mountpoint, Points: []diskPoint{point}})
}

func appendNetworkPoint(series []networkSeries, name string, point networkPoint) []networkSeries {
	if n := len(series); n > 0 && series[n-1].Interface == name {
		series[n-1].Points = append(series[n-1].Points, point)
		return series
	}
	return append(series, networkSeries{Interface: name, Points: []networkPoint{point}})
}
