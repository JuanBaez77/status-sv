package ws

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/JuanBaez77/status-sv/agent/internal/projects"
	"github.com/JuanBaez77/status-sv/proto"
)

func (f *fakeControlPlane) handleProjects(ctx context.Context, conn *websocket.Conn, envelope proto.Envelope) error {
	state, err := proto.Decode[proto.ProjectsState](envelope)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.projectsStates = append(f.projectsStates, state)
	f.mu.Unlock()

	raw, err := proto.Marshal(proto.ProjectsStateAck{TakenAt: state.TakenAt})
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, raw)
}

func (f *fakeControlPlane) projectsCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.projectsStates)
}

func TestClientSendsProjectsAndReleasesThemOnAck(t *testing.T) {
	fake := newFakeControlPlane(t)
	client := newTestClient(t, fake)
	outbox := projects.NewOutbox()
	client.SetProjects(outbox)
	runClient(t, client)

	at := time.Now().UTC().Truncate(time.Millisecond)
	outbox.Put(proto.ProjectsState{TakenAt: at, Source: proto.RuntimeSource{Status: proto.SourceUnavailable}})
	eventually(t, "que lleguen los proyectos", func() bool { return fake.projectsCount() == 1 })

	outbox.Put(proto.ProjectsState{TakenAt: at.Add(time.Second), Source: proto.RuntimeSource{Status: proto.SourceOK}})
	eventually(t, "que llegue la segunda lectura", func() bool { return fake.projectsCount() == 2 })
	eventually(t, "que la bandeja quede vacía", func() bool {
		_, pending := outbox.Next(time.Now())
		return !pending
	})
}
