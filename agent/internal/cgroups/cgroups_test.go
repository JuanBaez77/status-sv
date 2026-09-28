package cgroups

import (
	"errors"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// wslUbuntu son archivos reales de cgroup v2, capturados de WSL Ubuntu con
// systemd 255 (ver testdata/README.md).
func wslUbuntu() *Reader {
	return NewReader(os.DirFS("testdata/wsl-ubuntu"))
}

func TestReadRealService(t *testing.T) {
	stat, err := wslUbuntu().Read("/system.slice/dbus.service")
	if err != nil {
		t.Fatalf("leer: %v", err)
	}
	if want := 81441 * time.Microsecond; stat.CPUUsage != want {
		t.Errorf("CPU: obtuve %v, esperaba %v", stat.CPUUsage, want)
	}
	// memory.current (2633728) menos inactive_file (1667072).
	if want := uint64(966656); stat.MemoryBytes != want {
		t.Errorf("memoria: obtuve %d, esperaba %d", stat.MemoryBytes, want)
	}
}

func TestAvailable(t *testing.T) {
	if !wslUbuntu().Available() {
		t.Error("la jerarquía de WSL es cgroup v2 y no se reconoció")
	}
	// Con cgroup v1, /sys/fs/cgroup tiene un directorio por controlador y no
	// existe cgroup.controllers.
	v1 := NewReader(fstest.MapFS{"cpu/tasks": {Data: []byte("1\n")}})
	if v1.Available() {
		t.Error("una jerarquía v1 no tiene que reconocerse como v2")
	}
}

func TestReadMissingGroup(t *testing.T) {
	_, err := wslUbuntu().Read("/system.slice/no-existe.service")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("esperaba ErrNotFound, obtuve %v", err)
	}
}

func TestReadMemoryVariants(t *testing.T) {
	tests := []struct {
		name  string
		files fstest.MapFS
		want  uint64
	}{
		{
			name: "sin memory.stat, cuenta la caché",
			files: fstest.MapFS{
				"a/cpu.stat":       {Data: []byte("usage_usec 10\n")},
				"a/memory.current": {Data: []byte("5000\n")},
			},
			want: 5000,
		},
		{
			name: "memory.stat sin inactive_file",
			files: fstest.MapFS{
				"a/cpu.stat":       {Data: []byte("usage_usec 10\n")},
				"a/memory.current": {Data: []byte("5000\n")},
				"a/memory.stat":    {Data: []byte("anon 100\n")},
			},
			want: 5000,
		},
		{
			// Las dos lecturas no son atómicas: si la caché creció en el medio,
			// no puede quedar un número negativo dado vuelta.
			name: "inactive_file mayor que la memoria",
			files: fstest.MapFS{
				"a/cpu.stat":       {Data: []byte("usage_usec 10\n")},
				"a/memory.current": {Data: []byte("5000\n")},
				"a/memory.stat":    {Data: []byte("inactive_file 6000\n")},
			},
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stat, err := NewReader(tt.files).Read("/a")
			if err != nil {
				t.Fatalf("leer: %v", err)
			}
			if stat.MemoryBytes != tt.want {
				t.Errorf("obtuve %d, esperaba %d", stat.MemoryBytes, tt.want)
			}
		})
	}
}

func TestReadRejectsBadFiles(t *testing.T) {
	tests := map[string]fstest.MapFS{
		"cpu.stat sin usage_usec": {
			"a/cpu.stat":       {Data: []byte("user_usec 1\n")},
			"a/memory.current": {Data: []byte("1\n")},
		},
		"usage_usec que no es número": {
			"a/cpu.stat":       {Data: []byte("usage_usec muchos\n")},
			"a/memory.current": {Data: []byte("1\n")},
		},
		"memory.current que no es número": {
			"a/cpu.stat":       {Data: []byte("usage_usec 1\n")},
			"a/memory.current": {Data: []byte("max\n")},
		},
	}
	for name, files := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := NewReader(files).Read("/a"); err == nil {
				t.Fatal("se esperaba un error")
			}
		})
	}
}

// Una ruta de cgroup no puede servir para leer fuera de la jerarquía.
func TestReadRejectsEscapes(t *testing.T) {
	r := wslUbuntu()
	for _, group := range []string{"system.slice/dbus.service", "", "/../../etc"} {
		if _, err := r.Read(group); err == nil {
			t.Errorf("la ruta %q no tendría que leerse", group)
		}
	}
}

func TestContainerGroup(t *testing.T) {
	id := strings.Repeat("0a", 32)
	tests := []struct {
		name  string
		files fstest.MapFS
		want  string
		found bool
	}{
		{
			name:  "driver systemd, el de un VPS",
			files: fstest.MapFS{"system.slice/docker-" + id + ".scope/cpu.stat": {}},
			want:  "/system.slice/docker-" + id + ".scope",
			found: true,
		},
		{
			name:  "driver cgroupfs, el de Docker Desktop",
			files: fstest.MapFS{"docker/" + id + "/cpu.stat": {}},
			want:  "/docker/" + id,
			found: true,
		},
		{name: "contenedor detenido", files: fstest.MapFS{"cgroup.controllers": {}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := NewReader(tt.files).ContainerGroup(id)
			if got != tt.want || found != tt.found {
				t.Errorf("obtuve (%q, %v), esperaba (%q, %v)", got, found, tt.want, tt.found)
			}
		})
	}
}

func TestParseProcCgroup(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
		ok   bool
	}{
		// Capturados de WSL Ubuntu con systemd 255.
		{name: "servicio", data: "0::/system.slice/dbus.service\n", want: "/system.slice/dbus.service", ok: true},
		{name: "systemd", data: "0::/init.scope\n", want: "/init.scope", ok: true},
		{
			name: "híbrido v1 y v2",
			data: "12:memory:/system.slice/a.service\n1:name=systemd:/system.slice/a.service\n0::/system.slice/a.service\n",
			want: "/system.slice/a.service", ok: true,
		},
		{name: "solo v1", data: "12:memory:/docker/abc\n1:name=systemd:/docker/abc\n"},
		{name: "vacío", data: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseProcCgroup(tt.data)
			if got != tt.want || ok != tt.ok {
				t.Errorf("obtuve (%q, %v), esperaba (%q, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	id := strings.Repeat("9f", 32)
	tests := []struct {
		group string
		want  Owner
	}{
		{"/", Owner{Kind: KindSystem}},
		{"/init.scope", Owner{Kind: KindSystem}},
		{"/system.slice/docker-" + id + ".scope", Owner{Kind: KindContainer, ID: id}},
		// Un proceso anidado dentro del contenedor sigue siendo del contenedor.
		{"/system.slice/docker-" + id + ".scope/init", Owner{Kind: KindContainer, ID: id}},
		{"/docker/" + id, Owner{Kind: KindContainer, ID: id}},
		{"/system.slice/nginx.service", Owner{Kind: KindService, ID: "nginx.service"}},
		{"/system.slice/docker.service", Owner{Kind: KindService, ID: "docker.service"}},
		{"/system.slice/system-gunicorn.slice/gunicorn@cristales.service", Owner{Kind: KindService, ID: "gunicorn@cristales.service"}},
		// Lo que se arranca a mano: sesión SSH, systemd del usuario, systemd-run.
		{"/user.slice/user-1000.slice/session-3.scope", Owner{Kind: KindUnmanaged}},
		{"/user.slice/user-1000.slice/user@1000.service/app.slice/tmux.scope", Owner{Kind: KindUnmanaged}},
		{"/system.slice/run-r1b2c3.scope", Owner{Kind: KindUnmanaged}},
		// Un "docker-" que no es un ID de contenedor no engaña a la clasificación.
		{"/system.slice/docker-compose-algo.scope", Owner{Kind: KindUnmanaged}},
	}
	for _, tt := range tests {
		t.Run(tt.group, func(t *testing.T) {
			if got := Classify(tt.group); got != tt.want {
				t.Errorf("obtuve %+v, esperaba %+v", got, tt.want)
			}
		})
	}
}
