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
)

// svdemoProposal es el alta de la demo de systemd como la manda el panel.
const svdemoProposal = `{
  "name": "svdemo", "environment": "staging", "repo": "github.com/JuanBaez77/status-sv-demo", "branch": "main",
  "runtime": "systemd", "command": [".venv/bin/gunicorn", "app:app", "--bind", "127.0.0.1:18097"],
  "required_env": ["SECRET_KEY"],
  "site": {"domain": "svdemo.extendsystem.com", "port": 18097},
  "healthcheck": {"url": "http://127.0.0.1:18097/health"},
  "build": [{"command": ["/usr/bin/python3", "-m", "venv", ".venv"]}]
}`

type previewBody struct {
	YAML    string `json:"yaml"`
	Unit    string `json:"unit"`
	Site    string `json:"site"`
	Command string `json:"command"`
}

type proposalBody struct {
	ID          uuid.UUID   `json:"id"`
	ServerID    uuid.UUID   `json:"server_id"`
	Name        string      `json:"name"`
	Runtime     string      `json:"runtime"`
	Status      string      `json:"status"`
	Message     string      `json:"message"`
	CreatedBy   string      `json:"created_by"`
	ActorType   string      `json:"actor_type"`
	DeliveredAt *time.Time  `json:"delivered_at"`
	Command     string      `json:"command"`
	Preview     previewBody `json:"preview"`
}

type proposalListBody struct {
	Proposals []proposalBody `json:"proposals"`
}

// nextProposal espera el próximo project_proposal que le llega al agent.
func (a *deployAgent) nextProposal(t *testing.T) proto.ProjectProposal {
	t.Helper()
	envelope := a.next(t)
	if envelope.Type != proto.TypeProjectProposal {
		t.Fatalf("llegó %q, se esperaba project_proposal", envelope.Type)
	}
	p, err := proto.Decode[proto.ProjectProposal](envelope)
	if err != nil {
		t.Fatalf("leer la propuesta: %v", err)
	}
	return p
}

// proposalStatus espera a que la propuesta quede en ese estado: el acuse del
// agent se procesa aparte del pedido HTTP.
func (f *fixture) proposalStatus(t *testing.T, cookie *http.Cookie, ip string, id uuid.UUID, want string) proposalBody {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := decodeJSON[proposalBody](t, f.do(t, http.MethodGet, "/v1/proposals/"+id.String(), "", ip, cookie, nil))
		if got.Status == want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("la propuesta quedó %q, se esperaba %q", got.Status, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestProposalFromThePanel(t *testing.T) {
	f := setup(t)
	const ip = "198.51.100.110"
	cookie := f.login(t, ip)
	conn, serverID := connectAgent(t, f, "sv-alta", "203.0.113.110")
	agent := newDeployAgent(t, conn, acceptAll)
	// Hasta que termine la comparación de propuestas: si se atrasa, ve abierta
	// la propuesta que se hace más abajo y se la vuelve a mandar.
	agent.declareWith(t, proto.ProjectsState{})
	base := "/v1/servers/" + serverID.String() + "/proposals"

	// La vista previa muestra lo que se va a escribir y no guarda nada.
	resp := f.do(t, http.MethodPost, base+"/preview", svdemoProposal, ip, cookie, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("vista previa = %d %s", resp.StatusCode, resp.body)
	}
	preview := decodeJSON[previewBody](t, resp)
	if !strings.Contains(preview.YAML, "workdir: /srv/svdemo\n") || !strings.Contains(preview.Unit, "User=svdemo") ||
		!strings.Contains(preview.Site, "server_name svdemo.extendsystem.com;") || preview.Command != "sudo statussv-agent project accept svdemo" {
		t.Fatalf("vista previa = %+v", preview)
	}
	if list := decodeJSON[proposalListBody](t, f.do(t, http.MethodGet, base, "", ip, cookie, nil)); len(list.Proposals) != 0 {
		t.Fatalf("la vista previa guardó algo: %+v", list)
	}

	// Una propuesta inválida dice qué campo, y queda anotado el rechazo.
	bad := strings.Replace(svdemoProposal, "svdemo.extendsystem.com", "*.extendsystem.com", 1)
	if resp := f.do(t, http.MethodPost, base, bad, ip, cookie, nil); resp.StatusCode != http.StatusBadRequest ||
		!strings.Contains(string(resp.body), "invalid_proposal") || !strings.Contains(string(resp.body), "site.domain") {
		t.Fatalf("inválida = %d %s", resp.StatusCode, resp.body)
	}
	// Los valores de los secretos no tienen dónde ir: un campo de más se
	// rechaza entero.
	withValues := strings.Replace(svdemoProposal, `"required_env"`, `"env": {"SECRET_KEY": "s3cr3t0"}, "required_env"`, 1)
	if resp := f.do(t, http.MethodPost, base, withValues, ip, cookie, nil); resp.StatusCode != http.StatusBadRequest ||
		strings.Contains(string(resp.body), "s3cr3t0") {
		t.Fatalf("con valores = %d %s", resp.StatusCode, resp.body)
	}
	// Sin sesión, nada.
	if resp := f.do(t, http.MethodPost, base, svdemoProposal, ip, nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("sin sesión = %d", resp.StatusCode)
	}

	resp = f.do(t, http.MethodPost, base, svdemoProposal, ip, cookie, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("propuesta = %d %s", resp.StatusCode, resp.body)
	}
	created := decodeJSON[proposalBody](t, resp)
	if created.Status != "pending" || created.CreatedBy != "juan" || created.ActorType != "user" || created.Preview.Unit != preview.Unit {
		t.Fatalf("creada = %+v", created)
	}

	// Al agent le llega la misma declaración que se mostró.
	sent := agent.nextProposal(t)
	if sent.ProposalID != created.ID || sent.Action != proto.ProposalPut || sent.Name != "svdemo" || sent.YAML != preview.YAML {
		t.Fatalf("al agent le llegó %+v", sent)
	}
	sendEnvelope(t, conn, proto.ProjectProposalAck{ProposalID: sent.ProposalID, Name: sent.Name, Result: proto.ProposalStored})
	delivered := f.proposalStatus(t, cookie, ip, created.ID, "delivered")
	if delivered.DeliveredAt == nil || delivered.Preview.YAML != preview.YAML {
		t.Errorf("entregada = %+v", delivered)
	}

	// El agent la informa guardada: no se la vuelve a mandar.
	stored := proto.ProjectsState{Proposals: []proto.PendingProposal{{ProposalID: sent.ProposalID, Name: "svdemo", Hash: proto.SpecHash([]byte(sent.YAML))}}}
	if resent := agent.declareWith(t, stored); len(resent) != 0 {
		t.Fatalf("al informarla guardada le llegó %+v", resent)
	}
	resp = f.do(t, http.MethodDelete, "/v1/proposals/"+created.ID.String(), "", ip, cookie, nil)
	if resp.StatusCode != http.StatusOK || decodeJSON[proposalBody](t, resp).Status != "withdrawn" {
		t.Fatalf("retirar = %d %s", resp.StatusCode, resp.body)
	}
	if withdraw := agent.nextProposal(t); withdraw.ProposalID != created.ID || withdraw.Action != proto.ProposalWithdraw {
		t.Fatalf("retiro = %+v", withdraw)
	}
	if resp := f.do(t, http.MethodDelete, "/v1/proposals/"+created.ID.String(), "", ip, cookie, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("retirar dos veces = %d", resp.StatusCode)
	}
	// Si el agent la sigue informando guardada (por ejemplo, porque el retiro
	// se cruzó con un projects_state viejo), se le vuelve a pedir que la borre.
	if withdraw := agent.declareWith(t, stored); len(withdraw) != 1 ||
		withdraw[0].ProposalID != created.ID || withdraw[0].Action != proto.ProposalWithdraw {
		t.Fatalf("la sigue teniendo y le llegó %+v", withdraw)
	}

	// Se propone de nuevo y una persona la acepta en el servidor: el agent
	// informa el proyecto declarado con el mismo hash.
	again := decodeJSON[proposalBody](t, f.do(t, http.MethodPost, base, svdemoProposal, ip, cookie, nil))
	sent = agent.nextProposal(t)
	if sent.ProposalID != again.ID {
		t.Fatalf("al agent le llegó %+v", sent)
	}
	project := proto.Project{
		Name: "svdemo", Environment: proto.EnvStaging, Repo: "github.com/JuanBaez77/status-sv-demo", Branch: "main",
		Runtime: proto.RuntimeSystemd, Workdir: "/srv/svdemo", Units: []string{"svdemo.service"},
		SpecHash: proto.SpecHash([]byte(sent.YAML)),
	}
	agent.declareWith(t, proto.ProjectsState{Projects: []proto.Project{project}})
	accepted := f.proposalStatus(t, cookie, ip, again.ID, "accepted")
	if accepted.Message != "declarada igual a la propuesta" {
		t.Errorf("aceptada = %+v", accepted)
	}

	// Ya declarado, no se puede proponer con ese nombre.
	if resp := f.do(t, http.MethodPost, base, svdemoProposal, ip, cookie, nil); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(string(resp.body), "already_declared") {
		t.Fatalf("propuesta de un declarado = %d %s", resp.StatusCode, resp.body)
	}

	list := decodeJSON[proposalListBody](t, f.do(t, http.MethodGet, base, "", ip, cookie, nil))
	if len(list.Proposals) != 2 || list.Proposals[0].ID != again.ID || list.Proposals[1].Status != "withdrawn" ||
		list.Proposals[0].Command != "sudo statussv-agent project accept svdemo" {
		t.Errorf("lista = %+v", list)
	}

	wantAudit(t, f.auditFrom(t, ip),
		"auth.login ok juan", "auth.totp_enroll ok juan",
		"project.propose rejected juan", "project.propose ok juan", "project.withdraw ok juan",
		"project.propose ok juan", "project.propose rejected juan")
	var actor, actorType string
	if err := f.pool.QueryRow(t.Context(), "SELECT actor, actor_type FROM audit_log WHERE action = 'project.accept'").Scan(&actor, &actorType); err != nil {
		t.Fatalf("leer la aceptación: %v", err)
	}
	if actor != "sv-alta" || actorType != "agent" {
		t.Errorf("aceptación anotada por %s (%s)", actor, actorType)
	}
}

// declareWith manda un projects_state con lo que traiga, espera su acuse y
// devuelve las propuestas que el control plane le mandó al compararlas.
//
// El acuse sale antes de esa comparación, así que con el acuse solo no alcanza:
// un retiro hecho enseguida puede cruzarse con ella. Por eso le sigue un latido:
// el canal atiende los mensajes del agent de a uno, y el acuse del latido llega
// recién cuando la comparación terminó.
func (a *deployAgent) declareWith(t *testing.T, state proto.ProjectsState) []proto.ProjectProposal {
	t.Helper()
	state.TakenAt = time.Now().UTC()
	state.Source = proto.RuntimeSource{Status: proto.SourceOK}
	sendEnvelope(t, a.conn, state)
	if envelope := a.next(t); envelope.Type != proto.TypeProjectsStateAck {
		t.Fatalf("llegó %q, se esperaba projects_state_ack", envelope.Type)
	}
	sendEnvelope(t, a.conn, proto.Heartbeat{AgentVersion: "test"})
	var sent []proto.ProjectProposal
	for {
		envelope := a.next(t)
		switch envelope.Type {
		case proto.TypeHeartbeatAck:
			return sent
		case proto.TypeProjectProposal:
			p, err := proto.Decode[proto.ProjectProposal](envelope)
			if err != nil {
				t.Fatalf("leer la propuesta: %v", err)
			}
			sent = append(sent, p)
		default:
			t.Fatalf("llegó %q, se esperaba project_proposal o heartbeat_ack", envelope.Type)
		}
	}
}

// Una propuesta hecha con el agent desconectado le llega cuando se conecta e
// informa sus proyectos.
func TestProposalReachesAgentOnConnect(t *testing.T) {
	f := setup(t)
	const ip = "198.51.100.111"
	cookie := f.login(t, ip)
	ack := f.enrollOK(t, "sv-tarde", "203.0.113.111")

	resp := f.do(t, http.MethodPost, "/v1/servers/"+ack.ServerID.String()+"/proposals", svdemoProposal, ip, cookie, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("propuesta = %d %s", resp.StatusCode, resp.body)
	}
	created := decodeJSON[proposalBody](t, resp)

	conn, _, err := f.dial(t, ack.Credential)
	if err != nil {
		t.Fatalf("abrir el canal: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	agent := newDeployAgent(t, conn, acceptAll)
	agent.declare(t)
	if sent := agent.nextProposal(t); sent.ProposalID != created.ID || sent.Action != proto.ProposalPut {
		t.Fatalf("al conectarse le llegó %+v", sent)
	}

	// El agent no la pudo guardar: queda rechazada, con el motivo.
	sendEnvelope(t, conn, proto.ProjectProposalAck{ProposalID: created.ID, Name: "svdemo", Result: proto.ProposalRejected, Error: "ya hay 10 propuestas guardadas"})
	if got := f.proposalStatus(t, cookie, ip, created.ID, "rejected"); got.Message != "ya hay 10 propuestas guardadas" {
		t.Errorf("rechazada = %+v", got)
	}
}

// Un agent no puede cerrar ni dar por entregada la propuesta de otro
// servidor.
func TestProposalAckFromAnotherServer(t *testing.T) {
	f := setup(t)
	const ip = "198.51.100.112"
	cookie := f.login(t, ip)
	_, serverID := connectAgent(t, f, "sv-uno", "203.0.113.112")
	otherConn, _ := connectAgent(t, f, "sv-dos", "203.0.113.113")
	other := newDeployAgent(t, otherConn, acceptAll)
	other.declare(t)

	created := decodeJSON[proposalBody](t, f.do(t, http.MethodPost, "/v1/servers/"+serverID.String()+"/proposals", svdemoProposal, ip, cookie, nil))
	sendEnvelope(t, otherConn, proto.ProjectProposalAck{ProposalID: created.ID, Name: "svdemo", Result: proto.ProposalRejected, Error: "no"})
	sendEnvelope(t, otherConn, proto.ProjectProposalAck{ProposalID: created.ID, Name: "svdemo", Result: proto.ProposalStored})
	// Ni el projects_state del otro con el proyecto declarado la acepta.
	other.declareWith(t, proto.ProjectsState{Projects: []proto.Project{dockerProject("svdemo", proto.EnvStaging)}})

	got := decodeJSON[proposalBody](t, f.do(t, http.MethodGet, "/v1/proposals/"+created.ID.String(), "", ip, cookie, nil))
	if got.Status != "pending" {
		t.Fatalf("la propuesta quedó %+v", got)
	}
}

// El panel que mira el servidor se entera cuando cambian sus propuestas.
func TestProposalNotifiesThePanel(t *testing.T) {
	f := setup(t)
	const ip = "198.51.100.114"
	cookie := f.login(t, ip)
	_, serverID := connectAgent(t, f, "sv-panel", "203.0.113.114")

	panelConn, status, err := f.dialPanel(t, cookie)
	if err != nil {
		t.Fatalf("abrir el canal del panel: %v (estado %d)", err, status)
	}
	readPanel(t, panelConn) // bienvenida
	writePanel(t, panelConn, `{"type":"subscribe","server_ids":["`+serverID.String()+`"]}`)
	if ack := readPanel(t, panelConn); ack.Type != "subscribed" {
		t.Fatalf("confirmación = %+v", ack)
	}

	if resp := f.do(t, http.MethodPost, "/v1/servers/"+serverID.String()+"/proposals", svdemoProposal, ip, cookie, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("propuesta = %d %s", resp.StatusCode, resp.body)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		_, raw, err := panelConn.Read(ctx)
		if err != nil {
			t.Fatalf("no llegó el aviso de propuestas: %v", err)
		}
		var message panelMessage
		if err := json.Unmarshal(raw, &message); err != nil {
			t.Fatalf("leer %s: %v", raw, err)
		}
		if message.Type == "proposals" {
			if message.ServerID != serverID {
				t.Errorf("aviso de otro servidor: %+v", message)
			}
			return
		}
	}
}

func TestProposalNotFound(t *testing.T) {
	f := setup(t)
	const ip = "198.51.100.115"
	cookie := f.login(t, ip)
	missing := uuid.Must(uuid.NewV7()).String()
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/v1/servers/" + missing + "/proposals"},
		{http.MethodPost, "/v1/servers/" + missing + "/proposals"},
		{http.MethodGet, "/v1/proposals/" + missing},
		{http.MethodDelete, "/v1/proposals/" + missing},
	} {
		if resp := f.do(t, c.method, c.path, svdemoProposal, ip, cookie, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s = %d %s", c.method, c.path, resp.StatusCode, resp.body)
		}
	}
	if resp := f.do(t, http.MethodGet, "/v1/proposals/no-es-un-uuid", "", ip, cookie, nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("ID inválido = %d", resp.StatusCode)
	}
}
