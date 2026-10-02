package proto_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
)

const (
	shaV1 = "0123456789abcdef0123456789abcdef01234567"
	shaV2 = "89abcdef0123456789abcdef0123456789abcdef"
)

// systemdDeployEvent es el deploy de un proyecto systemd que falló al
// verificar y volvió atrás (ADR-019): los pasos declarados no llevan servicio,
// hay un build por cada paso de deploy.build y un reinicio en lugar del up.
func systemdDeployEvent() proto.DeployEvent {
	start := goldenTS
	at := func(s int) time.Time { return start.Add(time.Duration(s) * time.Second) }
	return proto.DeployEvent{
		DeployID:       goldenID,
		Seq:            14,
		Project:        "ypf-aca",
		Kind:           proto.DeployKindDeploy,
		Status:         proto.DeployRolledBack,
		Commit:         shaV2,
		PreviousCommit: shaV1,
		StartedAt:      start,
		FinishedAt:     at(190),
		Code:           proto.DeployUnhealthy,
		Message:        "no quedó bien en 120 s: gunicorn-ypf-aca.service se reinició 3 veces",
		Steps: []proto.DeployStep{
			{Name: proto.StepPreflight, Status: proto.StepOK, StartedAt: at(0), FinishedAt: at(0)},
			{Name: proto.StepFetch, Status: proto.StepOK, StartedAt: at(0), FinishedAt: at(2), Detail: "main en 89abcdef"},
			{Name: proto.StepSnapshot, Status: proto.StepOK, StartedAt: at(2), FinishedAt: at(2), Detail: "01234567"},
			{Name: proto.StepCheckout, Status: proto.StepOK, StartedAt: at(2), FinishedAt: at(3)},
			{
				Name: proto.StepBuild, Status: proto.StepOK, StartedAt: at(3), FinishedAt: at(40),
				Detail: ".venv/bin/pip install -r requirements.txt", Output: []string{"Successfully installed gunicorn-23.0.0"},
			},
			{Name: proto.StepBuild, Status: proto.StepOK, StartedAt: at(40), FinishedAt: at(45), Detail: ".venv/bin/python manage.py collectstatic --noinput"},
			{Name: proto.StepPre, Status: proto.StepOK, StartedAt: at(45), FinishedAt: at(50), Detail: ".venv/bin/python manage.py migrate --noinput"},
			{Name: proto.StepRestart, Status: proto.StepOK, StartedAt: at(50), FinishedAt: at(52), Detail: "gunicorn-ypf-aca.service"},
			{Name: proto.StepHealthcheck, Status: proto.StepFailed, StartedAt: at(52), FinishedAt: at(172), Detail: "no quedó bien en 120 s"},
			{Name: proto.StepRollback, Status: proto.StepOK, StartedAt: at(172), FinishedAt: at(190), Detail: "volvió a 01234567"},
		},
	}
}

// bootstrapDeployEvent es un primer deploy que levantó el proyecto, no quedó
// bien y bajó lo que había levantado (ADR-021): sin foto de lo anterior ni
// commit anterior.
func bootstrapDeployEvent() proto.DeployEvent {
	start := goldenTS
	at := func(s int) time.Time { return start.Add(time.Duration(s) * time.Second) }
	return proto.DeployEvent{
		DeployID:   goldenID,
		Seq:        11,
		Project:    "turnos",
		Kind:       proto.DeployKindBootstrap,
		Status:     proto.DeployRolledBack,
		Commit:     shaV2,
		StartedAt:  start,
		FinishedAt: at(215),
		Code:       proto.DeployUnhealthy,
		Message:    "no quedó bien en 120 s: turnos-backend-1: terminó con código 1",
		Steps: []proto.DeployStep{
			{Name: proto.StepPreflight, Status: proto.StepOK, StartedAt: at(0), FinishedAt: at(0), Detail: "workdir sin clonar; compose 2.39.1"},
			{Name: proto.StepFetch, Status: proto.StepOK, StartedAt: at(0), FinishedAt: at(4), Detail: "main en 89abcdef"},
			{Name: proto.StepCheckout, Status: proto.StepOK, StartedAt: at(4), FinishedAt: at(5), Detail: "89abcdef"},
			{Name: proto.StepBuild, Status: proto.StepOK, StartedAt: at(5), FinishedAt: at(80)},
			{Name: proto.StepUp, Status: proto.StepOK, StartedAt: at(80), FinishedAt: at(88)},
			{Name: proto.StepHealthcheck, Status: proto.StepFailed, StartedAt: at(88), FinishedAt: at(208), Detail: "no quedó bien en 120 s"},
			{Name: proto.StepRollback, Status: proto.StepOK, StartedAt: at(208), FinishedAt: at(215), Detail: "bajó lo que había levantado; el código quedó en el workdir"},
		},
	}
}

// validDeployEvent es un deploy que terminó bien, con todos los pasos.
func validDeployEvent() proto.DeployEvent {
	start := goldenTS
	at := func(s int) time.Time { return start.Add(time.Duration(s) * time.Second) }
	return proto.DeployEvent{
		DeployID:       goldenID,
		Seq:            9,
		Project:        "cristales-baez",
		Kind:           proto.DeployKindDeploy,
		Status:         proto.DeploySucceeded,
		Commit:         shaV2,
		PreviousCommit: shaV1,
		StartedAt:      start,
		FinishedAt:     at(95),
		Steps: []proto.DeployStep{
			{Name: proto.StepPreflight, Status: proto.StepOK, StartedAt: at(0), FinishedAt: at(0)},
			{Name: proto.StepFetch, Status: proto.StepOK, StartedAt: at(0), FinishedAt: at(2), Detail: "main en 89abcdef"},
			{Name: proto.StepSnapshot, Status: proto.StepOK, StartedAt: at(2), FinishedAt: at(2), Detail: "3 imágenes anotadas"},
			{Name: proto.StepCheckout, Status: proto.StepOK, StartedAt: at(2), FinishedAt: at(3)},
			{Name: proto.StepBuild, Status: proto.StepOK, StartedAt: at(3), FinishedAt: at(70), Output: []string{"#8 naming to docker.io/library/cristales-baez-backend done"}},
			{
				Name: proto.StepPre, Service: "backend", Status: proto.StepOK, StartedAt: at(70), FinishedAt: at(80),
				Output: []string{"Applying pedidos.0042_estado... OK", "DATABASE_URL=postgres://app:****@db/app"},
			},
			{Name: proto.StepUp, Status: proto.StepOK, StartedAt: at(80), FinishedAt: at(85)},
			{Name: proto.StepHealthcheck, Status: proto.StepOK, StartedAt: at(85), FinishedAt: at(95), Detail: "3 contenedores sanos y 200 tres veces"},
		},
	}
}

func TestDeployParamsValidate(t *testing.T) {
	tests := []struct {
		name    string
		params  proto.DeployParams
		wantErr bool
	}{
		{name: "punta de la rama", params: proto.DeployParams{Project: "cristales-baez"}},
		{name: "commit exacto", params: proto.DeployParams{Project: "cristales-baez", Commit: shaV2}},
		{name: "proyecto con mayúsculas", params: proto.DeployParams{Project: "Cristales"}, wantErr: true},
		{name: "proyecto que parece una ruta", params: proto.DeployParams{Project: "../etc"}, wantErr: true},
		{name: "commit abreviado", params: proto.DeployParams{Project: "x", Commit: shaV2[:12]}, wantErr: true},
		{name: "commit en mayúsculas", params: proto.DeployParams{Project: "x", Commit: strings.ToUpper(shaV2)}, wantErr: true},
		// Nada que git pudiera leer como opción o como otra referencia.
		{name: "nombre de rama", params: proto.DeployParams{Project: "x", Commit: "main"}, wantErr: true},
		{name: "opción de git", params: proto.DeployParams{Project: "x", Commit: "--upload-pack=touch${IFS}x" + strings.Repeat("a", 14)}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.params.Validate()
			if tt.wantErr != (err != nil) {
				t.Fatalf("error = %v, se esperaba error: %v", err, tt.wantErr)
			}
		})
	}
}

func TestDeployCommandValidate(t *testing.T) {
	tests := []struct {
		name    string
		cmd     proto.Command
		wantErr bool
	}{
		{name: "deploy", cmd: proto.Command{Operation: proto.OpDeploy, Deploy: &proto.DeployParams{Project: "web"}}},
		{name: "rollback", cmd: proto.Command{Operation: proto.OpRollback, Rollback: &proto.RollbackParams{Project: "web"}}},
		{name: "deploy sin parámetros", cmd: proto.Command{Operation: proto.OpDeploy}, wantErr: true},
		{name: "rollback sin parámetros", cmd: proto.Command{Operation: proto.OpRollback}, wantErr: true},
		{
			name:    "deploy con los parámetros de rollback",
			cmd:     proto.Command{Operation: proto.OpDeploy, Rollback: &proto.RollbackParams{Project: "web"}},
			wantErr: true,
		},
		{
			name: "parámetros de dos operaciones",
			cmd: proto.Command{
				Operation: proto.OpDeploy,
				Deploy:    &proto.DeployParams{Project: "web"},
				GetLogs:   &proto.GetLogsParams{Kind: proto.LogService, Target: "nginx.service", Lines: 1},
			},
			wantErr: true,
		},
		{name: "rollback de un proyecto inválido", cmd: proto.Command{Operation: proto.OpRollback, Rollback: &proto.RollbackParams{Project: ""}}, wantErr: true},
		{name: "primer deploy", cmd: proto.Command{Operation: proto.OpBootstrap, Bootstrap: &proto.BootstrapParams{Project: "web"}}},
		{name: "primer deploy sin parámetros", cmd: proto.Command{Operation: proto.OpBootstrap}, wantErr: true},
		{name: "primer deploy de un proyecto inválido", cmd: proto.Command{Operation: proto.OpBootstrap, Bootstrap: &proto.BootstrapParams{Project: "../web"}}, wantErr: true},
		{
			name:    "primer deploy con los parámetros de deploy",
			cmd:     proto.Command{Operation: proto.OpBootstrap, Deploy: &proto.DeployParams{Project: "web"}},
			wantErr: true,
		},
		{
			name: "primer deploy con parámetros de más",
			cmd: proto.Command{
				Operation: proto.OpBootstrap,
				Bootstrap: &proto.BootstrapParams{Project: "web"},
				Deploy:    &proto.DeployParams{Project: "web"},
			},
			wantErr: true,
		},
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

func TestDeployEventValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(e *proto.DeployEvent)
		wantErr bool
	}{
		{name: "terminó bien", mutate: func(*proto.DeployEvent) {}},
		{
			name: "en curso",
			mutate: func(e *proto.DeployEvent) {
				e.Status, e.FinishedAt = proto.DeployRunning, time.Time{}
				e.Steps[len(e.Steps)-1].Status, e.Steps[len(e.Steps)-1].FinishedAt = proto.StepRunning, time.Time{}
			},
		},
		{
			name: "volvió atrás",
			mutate: func(e *proto.DeployEvent) {
				e.Status, e.Code, e.Message = proto.DeployRolledBack, proto.DeployUnhealthy, "el healthcheck devolvió 502 durante 120 s"
				e.Steps = append(e.Steps, proto.DeployStep{Name: proto.StepRollback, Status: proto.StepOK})
			},
		},
		{
			name: "falló antes de empezar, sin commit",
			mutate: func(e *proto.DeployEvent) {
				e.Status, e.Code, e.Commit, e.PreviousCommit = proto.DeployFailed, proto.DeployDirtyWorktree, "", ""
				e.Steps = e.Steps[:1]
			},
		},
		{name: "rollback manual", mutate: func(e *proto.DeployEvent) { e.Kind = proto.DeployKindRollback }},
		// Un primer deploy (ADR-021) no tiene de dónde volver.
		{name: "primer deploy", mutate: func(e *proto.DeployEvent) { e.Kind, e.PreviousCommit = proto.DeployKindBootstrap, "" }},
		{name: "primer deploy con commit anterior", mutate: func(e *proto.DeployEvent) { e.Kind = proto.DeployKindBootstrap }, wantErr: true},
		{
			name: "primer deploy de un proyecto que ya corre",
			mutate: func(e *proto.DeployEvent) {
				e.Kind, e.PreviousCommit, e.Commit = proto.DeployKindBootstrap, "", ""
				e.Status, e.Code = proto.DeployFailed, proto.DeployAlreadyDeployed
				e.Steps = e.Steps[:1]
			},
		},
		{name: "sin ID", mutate: func(e *proto.DeployEvent) { e.DeployID = uuid.Nil }, wantErr: true},
		{name: "secuencia cero", mutate: func(e *proto.DeployEvent) { e.Seq = 0 }, wantErr: true},
		{name: "proyecto inválido", mutate: func(e *proto.DeployEvent) { e.Project = "a b" }, wantErr: true},
		{name: "tipo inventado", mutate: func(e *proto.DeployEvent) { e.Kind = "redeploy" }, wantErr: true},
		{name: "estado inventado", mutate: func(e *proto.DeployEvent) { e.Status = "casi" }, wantErr: true},
		{name: "commit abreviado", mutate: func(e *proto.DeployEvent) { e.Commit = "89abcdef" }, wantErr: true},
		{name: "sin inicio", mutate: func(e *proto.DeployEvent) { e.StartedAt = time.Time{} }, wantErr: true},
		{name: "terminado sin fin", mutate: func(e *proto.DeployEvent) { e.FinishedAt = time.Time{} }, wantErr: true},
		{name: "en curso con fin", mutate: func(e *proto.DeployEvent) { e.Status = proto.DeployRunning }, wantErr: true},
		{name: "bien con código", mutate: func(e *proto.DeployEvent) { e.Code = proto.DeployUnhealthy }, wantErr: true},
		{name: "falla sin código", mutate: func(e *proto.DeployEvent) { e.Status = proto.DeployFailed }, wantErr: true},
		{
			name:    "código inventado",
			mutate:  func(e *proto.DeployEvent) { e.Status, e.Code = proto.DeployFailed, "se_rompio" },
			wantErr: true,
		},
		{name: "termina antes de empezar", mutate: func(e *proto.DeployEvent) { e.FinishedAt = e.StartedAt.Add(-time.Second) }, wantErr: true},
		{name: "mensaje con escape", mutate: func(e *proto.DeployEvent) { e.Message = "\x1b[2Jlimpio" }, wantErr: true},
		{
			name: "demasiados pasos",
			mutate: func(e *proto.DeployEvent) {
				for len(e.Steps) <= proto.MaxDeploySteps {
					e.Steps = append(e.Steps, proto.DeployStep{Name: proto.StepBuild, Status: proto.StepOK})
				}
			},
			wantErr: true,
		},
		{name: "paso desconocido", mutate: func(e *proto.DeployEvent) { e.Steps[0].Name = "shell" }, wantErr: true},
		// Los pasos declarados de un proyecto systemd no corren en un servicio de
		// Compose (ADR-019).
		{name: "paso pre sin servicio", mutate: func(e *proto.DeployEvent) { e.Steps[5].Service = "" }},
		{name: "reinicio en lugar de up", mutate: func(e *proto.DeployEvent) { e.Steps[6].Name = proto.StepRestart }},
		{name: "servicio en el reinicio", mutate: func(e *proto.DeployEvent) {
			e.Steps[6] = proto.DeployStep{Name: proto.StepRestart, Service: "backend", Status: proto.StepOK}
		}, wantErr: true},
		{
			name: "más de un build",
			mutate: func(e *proto.DeployEvent) {
				e.Steps = append(e.Steps, proto.DeployStep{Name: proto.StepBuild, Status: proto.StepOK, Detail: ".venv/bin/pip install -r requirements.txt"})
			},
		},
		{
			name: "no pudo reiniciar",
			mutate: func(e *proto.DeployEvent) {
				e.Status, e.Code = proto.DeployRolledBack, proto.DeployRestartFailed
			},
		},
		{name: "servicio en un paso fijo", mutate: func(e *proto.DeployEvent) { e.Steps[4].Service = "backend" }, wantErr: true},
		{name: "servicio con espacio", mutate: func(e *proto.DeployEvent) { e.Steps[5].Service = "back end" }, wantErr: true},
		{name: "estado de paso inventado", mutate: func(e *proto.DeployEvent) { e.Steps[0].Status = "maso" }, wantErr: true},
		{name: "paso en curso con fin", mutate: func(e *proto.DeployEvent) { e.Steps[0].Status = proto.StepRunning }, wantErr: true},
		{
			name: "renglón de salida demasiado largo",
			mutate: func(e *proto.DeployEvent) {
				e.Steps[4].Output = []string{strings.Repeat("x", proto.MaxStepOutputLineLen+1)}
			},
			wantErr: true,
		},
		{
			name:    "renglón de salida con salto",
			mutate:  func(e *proto.DeployEvent) { e.Steps[4].Output = []string{"uno\ndos"} },
			wantErr: true,
		},
		{
			name: "demasiados renglones",
			mutate: func(e *proto.DeployEvent) {
				e.Steps[4].Output = make([]string, proto.MaxStepOutputLines+1)
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := validDeployEvent()
			e.Steps = append([]proto.DeployStep(nil), e.Steps...)
			tt.mutate(&e)
			err := e.Validate()
			if tt.wantErr != (err != nil) {
				t.Fatalf("error = %v, se esperaba error: %v", err, tt.wantErr)
			}
		})
	}
}

// Con todos los topes al máximo y salida común (ASCII sin nada que escapar), la
// foto entra en un mensaje. Con caracteres que JSON escapa ("<" ocupa seis
// bytes) podría no entrar: ahí el agent recorta la salida más vieja antes de
// mandarla.
func TestLargestDeployEventFitsInAMessage(t *testing.T) {
	e := validDeployEvent()
	e.Status, e.Code = proto.DeployRolledBack, proto.DeployRollbackFailed
	e.Message = strings.Repeat("m", proto.MaxDeployMessageLen)
	line := strings.Repeat("x", proto.MaxStepOutputLineLen)
	e.Steps = nil
	for range proto.MaxDeploySteps {
		output := make([]string, proto.MaxStepOutputLines)
		for i := range output {
			output[i] = line
		}
		e.Steps = append(e.Steps, proto.DeployStep{
			Name: proto.StepPre, Service: strings.Repeat("s", proto.MaxStepServiceLen), Status: proto.StepFailed,
			StartedAt: e.StartedAt, FinishedAt: e.FinishedAt, Detail: strings.Repeat("d", proto.MaxDeployMessageLen), Output: output,
		})
	}
	if err := e.Validate(); err != nil {
		t.Fatalf("la foto más grande no es válida: %v", err)
	}
	raw, err := proto.Marshal(e)
	if err != nil {
		t.Fatalf("serializar: %v", err)
	}
	if len(raw) > proto.MaxMessageBytes {
		t.Fatalf("la foto más grande ocupa %d bytes y el tope es %d", len(raw), proto.MaxMessageBytes)
	}
	t.Logf("la foto más grande ocupa %d bytes de %d", len(raw), proto.MaxMessageBytes)
}

func TestDeployEventAckValidate(t *testing.T) {
	if err := (proto.DeployEventAck{DeployID: goldenID, Seq: 1}).Validate(); err != nil {
		t.Fatalf("un acuse válido dio error: %v", err)
	}
	if (proto.DeployEventAck{Seq: 1}).Validate() == nil {
		t.Fatal("un acuse sin ID no dio error")
	}
	if (proto.DeployEventAck{DeployID: goldenID}).Validate() == nil {
		t.Fatal("un acuse sin secuencia no dio error")
	}
}

func TestDeployStatusFinal(t *testing.T) {
	for status, final := range map[proto.DeployStatus]bool{
		proto.DeployRunning: false, proto.DeploySucceeded: true, proto.DeployFailed: true, proto.DeployRolledBack: true,
	} {
		if status.Final() != final {
			t.Errorf("%s.Final() = %v", status, !final)
		}
	}
}

// Lo que viaja en el cable se lee de vuelta igual.
func TestDeployEventRoundTrip(t *testing.T) {
	want := validDeployEvent()
	raw, err := proto.Marshal(want)
	if err != nil {
		t.Fatalf("serializar: %v", err)
	}
	envelope, err := proto.ParseEnvelope(raw)
	if err != nil {
		t.Fatalf("leer el envelope: %v", err)
	}
	got, err := proto.Decode[proto.DeployEvent](envelope)
	if err != nil {
		t.Fatalf("leer el mensaje: %v", err)
	}
	a, _ := json.Marshal(want)
	b, _ := json.Marshal(got)
	if string(a) != string(b) {
		t.Fatalf("no volvió igual:\n%s\n%s", a, b)
	}
}
