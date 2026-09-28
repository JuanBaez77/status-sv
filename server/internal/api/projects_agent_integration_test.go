//go:build integration

package api_test

import (
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/JuanBaez77/status-sv/proto"
)

// El agent manda los proyectos que leyó, se guardan y llega el acuse con la
// misma marca, también en un reenvío.
func TestProjectsStateIsStoredAndAcknowledged(t *testing.T) {
	f := setup(t)
	ack := f.enrollOK(t, "vps-proyectos", "203.0.113.70")
	conn, _, err := f.dial(t, ack.Credential)
	if err != nil {
		t.Fatalf("abrir el canal: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })

	at := time.Now().UTC().Truncate(time.Millisecond)
	state := proto.ProjectsState{
		TakenAt: at,
		Source:  proto.RuntimeSource{Status: proto.SourceOK},
		Projects: []proto.Project{{
			Name: "facturas", Environment: proto.EnvStaging, Repo: "github.com/x/facturas", Branch: "main",
			Runtime: proto.RuntimeSystemd, Workdir: "/srv/facturas", Units: []string{"gunicorn@facturas.service"},
		}},
		Invalid: []proto.InvalidProject{{File: "roto.yaml", Error: "name: falta"}},
	}
	for attempt := range 2 {
		sendEnvelope(t, conn, state)
		envelope := receive(t, conn)
		if envelope.Type != proto.TypeProjectsStateAck {
			t.Fatalf("envío %d: llegó %q, se esperaba projects_state_ack", attempt+1, envelope.Type)
		}
		got, err := proto.Decode[proto.ProjectsStateAck](envelope)
		if err != nil || !got.TakenAt.Equal(at) {
			t.Fatalf("acuse = %+v, %v; se mandó %v", got, err, at)
		}
	}

	var projects, invalid int
	if err := f.pool.QueryRow(t.Context(), `
		SELECT (SELECT count(*) FROM projects WHERE server_id = $1),
		       (SELECT count(*) FROM project_invalid_files WHERE server_id = $1)`, ack.ServerID).
		Scan(&projects, &invalid); err != nil {
		t.Fatalf("contar: %v", err)
	}
	if projects != 1 || invalid != 1 {
		t.Errorf("guardados %d proyectos y %d inválidos, se esperaba 1 y 1", projects, invalid)
	}
}
