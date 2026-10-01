//go:build integration

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
	"github.com/JuanBaez77/status-sv/server/internal/auth"
)

const (
	commitA = "1111111111111111111111111111111111111111"
	commitB = "2222222222222222222222222222222222222222"
)

type deployBody struct {
	ID          uuid.UUID          `json:"id"`
	Kind        string             `json:"kind"`
	Commit      string             `json:"commit"`
	Status      string             `json:"status"`
	Code        string             `json:"code"`
	Message     string             `json:"message"`
	RequestedBy string             `json:"requested_by"`
	ProjectName string             `json:"project_name"`
	ServerName  string             `json:"server_name"`
	Steps       []proto.DeployStep `json:"steps"`
}

// deployAgent hace de agent: contesta los comandos con reply y deja el resto
// de lo que le llega (los acuses) en received.
type deployAgent struct {
	conn     *websocket.Conn
	commands chan proto.Envelope
	received chan proto.Envelope
}

func newDeployAgent(t *testing.T, conn *websocket.Conn, reply func(proto.Envelope, proto.Command) proto.CommandAck) *deployAgent {
	t.Helper()
	a := &deployAgent{conn: conn, commands: make(chan proto.Envelope, 8), received: make(chan proto.Envelope, 32)}
	go func() {
		for {
			_, raw, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			envelope, err := proto.ParseEnvelope(raw)
			if err != nil {
				continue
			}
			if envelope.Type != proto.TypeCommand {
				a.received <- envelope
				continue
			}
			cmd, err := proto.Decode[proto.Command](envelope)
			if err != nil {
				continue
			}
			a.commands <- envelope
			ack, err := proto.Marshal(reply(envelope, cmd))
			if err != nil {
				return
			}
			_ = conn.Write(context.Background(), websocket.MessageText, ack)
		}
	}()
	return a
}

// next espera el próximo mensaje que no es un comando.
func (a *deployAgent) next(t *testing.T) proto.Envelope {
	t.Helper()
	select {
	case envelope := <-a.received:
		return envelope
	case <-time.After(5 * time.Second):
		t.Fatal("no llegó nada del control plane")
		return proto.Envelope{}
	}
}

// declare manda los proyectos del servidor y espera su acuse.
func (a *deployAgent) declare(t *testing.T, projects ...proto.Project) {
	t.Helper()
	sendEnvelope(t, a.conn, proto.ProjectsState{TakenAt: time.Now().UTC(), Source: proto.RuntimeSource{Status: proto.SourceOK}, Projects: projects})
	if envelope := a.next(t); envelope.Type != proto.TypeProjectsStateAck {
		t.Fatalf("llegó %q, se esperaba projects_state_ack", envelope.Type)
	}
}

// event manda una foto de deploy y devuelve lo que contestó el control plane.
func (a *deployAgent) event(t *testing.T, ev proto.DeployEvent) proto.Envelope {
	t.Helper()
	sendEnvelope(t, a.conn, ev)
	return a.next(t)
}

func acceptAll(envelope proto.Envelope, _ proto.Command) proto.CommandAck {
	return proto.CommandAck{CommandID: envelope.ID, Status: proto.CommandOK, Message: "deploy aceptado"}
}

func dockerProject(name string, env proto.Environment) proto.Project {
	return proto.Project{
		Name: name, Environment: env, Repo: "github.com/a/" + name, Branch: "main", Runtime: proto.RuntimeDocker,
		Workdir: "/srv/" + name, ComposeFile: "compose.yml", ComposeProject: name,
	}
}

// projectID busca el ID de un proyecto en la lista del panel.
func (f *fixture) projectID(t *testing.T, cookie *http.Cookie, ip, name string) string {
	t.Helper()
	list := decodeJSON[projectListBody](t, f.do(t, http.MethodGet, "/v1/projects", "", ip, cookie, nil))
	for _, p := range list.Projects {
		if p.Name == name {
			return p.ID.String()
		}
	}
	t.Fatalf("no está el proyecto %s: %+v", name, list.Projects)
	return ""
}

func (f *fixture) postDeploy(t *testing.T, path, body, ip string, cookie *http.Cookie) (int, deployBody, string) {
	t.Helper()
	resp := f.do(t, http.MethodPost, path, body, ip, cookie, nil)
	var out deployBody
	if resp.StatusCode == http.StatusCreated {
		if err := json.Unmarshal(resp.body, &out); err != nil {
			t.Fatalf("leer la respuesta: %v", err)
		}
	}
	return resp.StatusCode, out, string(resp.body)
}

func runningEvent(id uuid.UUID, seq int, start time.Time, steps ...proto.DeployStep) proto.DeployEvent {
	return proto.DeployEvent{
		DeployID: id, Seq: seq, Project: "tienda", Kind: proto.DeployKindDeploy, Status: proto.DeployRunning,
		StartedAt: start, Commit: commitB, PreviousCommit: commitA, Steps: steps,
	}
}

func TestDeployFromThePanel(t *testing.T) {
	f := setup(t)
	const ip = "198.51.100.90"
	cookie := f.login(t, ip)
	conn, _ := connectAgent(t, f, "vps-deploy", "203.0.113.90")
	agent := newDeployAgent(t, conn, acceptAll)
	agent.declare(t, dockerProject("tienda", proto.EnvStaging))
	project := f.projectID(t, cookie, ip, "tienda")
	path := "/v1/projects/" + project + "/deploys"

	// Sin escribir el nombre del proyecto no se despliega.
	if status, _, body := f.postDeploy(t, path, `{"confirm":"otro"}`, ip, cookie); status != http.StatusBadRequest || !strings.Contains(body, "confirmation_required") {
		t.Fatalf("sin confirmación = %d %s", status, body)
	}
	// Un commit que no es un SHA completo, tampoco.
	if status, _, _ := f.postDeploy(t, path, `{"confirm":"tienda","commit":"main"}`, ip, cookie); status != http.StatusBadRequest {
		t.Fatalf("commit inválido = %d", status)
	}

	status, created, body := f.postDeploy(t, path, `{"confirm":"tienda"}`, ip, cookie)
	if status != http.StatusCreated || created.Status != "pending" || created.RequestedBy != "juan" || created.Kind != "deploy" {
		t.Fatalf("deploy = %d %s", status, body)
	}
	// El agent recibió el deploy con el mismo ID.
	envelope := <-agent.commands
	cmd, _ := proto.Decode[proto.Command](envelope)
	if envelope.ID != created.ID || cmd.Operation != proto.OpDeploy || cmd.Deploy.Project != "tienda" || cmd.Deploy.Commit != "" {
		t.Fatalf("comando = %s %+v", envelope.ID, cmd)
	}

	// Mientras está activo, otro pedido se rechaza.
	if status, _, body := f.postDeploy(t, path, `{"confirm":"tienda"}`, ip, cookie); status != http.StatusConflict || !strings.Contains(body, "deploy_in_progress") {
		t.Fatalf("segundo deploy = %d %s", status, body)
	}

	start := time.Now().UTC().Truncate(time.Millisecond)
	step := proto.DeployStep{Name: proto.StepBuild, Status: proto.StepRunning, StartedAt: start, Output: []string{"#1 building"}}
	if ack := agent.event(t, runningEvent(created.ID, 2, start, step)); ack.Type != proto.TypeDeployEventAck {
		t.Fatalf("llegó %q", ack.Type)
	}
	running := decodeJSON[deployBody](t, f.do(t, http.MethodGet, "/v1/deploys/"+created.ID.String(), "", ip, cookie, nil))
	if running.Status != "running" || running.Commit != commitB || len(running.Steps) != 1 || running.Steps[0].Output[0] != "#1 building" {
		t.Fatalf("en curso = %+v", running)
	}

	final := runningEvent(created.ID, 5, start)
	final.Status, final.FinishedAt = proto.DeploySucceeded, start.Add(time.Minute)
	if ack := agent.event(t, final); ack.Type != proto.TypeDeployEventAck {
		t.Fatalf("llegó %q", ack.Type)
	}
	// Una foto vieja que llega tarde se confirma pero no pisa nada.
	if ack := agent.event(t, runningEvent(created.ID, 3, start)); ack.Type != proto.TypeDeployEventAck {
		t.Fatalf("la foto vieja no se confirmó: %q", ack.Type)
	}
	// Una más nueva sobre un deploy que el agent ya cerró, también: se
	// confirma, pero no lo reabre ni le cambia el final.
	reopened := runningEvent(created.ID, 9, start)
	reopened.Status, reopened.Code, reopened.FinishedAt = proto.DeployRolledBack, proto.DeployInterrupted, start.Add(2*time.Minute)
	reopened.Message = "el agent se reinició en medio del deploy"
	if ack := agent.event(t, reopened); ack.Type != proto.TypeDeployEventAck {
		t.Fatalf("la foto sobre un deploy cerrado no se confirmó: %q", ack.Type)
	}
	done := decodeJSON[deployBody](t, f.do(t, http.MethodGet, "/v1/deploys/"+created.ID.String(), "", ip, cookie, nil))
	if done.Status != "succeeded" {
		t.Fatalf("terminado = %+v", done)
	}

	history := decodeJSON[struct {
		Deploys []deployBody `json:"deploys"`
	}](t, f.do(t, http.MethodGet, path, "", ip, cookie, nil))
	if len(history.Deploys) != 1 || history.Deploys[0].ID != created.ID || history.Deploys[0].Status != "succeeded" {
		t.Fatalf("historia = %+v", history)
	}

	// Todo quedó en la auditoría: el pedido de la persona y el final del agent.
	// Un solo final: la foto sobre el deploy cerrado no sumó otro.
	var requested, finished, finals int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT count(*) FILTER (WHERE action = 'deploy.request' AND result = 'accepted' AND actor = 'juan' AND actor_type = 'user'),
		        count(*) FILTER (WHERE action = 'deploy.finish' AND result = 'succeeded' AND actor = 'vps-deploy' AND actor_type = 'agent'),
		        count(*) FILTER (WHERE action = 'deploy.finish')
		 FROM audit_log WHERE target_id = $1`, project).Scan(&requested, &finished, &finals); err != nil {
		t.Fatal(err)
	}
	if requested != 1 || finished != 1 || finals != 1 {
		t.Errorf("auditoría: %d pedidos, %d finales buenos y %d finales en total", requested, finished, finals)
	}

	// Terminado, se puede pedir otro.
	if status, _, body := f.postDeploy(t, path, `{"confirm":"tienda","commit":"`+commitA+`"}`, ip, cookie); status != http.StatusCreated {
		t.Fatalf("otro deploy = %d %s", status, body)
	}
}

func TestProductionDeployNeedsAFreshCode(t *testing.T) {
	f := setup(t)
	const ip = "198.51.100.91"
	f.createUser(t)
	cookie, secret := f.loginWithTOTP(t, ip)
	conn, _ := connectAgent(t, f, "vps-prod", "203.0.113.91")
	agent := newDeployAgent(t, conn, acceptAll)
	agent.declare(t, dockerProject("tienda", proto.EnvProduction))
	path := "/v1/projects/" + f.projectID(t, cookie, ip, "tienda") + "/deploys"

	if status, _, body := f.postDeploy(t, path, `{"confirm":"tienda"}`, ip, cookie); status != http.StatusForbidden || !strings.Contains(body, "invalid_code") {
		t.Fatalf("sin código = %d %s", status, body)
	}
	// El código del login ya está usado.
	used := auth.TOTPCode(secret, auth.TOTPStep(time.Now()))
	if status, _, _ := f.postDeploy(t, path, `{"confirm":"tienda","totp_code":"`+used+`"}`, ip, cookie); status != http.StatusForbidden {
		t.Fatalf("con el código del login = %d", status)
	}
	fresh := auth.TOTPCode(secret, auth.TOTPStep(time.Now())+1)
	status, created, body := f.postDeploy(t, path, `{"confirm":"tienda","totp_code":"`+fresh+`"}`, ip, cookie)
	if status != http.StatusCreated || created.Status != "pending" {
		t.Fatalf("con código nuevo = %d %s", status, body)
	}
	<-agent.commands
}

func TestDeployRejectedByTheAgent(t *testing.T) {
	f := setup(t)
	const ip = "198.51.100.92"
	cookie := f.login(t, ip)
	conn, _ := connectAgent(t, f, "vps-apagado", "203.0.113.92")
	agent := newDeployAgent(t, conn, func(envelope proto.Envelope, _ proto.Command) proto.CommandAck {
		return proto.CommandAck{CommandID: envelope.ID, Status: proto.CommandRejected, Code: proto.CommandDisabled, Message: "el deploy está apagado"}
	})
	agent.declare(t, dockerProject("tienda", proto.EnvStaging))
	path := "/v1/projects/" + f.projectID(t, cookie, ip, "tienda") + "/deploys"

	status, created, body := f.postDeploy(t, path, `{"confirm":"tienda"}`, ip, cookie)
	if status != http.StatusCreated || created.Status != "failed" || created.Code != "disabled" {
		t.Fatalf("rechazado = %d %s", status, body)
	}
	// El proyecto queda libre.
	if status, _, _ := f.postDeploy(t, path, `{"confirm":"tienda"}`, ip, cookie); status != http.StatusCreated {
		t.Fatalf("después del rechazo = %d", status)
	}
}

func TestRollbackFromThePanel(t *testing.T) {
	f := setup(t)
	const ip = "198.51.100.93"
	cookie := f.login(t, ip)
	conn, _ := connectAgent(t, f, "vps-rollback", "203.0.113.93")
	agent := newDeployAgent(t, conn, acceptAll)
	agent.declare(t, dockerProject("tienda", proto.EnvStaging))
	path := "/v1/projects/" + f.projectID(t, cookie, ip, "tienda") + "/rollback"

	if status, _, _ := f.postDeploy(t, path, `{"confirm":"tienda","commit":"`+commitA+`"}`, ip, cookie); status != http.StatusBadRequest {
		t.Fatalf("rollback con commit = %d", status)
	}
	status, created, body := f.postDeploy(t, path, `{"confirm":"tienda"}`, ip, cookie)
	if status != http.StatusCreated || created.Kind != "rollback" {
		t.Fatalf("rollback = %d %s", status, body)
	}
	envelope := <-agent.commands
	cmd, _ := proto.Decode[proto.Command](envelope)
	if cmd.Operation != proto.OpRollback || cmd.Rollback.Project != "tienda" {
		t.Fatalf("comando = %+v", cmd)
	}
}

// Un agent no puede mandar fotos de un deploy de otro servidor.
func TestDeployEventFromAnotherServer(t *testing.T) {
	f := setup(t)
	const ip = "198.51.100.94"
	cookie := f.login(t, ip)
	conn, _ := connectAgent(t, f, "vps-dueno", "203.0.113.94")
	owner := newDeployAgent(t, conn, acceptAll)
	owner.declare(t, dockerProject("tienda", proto.EnvStaging))
	intruderConn, _ := connectAgent(t, f, "vps-intruso", "203.0.113.95")
	intruder := newDeployAgent(t, intruderConn, acceptAll)

	_, created, body := f.postDeploy(t, "/v1/projects/"+f.projectID(t, cookie, ip, "tienda")+"/deploys", `{"confirm":"tienda"}`, ip, cookie)
	if created.ID == uuid.Nil {
		t.Fatalf("deploy = %s", body)
	}
	final := runningEvent(created.ID, 9, time.Now().UTC())
	final.Status, final.Code, final.FinishedAt = proto.DeployFailed, proto.DeployRollbackFailed, time.Now().UTC().Add(time.Second)
	reply := intruder.event(t, final)
	if reply.Type != proto.TypeError || errorCode(t, reply) != proto.CodeInvalidMessage {
		t.Fatalf("respuesta al intruso = %q", reply.Type)
	}
	d := decodeJSON[deployBody](t, f.do(t, http.MethodGet, "/v1/deploys/"+created.ID.String(), "", ip, cookie, nil))
	if d.Status != "pending" {
		t.Fatalf("el intruso cambió el deploy: %+v", d)
	}
}

func TestDeployRequestErrors(t *testing.T) {
	f := setup(t)
	const ip = "198.51.100.96"
	cookie := f.login(t, ip)
	conn, _ := connectAgent(t, f, "vps-errores", "203.0.113.96")
	agent := newDeployAgent(t, conn, acceptAll)
	agent.declare(t, dockerProject("tienda", proto.EnvStaging), proto.Project{
		Name: "facturas", Environment: proto.EnvStaging, Repo: "github.com/a/facturas", Branch: "main",
		Runtime: proto.RuntimeSystemd, Workdir: "/srv/facturas", Units: []string{"facturas.service"},
	})
	tienda := "/v1/projects/" + f.projectID(t, cookie, ip, "tienda") + "/deploys"
	facturas := "/v1/projects/" + f.projectID(t, cookie, ip, "facturas") + "/deploys"

	tests := []struct {
		name   string
		path   string
		body   string
		cookie *http.Cookie
		want   int
	}{
		{"sin sesión", tienda, `{"confirm":"tienda"}`, nil, http.StatusUnauthorized},
		{"campo de más", tienda, `{"confirm":"tienda","steps":["rm -rf /"]}`, cookie, http.StatusBadRequest},
		{"proyecto que no existe", "/v1/projects/" + uuid.Must(uuid.NewV7()).String() + "/deploys", `{"confirm":"x"}`, cookie, http.StatusNotFound},
		{"systemd todavía no", facturas, `{"confirm":"facturas"}`, cookie, http.StatusConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if status, _, body := f.postDeploy(t, tt.path, tt.body, ip, tt.cookie); status != tt.want {
				t.Errorf("estado %d (%s), se esperaba %d", status, body, tt.want)
			}
		})
	}

	// Con el agent desconectado no se anota nada.
	_ = conn.Close(websocket.StatusNormalClosure, "")
	time.Sleep(200 * time.Millisecond)
	if status, _, body := f.postDeploy(t, tienda, `{"confirm":"tienda"}`, ip, cookie); status != http.StatusConflict || !strings.Contains(body, "agent_offline") {
		t.Errorf("agent desconectado = %d %s", status, body)
	}
}
