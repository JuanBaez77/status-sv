package proto_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
)

func logsCommand(kind proto.LogKind, target string, lines int) proto.Command {
	return proto.Command{Operation: proto.OpGetLogs, GetLogs: &proto.GetLogsParams{Kind: kind, Target: target, Lines: lines}}
}

func TestCommandValidate(t *testing.T) {
	id := strings.Repeat("ab", 32)
	tests := []struct {
		name    string
		cmd     proto.Command
		wantErr bool
	}{
		{name: "logs de un contenedor", cmd: logsCommand(proto.LogContainer, id, 200)},
		{name: "logs de un servicio", cmd: logsCommand(proto.LogService, "gunicorn@cristales.service", 1)},
		{name: "sin operación", cmd: proto.Command{}, wantErr: true},
		{name: "operación que no existe", cmd: proto.Command{Operation: "run_shell"}, wantErr: true},
		{name: "get_logs sin parámetros", cmd: proto.Command{Operation: proto.OpGetLogs}, wantErr: true},
		{name: "contenedor por nombre y no por ID", cmd: logsCommand(proto.LogContainer, "web", 10), wantErr: true},
		{name: "servicio que no es .service", cmd: logsCommand(proto.LogService, "nginx.socket", 10), wantErr: true},
		{name: "servicio con espacio", cmd: logsCommand(proto.LogService, "a b.service", 10), wantErr: true},
		// Nada que se parezca a una opción de journalctl puede pasar como unidad.
		{name: "servicio que empieza como opción", cmd: logsCommand(proto.LogService, "--since=yesterday.service", 10), wantErr: true},
		{name: "tipo desconocido", cmd: logsCommand("archivo", "/etc/shadow", 10), wantErr: true},
		{name: "cero renglones", cmd: logsCommand(proto.LogService, "nginx.service", 0), wantErr: true},
		{name: "demasiados renglones", cmd: logsCommand(proto.LogService, "nginx.service", proto.MaxLogLines+1), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cmd.Validate()
			if tt.wantErr != (err != nil) {
				t.Fatalf("error = %v, se esperaba error: %v", err, tt.wantErr)
			}
		})
	}
}

func TestUnknownOperationIsRecognizable(t *testing.T) {
	err := proto.Command{Operation: "exec"}.Validate()
	if !errors.Is(err, proto.ErrUnknownOperation) {
		t.Fatalf("esperaba ErrUnknownOperation, obtuve %v", err)
	}
}

func TestCommandAckValidate(t *testing.T) {
	id := uuid.MustParse("01924f9c-0000-7000-8000-000000000001")
	logs := &proto.LogsResult{Lines: []proto.LogLine{{TS: time.Now(), Stream: "stderr", Text: "error: sin conexión"}}}
	tests := []struct {
		name    string
		ack     proto.CommandAck
		wantErr bool
	}{
		{name: "logs", ack: proto.CommandAck{CommandID: id, Status: proto.CommandOK, Logs: logs}},
		{name: "rechazado", ack: proto.CommandAck{CommandID: id, Status: proto.CommandRejected, Code: proto.CommandUnknownTarget, Message: "no existe"}},
		{name: "sin ID", ack: proto.CommandAck{Status: proto.CommandOK}, wantErr: true},
		{name: "ok con código", ack: proto.CommandAck{CommandID: id, Status: proto.CommandOK, Code: proto.CommandBusy}, wantErr: true},
		{name: "falla sin código", ack: proto.CommandAck{CommandID: id, Status: proto.CommandFailed}, wantErr: true},
		{name: "falla con resultado", ack: proto.CommandAck{CommandID: id, Status: proto.CommandFailed, Code: proto.CommandInternal, Logs: logs}, wantErr: true},
		{name: "estado inventado", ack: proto.CommandAck{CommandID: id, Status: "masomenos"}, wantErr: true},
		{
			name:    "renglón con escape de terminal",
			ack:     proto.CommandAck{CommandID: id, Status: proto.CommandOK, Logs: &proto.LogsResult{Lines: []proto.LogLine{{Text: "\x1b[31mrojo"}}}},
			wantErr: true,
		},
		{
			name:    "renglón demasiado largo",
			ack:     proto.CommandAck{CommandID: id, Status: proto.CommandOK, Logs: &proto.LogsResult{Lines: []proto.LogLine{{Text: strings.Repeat("x", proto.MaxLogLineLen+1)}}}},
			wantErr: true,
		},
		{
			name:    "stream inventado",
			ack:     proto.CommandAck{CommandID: id, Status: proto.CommandOK, Logs: &proto.LogsResult{Lines: []proto.LogLine{{Stream: "stdin", Text: "x"}}}},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.ack.Validate()
			if tt.wantErr != (err != nil) {
				t.Fatalf("error = %v, se esperaba error: %v", err, tt.wantErr)
			}
		})
	}
}
