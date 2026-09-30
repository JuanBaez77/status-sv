package deploy

import (
	"os"
	"path/filepath"
	"strings"
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

func ptr[T any](v T) *T { return &v }
