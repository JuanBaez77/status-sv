package proto_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
)

// TestValidate recorre cada mensaje en su versión válida y en varias inválidas.
// Lo que se prueba es la frontera: qué entra desde la red y qué se rechaza.
func TestValidate(t *testing.T) {
	serverID := uuid.MustParse("01924f9c-0000-7000-8000-000000000001")
	validConfig := proto.DefaultAgentConfig()

	tests := []struct {
		name    string
		msg     proto.Message
		wantErr bool
	}{
		{name: "enroll válido", msg: validEnroll()},
		{name: "enroll sin token", msg: proto.Enroll{Hostname: "h", AgentVersion: "0.1.0"}, wantErr: true},
		{name: "enroll con token corto", msg: proto.Enroll{Token: "corto", Hostname: "h", AgentVersion: "0.1.0"}, wantErr: true},
		{name: "enroll sin hostname", msg: proto.Enroll{Token: validEnroll().Token, AgentVersion: "0.1.0"}, wantErr: true},

		{
			name: "enroll_ack válido",
			msg:  proto.EnrollAck{ServerID: serverID, ServerName: "vps-01", Credential: validEnroll().Token, Config: validConfig},
		},
		{
			name:    "enroll_ack sin servidor",
			msg:     proto.EnrollAck{ServerName: "vps-01", Credential: validEnroll().Token, Config: validConfig},
			wantErr: true,
		},
		{
			name:    "enroll_ack con configuración fuera de rango",
			msg:     proto.EnrollAck{ServerID: serverID, ServerName: "vps-01", Credential: validEnroll().Token, Config: proto.AgentConfig{HeartbeatSeconds: 1, MetricsIntervalSeconds: 10, MetricsBatchSeconds: 10}},
			wantErr: true,
		},

		{name: "heartbeat válido", msg: proto.Heartbeat{AgentVersion: "0.1.0", UptimeSeconds: 120, SelfMemoryBytes: 20 << 20, SelfCPUPercent: 0.4}},
		{name: "heartbeat sin versión", msg: proto.Heartbeat{UptimeSeconds: 1}, wantErr: true},
		{name: "heartbeat con CPU imposible", msg: proto.Heartbeat{AgentVersion: "0.1.0", SelfCPUPercent: 120}, wantErr: true},
		{name: "heartbeat con uptime negativo", msg: proto.Heartbeat{AgentVersion: "0.1.0", UptimeSeconds: -1}, wantErr: true},

		{name: "heartbeat_ack válido", msg: proto.HeartbeatAck{ServerTime: time.Now().UTC()}},
		{name: "heartbeat_ack sin hora", msg: proto.HeartbeatAck{}, wantErr: true},

		{name: "metrics válido", msg: proto.Metrics{Samples: []proto.Sample{validSample()}}},
		{name: "metrics sin muestras", msg: proto.Metrics{}, wantErr: true},
		{name: "metrics con lote enorme", msg: proto.Metrics{Samples: tooManySamples()}, wantErr: true},

		{name: "metrics_ack válido", msg: proto.MetricsAck{Accepted: 3, LastTS: time.Now().UTC()}},
		{name: "metrics_ack sin nada aceptado", msg: proto.MetricsAck{}},
		{name: "metrics_ack aceptando sin marca de tiempo", msg: proto.MetricsAck{Accepted: 3}, wantErr: true},

		{name: "error válido", msg: proto.NewError(proto.CodeInvalidToken, "el token no existe o ya se usó", serverID)},
		{name: "error con código inventado", msg: proto.ErrorMessage{Code: "vaya_a_saber", Message: "x"}, wantErr: true},
		{name: "error sin texto", msg: proto.ErrorMessage{Code: proto.CodeInternal}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.msg.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("se esperaba un error y no hubo")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("no se esperaba error: %v", err)
			}
		})
	}
}

func validSample() proto.Sample {
	return proto.Sample{
		TS:     time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC),
		CPU:    proto.CPUSample{UsedPercent: 12.5, Cores: 4},
		Memory: proto.MemorySample{TotalBytes: 8 << 30, UsedBytes: 3 << 30, SwapTotalBytes: 2 << 30, SwapUsedBytes: 0},
		Load:   proto.LoadSample{Load1: 0.31, Load5: 0.22, Load15: 0.18},
		Disks: []proto.DiskSample{
			{Mountpoint: "/", TotalBytes: 80 << 30, UsedBytes: 21 << 30},
		},
		Networks: []proto.NetworkSample{
			{Interface: "eth0", RxBytes: 981_234_567, TxBytes: 123_456_789},
		},
	}
}

func tooManySamples() []proto.Sample {
	samples := make([]proto.Sample, proto.MaxSamplesPerBatch+1)
	for i := range samples {
		samples[i] = validSample()
	}
	return samples
}

func TestSampleValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*proto.Sample)
	}{
		{name: "sin marca de tiempo", mutate: func(s *proto.Sample) { s.TS = time.Time{} }},
		{name: "cpu fuera de rango", mutate: func(s *proto.Sample) { s.CPU.UsedPercent = 101 }},
		{name: "memoria usada mayor que la total", mutate: func(s *proto.Sample) { s.Memory.UsedBytes = s.Memory.TotalBytes + 1 }},
		{name: "swap usado mayor que el total", mutate: func(s *proto.Sample) { s.Memory.SwapUsedBytes = s.Memory.SwapTotalBytes + 1 }},
		{name: "carga negativa", mutate: func(s *proto.Sample) { s.Load.Load5 = -1 }},
		{name: "disco sin punto de montaje", mutate: func(s *proto.Sample) { s.Disks[0].Mountpoint = "" }},
		{name: "disco con uso imposible", mutate: func(s *proto.Sample) { s.Disks[0].UsedBytes = s.Disks[0].TotalBytes + 1 }},
		{name: "interfaz sin nombre", mutate: func(s *proto.Sample) { s.Networks[0].Interface = "" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sample := validSample()
			tt.mutate(&sample)
			if err := sample.Validate(); err == nil {
				t.Fatal("se esperaba un error de validación")
			}
		})
	}
}

func TestDefaultAgentConfigIsValid(t *testing.T) {
	config := proto.DefaultAgentConfig()
	if err := config.Validate(); err != nil {
		t.Fatalf("la configuración por defecto no valida: %v", err)
	}
	if config.Heartbeat() != 15*time.Second {
		t.Errorf("el latido por defecto es %v, se esperaban 15s", config.Heartbeat())
	}
}

func TestAgentConfigRejectsIncoherentIntervals(t *testing.T) {
	// Un lote más corto que el intervalo de muestreo no tiene sentido.
	config := proto.AgentConfig{HeartbeatSeconds: 15, MetricsIntervalSeconds: 30, MetricsBatchSeconds: 10}
	if err := config.Validate(); err == nil {
		t.Fatal("se aceptó un lote más corto que el intervalo de muestreo")
	}
}

func TestErrorRetryable(t *testing.T) {
	tests := map[proto.ErrorCode]bool{
		proto.CodeInvalidToken:       false,
		proto.CodeExpiredToken:       false,
		proto.CodeUnauthorized:       false,
		proto.CodeUnsupportedVersion: false,
		proto.CodeInvalidMessage:     false,
		proto.CodeRateLimited:        true,
		proto.CodeInternal:           true,
	}

	for code, want := range tests {
		msg := proto.ErrorMessage{Code: code, Message: "x"}
		if got := msg.Retryable(); got != want {
			t.Errorf("Retryable() de %q = %v, se esperaba %v", code, got, want)
		}
	}
}

func TestNewErrorOmitsNilReply(t *testing.T) {
	msg := proto.NewError(proto.CodeInternal, "algo falló", uuid.Nil)
	if msg.InReplyTo != nil {
		t.Errorf("se guardó un ID de respuesta nulo: %v", msg.InReplyTo)
	}
}

// La muestra más grande que acepta el contrato tiene que entrar sola en un
// mensaje: si no, el agent no podría mandarla nunca y trabaría su buffer.
func TestLargestValidSampleFitsInAMessage(t *testing.T) {
	// "<" es el peor caso: encoding/json lo escapa como \u003c, seis bytes.
	name := strings.Repeat("<", proto.MaxDeviceNameLen)
	sample := validSample()
	sample.Disks = make([]proto.DiskSample, proto.MaxDisksPerSample)
	for i := range sample.Disks {
		sample.Disks[i] = proto.DiskSample{Mountpoint: name, TotalBytes: math.MaxUint64, UsedBytes: math.MaxUint64}
	}
	sample.Networks = make([]proto.NetworkSample, proto.MaxNetworksPerSample)
	for i := range sample.Networks {
		sample.Networks[i] = proto.NetworkSample{Interface: name, RxBytes: math.MaxUint64, TxBytes: math.MaxUint64}
	}

	raw, err := proto.Marshal(proto.Metrics{Samples: []proto.Sample{sample}})
	if err != nil {
		t.Fatalf("serializar: %v", err)
	}
	if len(raw) > proto.MaxMessageBytes {
		t.Errorf("la muestra más grande ocupa %d bytes y el tope es %d", len(raw), proto.MaxMessageBytes)
	}
	t.Logf("la muestra más grande ocupa %d bytes de %d", len(raw), proto.MaxMessageBytes)
}
