package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const (
	imageV1 = "sha256:" + "11111111111111111111111111111111111111111111111111111111111111aa"
	imageV2 = "sha256:" + "22222222222222222222222222222222222222222222222222222222222222bb"
)

// fakeImages imita las rutas de imágenes de la API con un mapa de
// referencias a IDs, como el de Docker.
type fakeImages struct {
	mu       sync.Mutex
	tags     map[string]string // referencia → ID
	requests []string
	filters  string
}

func (f *fakeImages) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/containers/json":
		f.filters = r.URL.Query().Get("filters")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"Id": idWeb, "Labels": map[string]string{labelComposeProject: "tienda", labelComposeService: "web"}},
			{"Id": idDB, "Labels": map[string]string{labelComposeProject: "tienda", labelComposeService: "migrate", labelComposeOneOff: "True"}},
			// Otro proyecto que empieza igual: versiones viejas filtran por prefijo.
			{"Id": strings.Repeat("c3", 32), "Labels": map[string]string{labelComposeProject: "tienda-vieja", labelComposeService: "web"}},
		})

	case r.Method == http.MethodGet && r.URL.Path == "/containers/"+idWeb+"/json":
		_, _ = w.Write([]byte(`{"Image":"` + imageV2 + `","Config":{"Image":"tienda-web"}}`))

	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/images/") && strings.HasSuffix(r.URL.Path, "/json") && r.URL.Path != "/images/json":
		ref := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/images/"), "/json")
		if f.resolve(ref) == "" {
			http.Error(w, `{"message":"No such image"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"Id":"` + f.resolve(ref) + `"}`))

	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/tag"):
		id := f.resolve(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/images/"), "/tag"))
		if id == "" {
			http.Error(w, `{"message":"No such image"}`, http.StatusNotFound)
			return
		}
		f.tags[r.URL.Query().Get("repo")+":"+r.URL.Query().Get("tag")] = id
		w.WriteHeader(http.StatusCreated)

	case r.Method == http.MethodDelete:
		ref := strings.TrimPrefix(r.URL.Path, "/images/")
		if _, ok := f.tags[ref]; !ok {
			http.Error(w, `{"message":"No such image"}`, http.StatusNotFound)
			return
		}
		delete(f.tags, ref)
		_, _ = w.Write([]byte(`[{"Untagged":"` + ref + `"}]`))

	case r.Method == http.MethodGet && r.URL.Path == "/images/json":
		f.filters = r.URL.Query().Get("filters")
		var list []map[string]any
		for ref := range f.tags {
			if strings.HasPrefix(ref, "statussv-rollback/") {
				list = append(list, map[string]any{"RepoTags": []string{ref}})
			}
		}
		_ = json.NewEncoder(w).Encode(list)

	default:
		http.NotFound(w, r)
	}
}

func (f *fakeImages) resolve(ref string) string {
	if strings.HasPrefix(ref, "sha256:") {
		for _, id := range f.tags {
			if id == ref {
				return id
			}
		}
		return ""
	}
	if !strings.Contains(ref[strings.LastIndex(ref, "/")+1:], ":") {
		ref += ":latest"
	}
	return f.tags[ref]
}

func startFakeImages(t *testing.T) (*Client, *fakeImages) {
	t.Helper()
	f := &fakeImages{tags: map[string]string{"tienda-web:latest": imageV2, "statussv-rollback/tienda-web:aaaaaaaaaaaa": imageV1}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return newClient(srv.Client(), srv.URL), f
}

func TestProjectContainers(t *testing.T) {
	client, fake := startFakeImages(t)
	containers, err := client.ProjectContainers(context.Background(), "tienda")
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 1 || containers[0].ID != idWeb || ServiceOf(containers[0]) != "web" {
		t.Fatalf("contenedores = %+v", containers)
	}
	if fake.filters != `{"label":["com.docker.compose.project=tienda"]}` {
		t.Errorf("filtro = %s", fake.filters)
	}
}

func TestImageOf(t *testing.T) {
	client, _ := startFakeImages(t)
	img, err := client.ImageOf(context.Background(), idWeb)
	if err != nil {
		t.Fatal(err)
	}
	if img.ID != imageV2 || img.Ref != "tienda-web" {
		t.Errorf("imagen = %+v", img)
	}
}

func TestTagMovesTheReference(t *testing.T) {
	client, fake := startFakeImages(t)
	ctx := context.Background()

	// Volver atrás: la referencia que usa el compose pasa a apuntar a v1.
	if err := client.Tag(ctx, imageV1, "tienda-web", "latest"); err != nil {
		t.Fatalf("Tag: %v", err)
	}
	if fake.tags["tienda-web:latest"] != imageV1 {
		t.Errorf("tienda-web:latest apunta a %s", fake.tags["tienda-web:latest"])
	}

	exists, err := client.ImageExists(ctx, imageV1)
	if err != nil || !exists {
		t.Errorf("ImageExists(v1) = %v, %v", exists, err)
	}
	exists, err = client.ImageExists(ctx, "sha256:"+strings.Repeat("0", 64))
	if err != nil || exists {
		t.Errorf("ImageExists de una imagen que no existe = %v, %v", exists, err)
	}
	if err := client.Tag(ctx, "sha256:"+strings.Repeat("0", 64), "x", "y"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Tag de una imagen que no existe = %v", err)
	}
}

func TestUntagAndList(t *testing.T) {
	client, fake := startFakeImages(t)
	ctx := context.Background()

	refs, err := client.ImagesByReference(ctx, "statussv-rollback/tienda-*")
	if err != nil || len(refs) != 1 || refs[0] != "statussv-rollback/tienda-web:aaaaaaaaaaaa" {
		t.Fatalf("ImagesByReference = %v, %v", refs, err)
	}
	if fake.filters != `{"reference":["statussv-rollback/tienda-*"]}` {
		t.Errorf("filtro = %s", fake.filters)
	}
	if err := client.Untag(ctx, refs[0]); err != nil {
		t.Fatalf("Untag: %v", err)
	}
	if _, ok := fake.tags[refs[0]]; ok {
		t.Error("la etiqueta sigue")
	}
	// Sacar una etiqueta que ya no está no es un error.
	if err := client.Untag(ctx, refs[0]); err != nil {
		t.Errorf("Untag de nuevo: %v", err)
	}
	last := fake.requests[len(fake.requests)-1]
	if !strings.Contains(last, "noprune=1") {
		t.Errorf("el borrado no lleva noprune: %s", last)
	}
}

func TestSplitRef(t *testing.T) {
	tests := []struct {
		ref, repo, tag string
		ok             bool
	}{
		{"tienda-web", "tienda-web", "latest", true},
		{"postgres:17", "postgres", "17", true},
		{"ghcr.io/extendsystem/cristales:2026.09.20", "ghcr.io/extendsystem/cristales", "2026.09.20", true},
		{"registro:5000/app", "registro:5000/app", "latest", true},
		{"registro:5000/app:1.2", "registro:5000/app", "1.2", true},
		{"app@sha256:abc", "", "", false},
		{"", "", "", false},
		{"app:", "", "", false},
	}
	for _, tt := range tests {
		repo, tag, ok := SplitRef(tt.ref)
		if repo != tt.repo || tag != tt.tag || ok != tt.ok {
			t.Errorf("SplitRef(%q) = %q, %q, %v", tt.ref, repo, tag, ok)
		}
	}
}
