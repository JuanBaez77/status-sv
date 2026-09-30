package ws

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/agent/internal/deploy"
	"github.com/JuanBaez77/status-sv/proto"
)

func TestClientSendsDeployEventsUntilAcked(t *testing.T) {
	fake := newFakeControlPlane(t)
	client := newTestClient(t, fake)
	box := deploy.NewOutbox()
	client.SetDeploys(box)

	id := uuid.Must(uuid.NewV7())
	start := time.Now().UTC().Truncate(time.Millisecond)
	box.Put(proto.DeployEvent{DeployID: id, Seq: 1, Project: "tienda", Kind: proto.DeployKindDeploy, Status: proto.DeployRunning, StartedAt: start})
	runClient(t, client)

	eventually(t, "que llegue la foto del deploy", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.deployEvents) == 1
	})
	eventually(t, "que el acuse suelte la foto", func() bool { return box.Len() == 0 })

	// La final sale apenas aparece, en el mismo canal.
	box.Put(proto.DeployEvent{
		DeployID: id, Seq: 2, Project: "tienda", Kind: proto.DeployKindDeploy, Status: proto.DeploySucceeded,
		StartedAt: start, FinishedAt: start.Add(time.Minute),
	})
	eventually(t, "que llegue la foto final", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.deployEvents) == 2 && fake.deployEvents[1].Status == proto.DeploySucceeded
	})
	eventually(t, "que se suelte la final", func() bool { return box.Len() == 0 })
}
