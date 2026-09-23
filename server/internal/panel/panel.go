// Package panel sirve el panel web desde el mismo binario del control plane.
//
// El panel compilado se copia a dist/ antes de construir el binario (`make
// web-embed`, o la etapa de Node de deploy/Dockerfile) y queda adentro con
// go:embed: se despliega un solo archivo, sin Node ni archivos sueltos en el
// servidor, y no hay forma de que el front y la API queden en versiones
// distintas.
//
// En el repositorio dist/ solo tiene una página de aviso: si el binario se
// compiló sin pasar por ese paso, el panel lo dice en vez de devolver un 404
// sin explicación.
package panel

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed all:dist
var embedded embed.FS

const (
	// assetsCache: los archivos de assets/ llevan el hash del contenido en el
	// nombre, así que si cambian, cambia la dirección. Se pueden guardar para
	// siempre.
	assetsCache = "public, max-age=31536000, immutable"
	// indexCache: el index.html es el que apunta a esos nombres. Se revalida en
	// cada visita para que un despliegue nuevo se vea sin vaciar la caché.
	indexCache = "no-cache"
)

// csp es la política de contenido del panel. El front no tiene nada en línea:
// ni scripts ni estilos sueltos en el HTML, todo sale de archivos propios. Con
// eso alcanza para que el navegador rechace cualquier cosa inyectada.
//
// connect-src 'self' incluye el WebSocket del mismo origen (CSP nivel 3).
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' data:; font-src 'self'; connect-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// contentTypes fija el tipo de cada archivo del panel en vez de preguntarle al
// sistema. En Windows el tipo de un .js sale del registro, que muchas veces
// dice text/plain: con nosniff el navegador se niega a ejecutarlo y el panel no
// arranca. Es lo único que se sirve desde acá, así que la lista es corta.
var contentTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".html":  "text/html; charset=utf-8",
	".ico":   "image/x-icon",
	".js":    "text/javascript; charset=utf-8",
	".json":  "application/json",
	".png":   "image/png",
	".svg":   "image/svg+xml",
	".txt":   "text/plain; charset=utf-8",
	".webp":  "image/webp",
	".woff2": "font/woff2",
}

// Handler devuelve el panel embebido en el binario.
func Handler(log *slog.Logger) http.Handler {
	files, err := fs.Sub(embedded, "dist")
	if err != nil {
		// dist/ es parte del paquete: si esto falla es un error de programación.
		panic("no se pudo abrir el panel embebido: " + err.Error())
	}
	return HandlerFS(files, log)
}

// HandlerFS es Handler sobre un sistema de archivos cualquiera. Existe para las
// pruebas, que no pueden contar con un panel compilado.
func HandlerFS(files fs.FS, log *slog.Logger) http.Handler {
	index, err := fs.ReadFile(files, "index.html")
	if err != nil {
		log.Warn("este binario se compiló sin el panel",
			"consecuencia", "la raíz devuelve una página de aviso",
			"solución", "make web-embed y volver a compilar")
		return &missing{files: files}
	}

	sum := sha256.Sum256(index)
	return &handler{
		files: files,
		index: index,
		// El contenido embebido no tiene fecha, así que la validación es por
		// etiqueta: mientras el index.html no cambie, el navegador reusa el suyo.
		etag: `"` + hex.EncodeToString(sum[:16]) + `"`,
	}
}

type handler struct {
	files fs.FS
	index []byte
	etag  string
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !readOnly(w, r) {
		return
	}
	secureHeaders(w)

	name := cleanPath(r.URL.Path)
	if name == "" {
		h.serveIndex(w, r)
		return
	}
	// En dist/ el único archivo oculto es el .gitignore que marca el lugar, y no
	// tiene por qué salir por la red.
	if hidden(name) {
		http.NotFound(w, r)
		return
	}

	if info, err := fs.Stat(h.files, name); err == nil && !info.IsDir() {
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", assetsCache)
		} else {
			w.Header().Set("Cache-Control", indexCache)
		}
		if contentType, ok := contentTypes[strings.ToLower(path.Ext(name))]; ok {
			w.Header().Set("Content-Type", contentType)
		}
		http.ServeFileFS(w, r, h.files, name)
		return
	}

	// Un archivo que no está es un 404. Devolver el index.html en su lugar
	// convertiría un despliegue a medias en una página en blanco: el navegador
	// pediría un .js y recibiría HTML.
	if path.Ext(name) != "" {
		http.NotFound(w, r)
		return
	}

	// Lo demás son rutas del panel (/servers/xxx): las resuelve el front.
	h.serveIndex(w, r)
}

func (h *handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", indexCache)
	w.Header().Set("ETag", h.etag)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(h.index))
}

// missing es el panel cuando el binario se compiló sin él.
type missing struct {
	files fs.FS
}

func (m *missing) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !readOnly(w, r) {
		return
	}
	secureHeaders(w)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 503 y no 404: el panel existe, este binario no lo tiene.
	w.WriteHeader(http.StatusServiceUnavailable)

	if r.Method == http.MethodHead {
		return
	}
	page, err := fs.ReadFile(m.files, "sin-panel.html")
	if err != nil {
		_, _ = w.Write([]byte("<!doctype html><p>Este binario se compiló sin el panel.\n"))
		return
	}
	_, _ = w.Write(page)
}

// readOnly rechaza lo que no sea una lectura: el panel son archivos, no hay
// nada que escribir acá.
func readOnly(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	http.Error(w, "método no permitido", http.StatusMethodNotAllowed)
	return false
}

func secureHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	// frame-ancestors ya lo cubre en los navegadores al día; esto es para los
	// que no leen CSP.
	h.Set("X-Frame-Options", "DENY")
}

// cleanPath pasa la ruta del pedido a un nombre dentro de dist. Devuelve vacío
// para la raíz.
func cleanPath(p string) string {
	name := strings.TrimPrefix(path.Clean("/"+p), "/")
	if name == "." {
		return ""
	}
	return name
}

func hidden(name string) bool {
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}
