package proto_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/JuanBaez77/status-sv/proto"
)

const proposalYAML = `name: svdemo
environment: staging
repo: github.com/JuanBaez77/status-sv-demo
branch: main
runtime: systemd
workdir: /srv/svdemo
run_as: svdemo
env_file: /etc/status-sv/env/svdemo.env
required_env: [SECRET_KEY]
service:
  command: [.venv/bin/gunicorn, app:app, --bind, 127.0.0.1:18097]
site:
  domain: svdemo.extendsystem.com
  port: 18097
`

// goldenKey es una llave pública ed25519 de ejemplo, sin comentario.
const goldenKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"

var goldenProposalID = uuid.MustParse("01924f9c-0000-7000-8000-0000000000bb")

func validProposal() proto.ProjectProposal {
	return proto.ProjectProposal{
		ProposalID: goldenProposalID,
		Name:       "svdemo",
		Action:     proto.ProposalPut,
		YAML:       proposalYAML,
		ExpiresAt:  time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC),
	}
}

// altaProject es un proyecto declarado con el alta: con su hash, su sitio, sus
// variables y su llave.
func altaProject() proto.Project {
	return proto.Project{
		Name:        "svdemo",
		Environment: proto.EnvStaging,
		Repo:        "github.com/JuanBaez77/status-sv-demo",
		Branch:      "main",
		Runtime:     proto.RuntimeSystemd,
		Workdir:     "/srv/svdemo",
		Units:       []string{"svdemo.service"},
		SpecHash:    proto.SpecHash([]byte(proposalYAML)),
		Site:        &proto.SiteSpec{Domain: "svdemo.extendsystem.com", Port: 18097},
		Env:         &proto.EnvStatus{Required: 2, Missing: []string{"DATABASE_URL"}},
		DeployKey:   goldenKey,
	}
}

func TestProjectProposalValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*proto.ProjectProposal)
		wantErr string
	}{
		{name: "válida", mutate: func(*proto.ProjectProposal) {}},
		{name: "retirar", mutate: func(p *proto.ProjectProposal) {
			p.Action, p.YAML, p.ExpiresAt = proto.ProposalWithdraw, "", time.Time{}
		}},
		{name: "sin ID", mutate: func(p *proto.ProjectProposal) { p.ProposalID = uuid.Nil }, wantErr: "ID"},
		{name: "nombre inválido", mutate: func(p *proto.ProjectProposal) { p.Name = "../etc" }, wantErr: "nombre"},
		{name: "acción inventada", mutate: func(p *proto.ProjectProposal) { p.Action = "apply" }, wantErr: "acción"},
		{name: "sin declaración", mutate: func(p *proto.ProjectProposal) { p.YAML = "" }, wantErr: "declaración"},
		{name: "declaración enorme", mutate: func(p *proto.ProjectProposal) { p.YAML = strings.Repeat("#", proto.MaxProposalBytes+1) }, wantErr: "pesa"},
		{name: "declaración que no es UTF-8", mutate: func(p *proto.ProjectProposal) { p.YAML = "name: \xff" }, wantErr: "UTF-8"},
		{name: "sin vencimiento", mutate: func(p *proto.ProjectProposal) { p.ExpiresAt = time.Time{} }, wantErr: "vencimiento"},
		{name: "retirar con declaración", mutate: func(p *proto.ProjectProposal) { p.Action = proto.ProposalWithdraw }, wantErr: "no lleva"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proposal := validProposal()
			tt.mutate(&proposal)
			err := proposal.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("Validate: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("Validate = %v, se esperaba un error que dijera %q", err, tt.wantErr)
			}
		})
	}
}

func TestProjectProposalAckValidate(t *testing.T) {
	valid := proto.ProjectProposalAck{ProposalID: goldenProposalID, Name: "svdemo", Result: proto.ProposalStored}
	tests := []struct {
		name    string
		mutate  func(*proto.ProjectProposalAck)
		wantErr bool
	}{
		{name: "guardada", mutate: func(*proto.ProjectProposalAck) {}},
		{name: "retirada", mutate: func(a *proto.ProjectProposalAck) { a.Result = proto.ProposalWithdrawn }},
		{name: "rechazada", mutate: func(a *proto.ProjectProposalAck) {
			a.Result, a.Error = proto.ProposalRejected, "workdir: en una propuesta es siempre /srv/<nombre>"
		}},
		{name: "rechazada sin por qué", mutate: func(a *proto.ProjectProposalAck) { a.Result = proto.ProposalRejected }, wantErr: true},
		{name: "guardada con error", mutate: func(a *proto.ProjectProposalAck) { a.Error = "x" }, wantErr: true},
		{name: "resultado inventado", mutate: func(a *proto.ProjectProposalAck) { a.Result = "accepted" }, wantErr: true},
		{name: "sin ID", mutate: func(a *proto.ProjectProposalAck) { a.ProposalID = uuid.Nil }, wantErr: true},
		{name: "error con control", mutate: func(a *proto.ProjectProposalAck) {
			a.Result, a.Error = proto.ProposalRejected, "a\x1b[31mb"
		}, wantErr: true},
		{name: "error larguísimo", mutate: func(a *proto.ProjectProposalAck) {
			a.Result, a.Error = proto.ProposalRejected, strings.Repeat("x", proto.MaxProjectErrorLen+1)
		}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ack := valid
			tt.mutate(&ack)
			if err := ack.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, se esperaba error: %v", err, tt.wantErr)
			}
		})
	}
}

func TestProjectAltaFieldsValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*proto.Project)
		wantErr bool
	}{
		{name: "válido", mutate: func(*proto.Project) {}},
		{name: "sin los campos del alta", mutate: func(p *proto.Project) { p.SpecHash, p.Site, p.Env, p.DeployKey = "", nil, nil, "" }},
		{name: "todas las variables cargadas", mutate: func(p *proto.Project) { p.Env.Missing = nil }},
		{name: "variables sin mirar", mutate: func(p *proto.Project) { p.Env = &proto.EnvStatus{Required: 3, Unknown: true} }},
		{name: "hash en mayúsculas", mutate: func(p *proto.Project) { p.SpecHash = strings.ToUpper(p.SpecHash) }, wantErr: true},
		{name: "hash corto", mutate: func(p *proto.Project) { p.SpecHash = "abc" }, wantErr: true},
		{name: "dominio comodín", mutate: func(p *proto.Project) { p.Site.Domain = "*.extendsystem.com" }, wantErr: true},
		{name: "dominio con directiva", mutate: func(p *proto.Project) { p.Site.Domain = "a.com; return 200" }, wantErr: true},
		{name: "puerto reservado", mutate: func(p *proto.Project) { p.Site.Port = 443 }, wantErr: true},
		{name: "sin variables declaradas", mutate: func(p *proto.Project) { p.Env = &proto.EnvStatus{} }, wantErr: true},
		{name: "faltan más de las declaradas", mutate: func(p *proto.Project) { p.Env.Missing = []string{"A", "B", "C"} }, wantErr: true},
		{name: "falta un nombre inválido", mutate: func(p *proto.Project) { p.Env.Missing = []string{"a=b"} }, wantErr: true},
		{name: "falta un nombre repetido", mutate: func(p *proto.Project) { p.Env.Missing = []string{"A", "A"} }, wantErr: true},
		{name: "sin mirar pero con faltantes", mutate: func(p *proto.Project) { p.Env.Unknown = true }, wantErr: true},
		{name: "llave rsa", mutate: func(p *proto.Project) { p.DeployKey = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ==" }},
		{name: "llave con comentario", mutate: func(p *proto.Project) { p.DeployKey = goldenKey + " root@sv-gcg" }, wantErr: true},
		{name: "llave privada", mutate: func(p *proto.Project) { p.DeployKey = "-----BEGIN OPENSSH PRIVATE KEY-----" }, wantErr: true},
		{name: "llave de tipo inventado", mutate: func(p *proto.Project) { p.DeployKey = "ssh-dss AAAA" }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			project := altaProject()
			tt.mutate(&project)
			if err := project.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, se esperaba error: %v", err, tt.wantErr)
			}
		})
	}
}

func TestProjectsStateProposals(t *testing.T) {
	pending := func(name string) proto.PendingProposal {
		return proto.PendingProposal{ProposalID: uuid.New(), Name: name, Hash: proto.SpecHash([]byte(name))}
	}
	state := validProjectsState()
	state.Proposals = []proto.PendingProposal{pending("svdemo"), pending("otro")}
	if err := state.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	// Las propuestas son del directorio de estado del agent: viajan aunque no
	// se haya podido leer el de proyectos.
	unavailable := state
	unavailable.Source = proto.RuntimeSource{Status: proto.SourceUnavailable, Detail: "no existe /etc/status-sv/projects.d"}
	unavailable.Projects, unavailable.Invalid = nil, nil
	if err := unavailable.Validate(); err != nil {
		t.Errorf("sin directorio de proyectos: %v", err)
	}

	repeated := state
	repeated.Proposals = []proto.PendingProposal{pending("svdemo"), pending("svdemo")}
	if err := repeated.Validate(); err == nil {
		t.Error("se aceptaron dos propuestas con el mismo nombre")
	}

	tooMany := state
	tooMany.Proposals = nil
	for i := range proto.MaxPendingProposals + 1 {
		tooMany.Proposals = append(tooMany.Proposals, pending("p"+strings.Repeat("x", i)))
	}
	if err := tooMany.Validate(); err == nil {
		t.Error("se aceptaron más propuestas que el máximo")
	}

	badHash := state
	badHash.Proposals = []proto.PendingProposal{{ProposalID: uuid.New(), Name: "svdemo", Hash: "x"}}
	if err := badHash.Validate(); err == nil {
		t.Error("se aceptó una propuesta con un hash inválido")
	}
}

// La propuesta más grande, con todo escapado por JSON, entra en un mensaje.
func TestLargestProposalFitsInAMessage(t *testing.T) {
	// "<" es el peor caso: encoding/json lo escapa como <, seis bytes.
	proposal := validProposal()
	proposal.YAML = strings.Repeat("<", proto.MaxProposalBytes)
	if err := proposal.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	raw, err := proto.Marshal(proposal)
	if err != nil {
		t.Fatalf("serializar: %v", err)
	}
	if len(raw) > proto.MaxMessageBytes {
		t.Errorf("la propuesta más grande ocupa %d bytes y el tope es %d", len(raw), proto.MaxMessageBytes)
	}
}

func TestSpecHash(t *testing.T) {
	// SHA-256 de la cadena vacía: es el hash de siempre, no uno propio.
	if got := proto.SpecHash(nil); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("SpecHash(nil) = %s", got)
	}
	if proto.SpecHash([]byte(proposalYAML)) == proto.SpecHash([]byte(proposalYAML+" ")) {
		t.Error("dos declaraciones distintas dan el mismo hash")
	}
}

func TestValidDomain(t *testing.T) {
	for _, domain := range []string{"svdemo.extendsystem.com", "a.b", "xn--and-6ma2c.com.ar", "a-1.b-2.c3", "api.v2.example.io"} {
		if !proto.ValidDomain(domain) {
			t.Errorf("ValidDomain(%q) = false", domain)
		}
	}
	for _, domain := range []string{
		"", "com", "a..b", ".a.b", "a.b.", "-a.b", "a-.b", "a_b.com", "A.com", "*.a.com", "a.com:80",
		"1.2.3.4", "a.123", "a b.com", "a.com\n", strings.Repeat("a", 64) + ".com",
		strings.Repeat(strings.Repeat("a", 60)+".", 5) + "com",
	} {
		if proto.ValidDomain(domain) {
			t.Errorf("ValidDomain(%q) = true", domain)
		}
	}
}

func TestValidEnvName(t *testing.T) {
	for _, name := range []string{"SECRET_KEY", "_X", "A1", strings.Repeat("A", proto.MaxEnvNameLen)} {
		if !proto.ValidEnvName(name) {
			t.Errorf("ValidEnvName(%q) = false", name)
		}
	}
	for _, name := range []string{"", "secret", "1A", "A-B", "A=B", "A B", strings.Repeat("A", proto.MaxEnvNameLen+1)} {
		if proto.ValidEnvName(name) {
			t.Errorf("ValidEnvName(%q) = true", name)
		}
	}
}
