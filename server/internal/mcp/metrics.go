package mcp

import (
	"context"
	"fmt"

	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/JuanBaez77/status-sv/server/internal/tokens"
)

// Rangos que acepta get_server_metrics. Son fijos a propósito: más fácil de
// pedir para un LLM que dos fechas, y cada uno cae en una resolución conocida.
var metricRanges = map[string]time.Duration{
	"1h":  time.Hour,
	"6h":  6 * time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
	"90d": 90 * 24 * time.Hour,
	"1y":  365 * 24 * time.Hour,
}

// rangeNames es el orden en que se ofrecen los rangos.
var rangeNames = []string{"1h", "6h", "24h", "7d", "30d", "90d", "1y"}

// defaultRange es el rango si no se pide ninguno.
const defaultRange = "1h"

// maxSeriesPoints es el largo máximo de la serie que se devuelve: el contexto
// del LLM es caro, y para ver una tendencia alcanzan.
const maxSeriesPoints = 60

// MetricsInfo son las series de un servidor en un rango, como las lee el panel.
type MetricsInfo struct {
	// Resolution es raw, 1m o 1h, según el largo del rango.
	Resolution string
	From, To   time.Time
	// Truncated es que la consulta llegó a su tope de renglones.
	Truncated bool
	Points    []MetricPoint
	Disks     []DiskSeries
	Networks  []NetworkSeries
}

// MetricPoint es una muestra, o el agregado de un minuto o de una hora: en
// los agregados el valor es el promedio y Max el pico.
type MetricPoint struct {
	At                    time.Time
	CPU, CPUMax           float64
	IOWait, IOWaitMax     float64
	MemUsed, MemUsedMax   int64
	MemTotal              int64
	SwapUsed, SwapUsedMax int64
	SwapTotal             int64
	Load1                 float64
}

// DiskSeries es un disco a lo largo del rango.
type DiskSeries struct {
	Mountpoint string
	Points     []DiskPoint
}

// DiskPoint es el uso de un disco en un momento.
type DiskPoint struct {
	At         time.Time
	UsedBytes  int64
	TotalBytes int64
}

// NetworkSeries es una interfaz a lo largo del rango.
type NetworkSeries struct {
	Interface string
	Points    []NetworkPoint
}

// NetworkPoint son las tasas de una interfaz, en bytes por segundo.
type NetworkPoint struct {
	At        time.Time
	Rx, RxMax float64
	Tx, TxMax float64
}

type serverMetricsInput struct {
	Server string `json:"server" jsonschema:"el servidor: su nombre, como lo da list_servers, o su ID"`
	Range  string `json:"range,omitempty" jsonschema:"el rango hasta ahora; si falta, 1h"`
}

type statOutput struct {
	Last float64 `json:"last"`
	Avg  float64 `json:"avg"`
	Max  float64 `json:"max"`
}

type diskStatOutput struct {
	Mountpoint  string  `json:"mountpoint"`
	LastPercent float64 `json:"last_used_percent"`
	MaxPercent  float64 `json:"max_used_percent"`
	TotalBytes  int64   `json:"total_bytes"`
}

type networkStatOutput struct {
	Interface      string  `json:"interface"`
	RxAvgPerSecond float64 `json:"rx_avg_bytes_per_sec"`
	RxMaxPerSecond float64 `json:"rx_max_bytes_per_sec"`
	TxAvgPerSecond float64 `json:"tx_avg_bytes_per_sec"`
	TxMaxPerSecond float64 `json:"tx_max_bytes_per_sec"`
}

type seriesOutput struct {
	// Bucket es cuánto abarca cada punto de la serie.
	Bucket        string    `json:"bucket"`
	Timestamps    []string  `json:"timestamps"`
	CPUPercent    []float64 `json:"cpu_percent"`
	CPUMaxPercent []float64 `json:"cpu_max_percent"`
	MemoryPercent []float64 `json:"memory_percent"`
}

type serverMetricsOutput struct {
	Server     string `json:"server"`
	Range      string `json:"range"`
	Resolution string `json:"resolution" jsonschema:"de dónde salen los datos: raw (muestras cada 10 s), 1m (agregado por minuto) o 1h (por hora)"`
	From       string `json:"from"`
	To         string `json:"to"`
	// Samples es cuántos puntos había en el rango.
	Samples          int                 `json:"samples"`
	Truncated        bool                `json:"truncated,omitempty" jsonschema:"la consulta llegó a su tope y faltan datos del principio del rango"`
	CPU              *statOutput         `json:"cpu_percent,omitempty"`
	IOWait           *statOutput         `json:"iowait_percent,omitempty"`
	Memory           *statOutput         `json:"memory_percent,omitempty"`
	MemoryTotalBytes int64               `json:"memory_total_bytes,omitempty"`
	Swap             *statOutput         `json:"swap_percent,omitempty" jsonschema:"falta si el servidor no tiene swap"`
	Load1            *statOutput         `json:"load1,omitempty"`
	Disks            []diskStatOutput    `json:"disks"`
	Networks         []networkStatOutput `json:"networks"`
	Series           *seriesOutput       `json:"series,omitempty" jsonschema:"CPU y memoria en 60 puntos como mucho, para ver la tendencia"`
}

func (s *Server) addMetricsTools() {
	addTool(s, &sdk.Tool{
		Name: "get_server_metrics",
		Description: "Métricas de un servidor en un rango hasta ahora (1h, 6h, 24h, 7d, 30d, 90d o 1y): " +
			"último valor, promedio y máximo de CPU, iowait, memoria, swap y carga; el uso de cada disco; " +
			"las tasas de red de cada interfaz; y CPU y memoria en una serie de 60 puntos como mucho.",
		Annotations: readOnly("Métricas de un servidor"),
		InputSchema: metricsInputSchema(),
	}, classRead, s.serverMetrics)
}

// metricsInputSchema es el esquema de la entrada con los rangos como enum,
// que las etiquetas de los campos no pueden decir.
func metricsInputSchema() *jsonschema.Schema {
	schema, err := jsonschema.For[serverMetricsInput](nil)
	if err != nil {
		panic(fmt.Sprintf("esquema de get_server_metrics: %v", err))
	}
	enum := make([]any, len(rangeNames))
	for i, name := range rangeNames {
		enum[i] = name
	}
	schema.Properties["range"].Enum = enum
	return schema
}

func (s *Server) serverMetrics(ctx context.Context, call *Call, in serverMetricsInput) (serverMetricsOutput, error) {
	rangeName := in.Range
	if rangeName == "" {
		rangeName = defaultRange
	}
	span, ok := metricRanges[rangeName]
	if !ok {
		return serverMetricsOutput{}, errInvalid("el rango tiene que ser uno de %s", strings.Join(rangeNames, ", "))
	}
	call.Note("rango", rangeName)

	server, err := s.visibleServer(ctx, call, in.Server)
	if err != nil {
		return serverMetricsOutput{}, err
	}
	call.Target("server", server.ID.String())
	call.Note("servidor", server.Name)

	info, err := s.backend.ServerMetrics(ctx, server.ID, span)
	if err != nil {
		return serverMetricsOutput{}, err
	}
	return summarize(server.Name, rangeName, info), nil
}

// visibleServer busca un servidor por nombre o ID entre los que ve el token.
// Uno que no existe y uno que el token no ve dan el mismo error.
func (s *Server) visibleServer(ctx context.Context, call *Call, ref string) (ServerInfo, error) {
	if !call.Token.CanRead() {
		return ServerInfo{}, errDenied("este token no tiene permiso de lectura: no ve servidores")
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ServerInfo{}, errInvalid("falta el servidor: su nombre o su ID, como los da list_servers")
	}
	servers, err := s.visibleServers(ctx, call)
	if err != nil {
		return ServerInfo{}, err
	}
	id, idErr := uuid.Parse(ref)
	for _, server := range servers {
		if (idErr == nil && server.ID == id) || server.Name == ref {
			return server, nil
		}
	}
	return ServerInfo{}, errNotFound("no hay un servidor %q que este token vea; list_servers da los que hay", ref)
}

// visibleServers devuelve los servidores que el token ve: todos sin lista de
// proyectos, o los de sus proyectos (ADR-025).
func (s *Server) visibleServers(ctx context.Context, call *Call) ([]ServerInfo, error) {
	servers, err := s.backend.Servers(ctx)
	if err != nil {
		return nil, err
	}
	if call.Token.AllProjects() {
		return servers, nil
	}
	projects, err := s.backend.Projects(ctx)
	if err != nil {
		return nil, err
	}
	hosts := map[uuid.UUID]bool{}
	for _, p := range visibleProjects(call.Token, tokens.CapRead, projects) {
		hosts[p.ServerID] = true
	}
	out := make([]ServerInfo, 0, len(servers))
	for _, server := range servers {
		if hosts[server.ID] {
			out = append(out, server)
		}
	}
	return out, nil
}

// summarize arma la respuesta: el resumen de cada métrica y una serie corta.
func summarize(server, rangeName string, info MetricsInfo) serverMetricsOutput {
	out := serverMetricsOutput{
		Server:     server,
		Range:      rangeName,
		Resolution: info.Resolution,
		From:       info.From.UTC().Format(time.RFC3339),
		To:         info.To.UTC().Format(time.RFC3339),
		Samples:    len(info.Points),
		Truncated:  info.Truncated,
		Disks:      []diskStatOutput{},
		Networks:   []networkStatOutput{},
	}

	if n := len(info.Points); n > 0 {
		var cpu, iowait, memory, swap, load stats
		hasSwap := false
		for _, p := range info.Points {
			cpu.add(p.CPU, p.CPUMax)
			iowait.add(p.IOWait, p.IOWaitMax)
			memory.add(ratio(p.MemUsed, p.MemTotal), ratio(p.MemUsedMax, p.MemTotal))
			load.add(p.Load1, p.Load1)
			if p.SwapTotal > 0 {
				hasSwap = true
				swap.add(ratio(p.SwapUsed, p.SwapTotal), ratio(p.SwapUsedMax, p.SwapTotal))
			}
		}
		out.CPU, out.IOWait, out.Memory, out.Load1 = cpu.output(1), iowait.output(1), memory.output(1), load.output(2)
		if hasSwap {
			out.Swap = swap.output(1)
		}
		out.MemoryTotalBytes = info.Points[n-1].MemTotal
		out.Series = downsample(info)
	}

	for _, disk := range info.Disks {
		if len(disk.Points) == 0 {
			continue
		}
		last := disk.Points[len(disk.Points)-1]
		item := diskStatOutput{
			Mountpoint:  disk.Mountpoint,
			LastPercent: round1(ratio(last.UsedBytes, last.TotalBytes)),
			TotalBytes:  last.TotalBytes,
		}
		for _, p := range disk.Points {
			item.MaxPercent = max(item.MaxPercent, round1(ratio(p.UsedBytes, p.TotalBytes)))
		}
		out.Disks = append(out.Disks, item)
	}

	for _, network := range info.Networks {
		if len(network.Points) == 0 {
			continue
		}
		var rx, tx stats
		for _, p := range network.Points {
			rx.add(p.Rx, p.RxMax)
			tx.add(p.Tx, p.TxMax)
		}
		out.Networks = append(out.Networks, networkStatOutput{
			Interface:      network.Interface,
			RxAvgPerSecond: roundRate(rx.avg()),
			RxMaxPerSecond: roundRate(rx.max),
			TxAvgPerSecond: roundRate(tx.avg()),
			TxMaxPerSecond: roundRate(tx.max),
		})
	}
	return out
}

// downsample reparte el rango en hasta 60 tramos iguales y da, por tramo, el
// promedio de CPU y memoria y el pico de CPU. Los tramos sin datos no van.
func downsample(info MetricsInfo) *seriesOutput {
	span := info.To.Sub(info.From)
	bucket := span / maxSeriesPoints
	if bucket <= 0 {
		bucket = time.Second
	}
	out := &seriesOutput{
		Bucket:        bucket.Round(time.Second).String(),
		Timestamps:    []string{},
		CPUPercent:    []float64{},
		CPUMaxPercent: []float64{},
		MemoryPercent: []float64{},
	}

	var cpu, memory stats
	current := -1
	flush := func() {
		if current < 0 || cpu.n == 0 {
			return
		}
		at := info.From.Add(time.Duration(current) * bucket)
		out.Timestamps = append(out.Timestamps, at.UTC().Format(time.RFC3339))
		out.CPUPercent = append(out.CPUPercent, round1(cpu.avg()))
		out.CPUMaxPercent = append(out.CPUMaxPercent, round1(cpu.max))
		out.MemoryPercent = append(out.MemoryPercent, round1(memory.avg()))
	}
	for _, p := range info.Points {
		index := int(p.At.Sub(info.From) / bucket)
		index = min(max(index, 0), maxSeriesPoints-1)
		if index != current {
			flush()
			current, cpu, memory = index, stats{}, stats{}
		}
		cpu.add(p.CPU, p.CPUMax)
		memory.add(ratio(p.MemUsed, p.MemTotal), ratio(p.MemUsedMax, p.MemTotal))
	}
	flush()
	return out
}

// stats acumula último valor, promedio y pico.
type stats struct {
	n         int
	sum, last float64
	max       float64
}

func (s *stats) add(value, peak float64) {
	if s.n == 0 || peak > s.max {
		s.max = peak
	}
	s.n++
	s.sum += value
	s.last = value
}

func (s *stats) avg() float64 {
	if s.n == 0 {
		return 0
	}
	return s.sum / float64(s.n)
}

func (s *stats) output(decimals int) *statOutput {
	round := round1
	if decimals == 2 {
		round = round2
	}
	return &statOutput{Last: round(s.last), Avg: round(s.avg()), Max: round(s.max)}
}

// roundRate redondea una tasa a bytes enteros.
func roundRate(v float64) float64 { return float64(int64(v + 0.5)) }
