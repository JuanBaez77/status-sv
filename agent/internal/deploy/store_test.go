package deploy

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
)

func good(commit string) GoodState {
	return GoodState{Commit: commit, At: time.Now().UTC()}
}

func TestPushStateKeepsThreeWithoutRepeats(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := func(n byte) string { return strings.Repeat(string('0'+n), 40) }

	st, _ := s.pushState("x", good(c(2)), ptr(good(c(1))))
	if got := st.commits(); strings.Join(got, ",") != c(2)+","+c(1) {
		t.Fatalf("historia = %v", got)
	}
	_, _ = s.pushState("x", good(c(3)), ptr(good(c(2))))
	st, _ = s.pushState("x", good(c(4)), ptr(good(c(3))))
	if got := st.commits(); strings.Join(got, ",") != c(4)+","+c(3)+","+c(2) {
		t.Fatalf("historia = %v; se esperaban los tres últimos", got)
	}
	// Un rollback vuelve al anterior: no se repite en la historia.
	st, _ = s.pushState("x", good(c(3)), ptr(good(c(4))))
	if got := st.commits(); strings.Join(got, ",") != c(3)+","+c(4)+","+c(2) {
		t.Fatalf("historia después de volver = %v", got)
	}
	// Queda en disco.
	loaded, err := s.loadState("x")
	if err != nil || len(loaded.History) != 3 {
		t.Fatalf("leída = %+v, %v", loaded, err)
	}
	if empty, err := s.loadState("nuevo"); err != nil || len(empty.History) != 0 {
		t.Fatalf("un proyecto sin historia = %+v, %v", empty, err)
	}
}

func TestTrimJournalsKeepsRunningAndUnacked(t *testing.T) {
	dir := t.TempDir()
	s, err := newStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	save := func(i int, status proto.DeployStatus, acked bool) uuid.UUID {
		id := uuid.Must(uuid.NewV7())
		j := &journal{Event: proto.DeployEvent{DeployID: id, Seq: 5, Project: "x", Kind: proto.DeployKindDeploy, Status: status, StartedAt: start.Add(time.Duration(i) * time.Minute)}}
		if acked {
			j.AckedSeq = 5
		}
		if err := s.saveJournal(j); err != nil {
			t.Fatal(err)
		}
		return id
	}
	var oldest uuid.UUID
	for i := range keptJournals + 5 {
		id := save(i, proto.DeploySucceeded, true)
		if i == 0 {
			oldest = id
		}
	}
	unacked := save(-10, proto.DeployFailed, false)
	running := save(-11, proto.DeployRunning, false)
	// Basura que no es un diario: no traba nada.
	if err := os.WriteFile(filepath.Join(dir, "no-es-un-id.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.trimJournals(); err != nil {
		t.Fatal(err)
	}
	all, err := s.journals()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != keptJournals+2 {
		t.Errorf("quedaron %d diarios, se esperaban %d", len(all), keptJournals+2)
	}
	for _, id := range []uuid.UUID{unacked, running} {
		if _, err := s.loadJournal(id); err != nil {
			t.Errorf("se borró un diario que no había que borrar: %v", err)
		}
	}
	if _, err := s.loadJournal(oldest); err == nil {
		t.Error("el diario más viejo sigue")
	}
}

func TestAckJournalOnlyChangesTheAck(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := s.ackJournal(id, 1); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("acuse de un deploy sin diario: %v", err)
	}

	// El deploy tiene el diario en memoria; el acuse lo anota en disco.
	mem := &journal{Event: proto.DeployEvent{DeployID: id, Seq: 3, Project: "x", Status: proto.DeployRunning}, Touched: true}
	if err := s.saveJournal(mem); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ackJournal(id, 2); err != nil {
		t.Fatal(err)
	}
	// Un acuse más viejo no lo baja.
	if j, err := s.ackJournal(id, 1); err != nil || j.AckedSeq != 2 {
		t.Fatalf("acuse viejo: %+v, %v", j, err)
	}

	// La foto final, guardada desde la copia en memoria, no pierde el acuse.
	mem.Event.Seq, mem.Event.Status = 4, proto.DeploySucceeded
	if err := s.saveJournal(mem); err != nil {
		t.Fatal(err)
	}
	j, err := s.ackJournal(id, 4)
	if err != nil {
		t.Fatal(err)
	}
	if j.Event.Status != proto.DeploySucceeded || j.Event.Seq != 4 || j.AckedSeq != 4 || !j.Touched {
		t.Errorf("diario = %+v", j)
	}
}

// Un acuse que llega mientras el deploy escribe su foto final no puede dejar
// en disco una copia vieja: el deploy terminado figuraría en curso y el agent
// lo volvería atrás al próximo arranque.
func TestAckDoesNotOverwriteANewerJournal(t *testing.T) {
	s, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const photos = 300
	for range 5 {
		id := uuid.Must(uuid.NewV7())
		mem := &journal{Event: proto.DeployEvent{DeployID: id, Seq: 1, Project: "x", Status: proto.DeployRunning}}
		if err := s.saveJournal(mem); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		wg.Go(func() {
			for seq := 2; seq <= photos; seq++ {
				mem.Event.Seq = seq
				if seq == photos {
					mem.Event.Status = proto.DeploySucceeded
				}
				if err := s.saveJournal(mem); err != nil {
					t.Error(err)
					return
				}
			}
		})
		wg.Go(func() {
			for seq := 1; seq < photos; seq++ {
				if _, err := s.ackJournal(id, seq); err != nil {
					t.Error(err)
					return
				}
			}
		})
		wg.Wait()

		j, err := s.loadJournal(id)
		if err != nil {
			t.Fatal(err)
		}
		if j.Event.Status != proto.DeploySucceeded || j.Event.Seq != photos {
			t.Fatalf("el diario quedó en %s, foto %d: un acuse pisó la foto final", j.Event.Status, j.Event.Seq)
		}
		if j.AckedSeq != photos-1 {
			t.Fatalf("acked_seq = %d, se esperaba %d", j.AckedSeq, photos-1)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// Active lee los diarios desde afuera del motor: lo usa project accept, como
// root, antes de reiniciar el agent.
func TestActive(t *testing.T) {
	stateDir := t.TempDir()
	if active, err := Active(stateDir); err != nil || len(active) != 0 {
		t.Fatalf("sin diarios: %v, %v", active, err)
	}

	s, err := newStore(filepath.Join(stateDir, "deploys"))
	if err != nil {
		t.Fatal(err)
	}
	for project, status := range map[string]proto.DeployStatus{
		"tienda": proto.DeployRunning, "svdemo": proto.DeploySucceeded, "otro": proto.DeployRolledBack,
	} {
		j := &journal{Event: proto.DeployEvent{DeployID: uuid.Must(uuid.NewV7()), Seq: 1, Project: project, Status: status}}
		if err := s.saveJournal(j); err != nil {
			t.Fatal(err)
		}
	}
	active, err := Active(stateDir)
	if err != nil || len(active) != 1 || active[0] != "tienda" {
		t.Errorf("Active = %v, %v", active, err)
	}
}
