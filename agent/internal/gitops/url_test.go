package gitops

import (
	"errors"
	"testing"
)

func TestFetchURL(t *testing.T) {
	tests := []struct {
		name      string
		repo      string
		hasKey    bool
		allowFile bool
		want      string
		wantErr   bool
	}{
		{name: "corta con llave", repo: "github.com/extendsystem/cristales-baez", hasKey: true, want: "ssh://git@github.com/extendsystem/cristales-baez.git"},
		{name: "corta sin llave", repo: "github.com/extendsystem/cristales-baez", want: "https://github.com/extendsystem/cristales-baez.git"},
		{name: "corta con .git", repo: "github.com/a/b.git", want: "https://github.com/a/b.git"},
		{name: "corta con subgrupos", repo: "gitlab.com/grupo/sub/repo", hasKey: true, want: "ssh://git@gitlab.com/grupo/sub/repo.git"},
		{name: "scp con llave", repo: "git@github.com:extendsystem/facturas.git", hasKey: true, want: "ssh://git@github.com/extendsystem/facturas.git"},
		{name: "scp sin llave", repo: "git@github.com:a/b.git", wantErr: true},
		{name: "https tal cual", repo: "https://github.com/a/b.git", want: "https://github.com/a/b.git"},
		{name: "https con usuario", repo: "https://juan:token@github.com/a/b.git", wantErr: true},
		{name: "https con parámetros", repo: "https://github.com/a/b.git?x=1", wantErr: true},
		{name: "ssh con llave", repo: "ssh://git@github.com/a/b.git", hasKey: true, want: "ssh://git@github.com/a/b.git"},
		{name: "ssh sin llave", repo: "ssh://git@github.com/a/b.git", wantErr: true},
		{name: "ssh con contraseña", repo: "ssh://git:clave@github.com/a/b.git", hasKey: true, wantErr: true},
		{name: "file sin permiso", repo: "file:///srv/origen.git", wantErr: true},
		{name: "file en pruebas", repo: "file:///srv/origen.git", allowFile: true, want: "file:///srv/origen.git"},
		// Los transportes que ejecutan cosas o que no cifran no se aceptan.
		{name: "ext", repo: "ext::sh -c touch% /tmp/x", wantErr: true},
		{name: "git://", repo: "git://github.com/a/b.git", wantErr: true},
		{name: "http", repo: "http://github.com/a/b.git", wantErr: true},
		{name: "sin dueño", repo: "github.com/repo", wantErr: true},
		{name: "servidor sin punto", repo: "localhost/a/b", wantErr: true},
		{name: "con ..", repo: "github.com/a/../b", wantErr: true},
		{name: "que parece opción", repo: "-oProxyCommand=x/a/b", hasKey: true, wantErr: true},
		{name: "scp con opción", repo: "git@-oProxyCommand=x:a/b", hasKey: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FetchURL(tt.repo, tt.hasKey, tt.allowFile)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("FetchURL(%q) = %q, se esperaba error", tt.repo, got)
				}
				if !errors.Is(err, ErrRepoURL) {
					t.Errorf("el error no es ErrRepoURL: %v", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("FetchURL(%q) = %q, %v; se esperaba %q", tt.repo, got, err, tt.want)
			}
		})
	}
}

func TestOpenRejectsUnsafePaths(t *testing.T) {
	cfg := Config{Git: "/usr/bin/git", StateDir: "/var/lib/status-sv", KnownHosts: "/etc/status-sv/known_hosts"}
	if _, err := Open(cfg, "/srv/x", "/etc/status-sv/keys/x"); err != nil {
		t.Fatalf("Open con rutas normales: %v", err)
	}
	// La llave termina adentro de GIT_SSH_COMMAND, que pasa por un shell.
	for _, key := range []string{"/etc/status-sv/keys/x; rm -rf /", "/etc/status-sv/keys/$(id)", "relativa"} {
		if _, err := Open(cfg, "/srv/x", key); err == nil {
			t.Errorf("Open aceptó la llave %q", key)
		}
	}
	cfg.Git = "git"
	if _, err := Open(cfg, "/srv/x", ""); err == nil {
		t.Error("Open aceptó git sin ruta absoluta")
	}
}

func TestSSHCommand(t *testing.T) {
	r, err := Open(Config{Git: "/usr/bin/git", KnownHosts: "/etc/status-sv/known_hosts"}, "/srv/x", "/etc/status-sv/keys/x")
	if err != nil {
		t.Fatal(err)
	}
	want := "ssh -F /dev/null -o BatchMode=yes -o ConnectTimeout=15 -o StrictHostKeyChecking=yes " +
		"-o UserKnownHostsFile=/etc/status-sv/known_hosts -i /etc/status-sv/keys/x -o IdentitiesOnly=yes"
	if got := r.sshCommand(); got != want {
		t.Errorf("sshCommand =\n%s\nse esperaba\n%s", got, want)
	}
}
