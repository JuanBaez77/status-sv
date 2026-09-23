package panel

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// panelFS imita lo que deja Vite en dist/.
func panelFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":              {Data: []byte(`<!doctype html><div id="root"></div>`)},
		"assets/index-abc123.js":  {Data: []byte("console.log('panel')")},
		"assets/index-abc123.css": {Data: []byte("body{}")},
		"sin-panel.html":          {Data: []byte("<!doctype html><p>sin panel")},
		".gitignore":              {Data: []byte("*\n")},
	}
}

func get(t *testing.T, h http.Handler, target string) *http.Response {
	t.Helper()
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	return recorder.Result()
}

func TestSirveElPanel(t *testing.T) {
	t.Parallel()
	h := HandlerFS(panelFS(), testLogger())

	t.Run("la raíz devuelve el index", func(t *testing.T) {
		t.Parallel()
		response := get(t, h, "/")
		defer response.Body.Close()

		body, _ := io.ReadAll(response.Body)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("estado = %d, se esperaba 200", response.StatusCode)
		}
		if got := string(body); got != `<!doctype html><div id="root"></div>` {
			t.Errorf("cuerpo = %q", got)
		}
		if got := response.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Errorf("Content-Type = %q", got)
		}
	})

	t.Run("una ruta del panel también devuelve el index", func(t *testing.T) {
		t.Parallel()
		// Las rutas las resuelve el front: el servidor no las conoce.
		response := get(t, h, "/servers/01924f9c-0000-7000-8000-0000000000aa")
		defer response.Body.Close()

		if response.StatusCode != http.StatusOK {
			t.Fatalf("estado = %d, se esperaba 200", response.StatusCode)
		}
	})

	t.Run("un archivo que no está es 404, no el index", func(t *testing.T) {
		t.Parallel()
		// Si devolviera el index, el navegador pediría un .js y recibiría HTML:
		// un despliegue a medias se vería como una página en blanco.
		response := get(t, h, "/assets/index-viejo.js")
		defer response.Body.Close()

		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("estado = %d, se esperaba 404", response.StatusCode)
		}
	})

	t.Run("los archivos con hash se guardan para siempre", func(t *testing.T) {
		t.Parallel()
		response := get(t, h, "/assets/index-abc123.js")
		defer response.Body.Close()

		if response.StatusCode != http.StatusOK {
			t.Fatalf("estado = %d, se esperaba 200", response.StatusCode)
		}
		if got := response.Header.Get("Cache-Control"); got != assetsCache {
			t.Errorf("Cache-Control = %q, se esperaba %q", got, assetsCache)
		}
	})

	t.Run("el javascript sale como javascript", func(t *testing.T) {
		t.Parallel()
		// El tipo lo fija el paquete y no el sistema: en Windows el registro
		// suele decir que un .js es texto plano, y con nosniff el navegador no
		// lo ejecuta.
		response := get(t, h, "/assets/index-abc123.js")
		defer response.Body.Close()

		if got := response.Header.Get("Content-Type"); got != "text/javascript; charset=utf-8" {
			t.Errorf("Content-Type = %q", got)
		}
	})

	t.Run("el index se revalida en cada visita", func(t *testing.T) {
		t.Parallel()
		// Es el que apunta a los archivos con hash: si el navegador se quedara
		// con uno viejo, no vería el despliegue nuevo.
		response := get(t, h, "/")
		defer response.Body.Close()

		if got := response.Header.Get("Cache-Control"); got != indexCache {
			t.Errorf("Cache-Control = %q, se esperaba %q", got, indexCache)
		}
		if response.Header.Get("ETag") == "" {
			t.Error("falta la etiqueta que permite contestar 304")
		}
	})

	t.Run("con la etiqueta al día contesta 304", func(t *testing.T) {
		t.Parallel()
		first := get(t, h, "/")
		first.Body.Close()

		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set("If-None-Match", first.Header.Get("ETag"))
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusNotModified {
			t.Fatalf("estado = %d, se esperaba 304", recorder.Code)
		}
	})

	t.Run("no sirve archivos ocultos", func(t *testing.T) {
		t.Parallel()
		response := get(t, h, "/.gitignore")
		defer response.Body.Close()

		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("estado = %d, se esperaba 404", response.StatusCode)
		}
	})

	t.Run("cada respuesta lleva la política de contenido", func(t *testing.T) {
		t.Parallel()
		response := get(t, h, "/")
		defer response.Body.Close()

		if got := response.Header.Get("Content-Security-Policy"); got != csp {
			t.Errorf("Content-Security-Policy = %q", got)
		}
		if got := response.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q", got)
		}
	})

	t.Run("solo contesta lecturas", func(t *testing.T) {
		t.Parallel()
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/", nil))

		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("estado = %d, se esperaba 405", recorder.Code)
		}
	})
}

func TestSinPanelCompilado(t *testing.T) {
	t.Parallel()
	// Un binario compilado sin pasar por el paso del panel: dist/ trae solo la
	// página de aviso.
	h := HandlerFS(fstest.MapFS{
		"sin-panel.html": {Data: []byte("<!doctype html><p>sin panel")},
	}, testLogger())

	response := get(t, h, "/")
	defer response.Body.Close()

	// 503 y no 404: el panel existe, este binario no lo tiene. Un 404 se
	// confundiría con una ruta mal escrita.
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("estado = %d, se esperaba 503", response.StatusCode)
	}
	body, _ := io.ReadAll(response.Body)
	if len(body) == 0 {
		t.Error("la página de aviso vino vacía")
	}
}

// TestPanelEmbebido comprueba el paquete tal como quedó compilado. Sin `make
// web-embed` el panel no está, y eso no es un error: el aviso es justamente
// para ese caso.
func TestPanelEmbebido(t *testing.T) {
	t.Parallel()
	h := Handler(testLogger())

	response := get(t, h, "/")
	defer response.Body.Close()

	switch response.StatusCode {
	case http.StatusOK:
		t.Log("este binario tiene el panel adentro")
	case http.StatusServiceUnavailable:
		t.Log("este binario se compiló sin el panel (falta make web-embed)")
	default:
		t.Fatalf("estado = %d: se esperaba 200 con panel o 503 sin él", response.StatusCode)
	}
}
