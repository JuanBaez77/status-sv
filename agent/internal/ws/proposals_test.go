package ws

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
)

// fakeProposals guarda lo que recibe y contesta que lo guardó.
type fakeProposals struct{ seen chan proto.ProjectProposal }

func (f *fakeProposals) Handle(p proto.ProjectProposal) proto.ProjectProposalAck {
	f.seen <- p
	return proto.ProjectProposalAck{ProposalID: p.ProposalID, Name: p.Name, Result: proto.ProposalStored}
}

// sendProposal manda una propuesta por el canal abierto y espera el acuse.
func sendProposal(t *testing.T, fake *fakeControlPlane, p proto.ProjectProposal) proto.ProjectProposalAck {
	t.Helper()
	eventually(t, "que el agent se conecte", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return len(fake.conns) > 0
	})
	fake.mu.Lock()
	conn := fake.conns[len(fake.conns)-1]
	fake.proposalAcks = make(chan proto.ProjectProposalAck, 1)
	acks := fake.proposalAcks
	fake.mu.Unlock()

	raw, err := proto.Marshal(p)
	if err != nil {
		t.Fatalf("armar la propuesta: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("mandar la propuesta: %v", err)
	}
	select {
	case ack := <-acks:
		return ack
	case <-ctx.Done():
		t.Fatal("no llegó el acuse de la propuesta")
		return proto.ProjectProposalAck{}
	}
}

func proposalForTest() proto.ProjectProposal {
	return proto.ProjectProposal{
		ProposalID: uuid.New(),
		Name:       "svdemo",
		Action:     proto.ProposalPut,
		YAML:       "name: svdemo\n",
		ExpiresAt:  time.Now().Add(time.Hour).UTC(),
	}
}

func TestClientStoresProposals(t *testing.T) {
	fake := newFakeControlPlane(t)
	client := newTestClient(t, fake)
	store := &fakeProposals{seen: make(chan proto.ProjectProposal, 1)}
	changed := make(chan struct{}, 1)
	client.SetProposals(store, func() { changed <- struct{}{} })
	runClient(t, client)

	p := proposalForTest()
	ack := sendProposal(t, fake, p)
	if ack.ProposalID != p.ProposalID || ack.Result != proto.ProposalStored {
		t.Errorf("acuse = %+v", ack)
	}
	if got := <-store.seen; got.YAML != p.YAML {
		t.Errorf("el almacén recibió %+v", got)
	}
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Error("no avisó que cambiaron las propuestas")
	}
}

// Sin almacén (la prueba de carga, por ejemplo), una propuesta se rechaza.
func TestClientWithoutStoreRejectsProposals(t *testing.T) {
	fake := newFakeControlPlane(t)
	client := newTestClient(t, fake)
	runClient(t, client)

	ack := sendProposal(t, fake, proposalForTest())
	if ack.Result != proto.ProposalRejected || ack.Error == "" {
		t.Errorf("acuse = %+v", ack)
	}
	if err := ack.Validate(); err != nil {
		t.Errorf("el acuse no entra en el contrato: %v", err)
	}
}
