package approvals

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const commit = "4f1c9a2b7d3e4f1c9a2b7d3e4f1c9a2b7d3e4f1c"

func request(op Operation) Request {
	r := Request{
		Operation: op, ServerID: uuid.New(), ProjectID: uuid.New(), ProjectName: "tienda",
		Environment: "production", TokenID: uuid.New(), TokenName: "claude-code",
		TokenPrefix: "svmcp_AbCdEf", IP: "203.0.113.7",
	}
	if op == OpDeploy {
		r.Commit = commit
	}
	return r
}

func TestValidateAcceptsEachOperation(t *testing.T) {
	for _, op := range []Operation{OpDeploy, OpRollback, OpRestart, OpBootstrap} {
		if _, err := validate(request(op)); err != nil {
			t.Errorf("%s: %v", op, err)
		}
	}
	r := request(OpRestart)
	r.Unit = "tienda-web-1"
	if _, err := validate(r); err != nil {
		t.Errorf("restart con unidad: %v", err)
	}
	r.Unit = "tienda-worker@2.service"
	if _, err := validate(r); err != nil {
		t.Errorf("restart de una unidad de systemd: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]func(*Request){
		"operación desconocida":    func(r *Request) { r.Operation = "exec" },
		"entorno desconocido":      func(r *Request) { r.Environment = "dev" },
		"sin proyecto":             func(r *Request) { r.ProjectID = uuid.Nil },
		"sin token":                func(r *Request) { r.TokenID = uuid.Nil },
		"nombre de proyecto vacío": func(r *Request) { r.ProjectName = "" },
		"prefijo que no es svmcp_": func(r *Request) { r.TokenPrefix = "svses_AbCdEf" },
		"deploy sin commit":        func(r *Request) { r.Commit = "" },
		"deploy con commit corto":  func(r *Request) { r.Commit = commit[:12] },
		"deploy con una rama":      func(r *Request) { r.Commit = "main" },
		"deploy con mayúsculas":    func(r *Request) { r.Commit = strings.ToUpper(commit) },
		"deploy con unidad":        func(r *Request) { r.Unit = "web" },
		"motivo larguísimo":        func(r *Request) { r.Reason = strings.Repeat("á", MaxReasonLen+1) },
		"motivo que no es UTF-8":   func(r *Request) { r.Reason = "\xff\xfe" },
	}
	for name, change := range cases {
		r := request(OpDeploy)
		change(&r)
		if _, err := validate(r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}

	rollback := request(OpRollback)
	rollback.Commit = commit
	if _, err := validate(rollback); !errors.Is(err, ErrInvalid) {
		t.Errorf("rollback con commit: err = %v", err)
	}
	for _, unit := range []string{"web; rm -rf /", "../etc/passwd", "-web", "web 1", strings.Repeat("a", 256)} {
		restart := request(OpRestart)
		restart.Unit = unit
		if _, err := validate(restart); !errors.Is(err, ErrInvalid) {
			t.Errorf("restart con unidad %q: err = %v", unit, err)
		}
	}
}

func TestValidateCleansTheReason(t *testing.T) {
	r := request(OpDeploy)
	r.Reason = "  arregla el total\x1b[31m del PDF\r\nsegunda línea\t \n"
	reason, err := validate(r)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "arregla el total [31m del PDF \nsegunda línea" {
		t.Errorf("motivo = %q", reason)
	}

	r.Reason = strings.Repeat("ñ", MaxReasonLen)
	if _, err := validate(r); err != nil {
		t.Errorf("motivo de %d caracteres: %v", MaxReasonLen, err)
	}
}

func TestPendingExistsErrorNamesTheRequest(t *testing.T) {
	id := uuid.New()
	err := error(&PendingExistsError{ID: id})
	if !errors.Is(err, ErrPendingExists) || !strings.Contains(err.Error(), id.String()) {
		t.Errorf("err = %v", err)
	}
}
