package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Lo que el motor de deploy necesita de la API (ADR-015): los contenedores de
// un proyecto de Compose con la imagen exacta de cada uno, y poner o sacar
// etiquetas de imágenes para poder volver atrás sin recompilar. Nada de esto
// crea ni arranca contenedores: eso lo hace docker compose.

// labelComposeOneOff marca los contenedores de docker compose run, que no son
// parte de lo que corre.
const labelComposeOneOff = "com.docker.compose.oneoff"

// ProjectContainers devuelve los contenedores de un proyecto de Compose,
// corriendo o no, sin los de una sola vez (compose run).
func (c *Client) ProjectContainers(ctx context.Context, project string) ([]Container, error) {
	filters, err := json.Marshal(map[string][]string{"label": {labelComposeProject + "=" + project}})
	if err != nil {
		return nil, err
	}
	var containers []Container
	if err := c.getJSON(ctx, "/containers/json", url.Values{"all": {"1"}, "filters": {string(filters)}}, &containers); err != nil {
		return nil, fmt.Errorf("listar los contenedores del proyecto: %w", err)
	}
	out := containers[:0]
	for _, ctr := range containers {
		// La API ya filtra, pero el filtro es por prefijo en versiones viejas:
		// se confirma igual.
		if ctr.Labels[labelComposeProject] != project || strings.EqualFold(ctr.Labels[labelComposeOneOff], "true") {
			continue
		}
		out = append(out, ctr)
	}
	return out, nil
}

// ServiceOf devuelve el servicio de Compose de un contenedor.
func ServiceOf(ctr Container) string {
	return ctr.Labels[labelComposeService]
}

// ContainerImage es la imagen exacta de un contenedor: el ID que usa de
// verdad y la referencia con la que se creó (lo que dice el compose).
type ContainerImage struct {
	ID  string
	Ref string
}

// ImageOf devuelve la imagen de un contenedor.
func (c *Client) ImageOf(ctx context.Context, id string) (ContainerImage, error) {
	var raw struct {
		Image  string `json:"Image"`
		Config struct {
			Image string `json:"Image"`
		} `json:"Config"`
	}
	if err := c.getJSON(ctx, "/containers/"+url.PathEscape(id)+"/json", nil, &raw); err != nil {
		return ContainerImage{}, fmt.Errorf("inspeccionar el contenedor %s: %w", shortID(id), err)
	}
	if !strings.HasPrefix(raw.Image, "sha256:") {
		return ContainerImage{}, fmt.Errorf("el contenedor %s no dice qué imagen usa", shortID(id))
	}
	return ContainerImage{ID: raw.Image, Ref: raw.Config.Image}, nil
}

// ImageExists dice si una imagen (por ID o referencia) está en el servidor.
func (c *Client) ImageExists(ctx context.Context, ref string) (bool, error) {
	var ignored struct {
		ID string `json:"Id"`
	}
	err := c.getJSON(ctx, "/images/"+escapeRef(ref)+"/json", nil, &ignored)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNotFound):
		return false, nil
	default:
		return false, fmt.Errorf("buscar la imagen: %w", err)
	}
}

// Tag le pone a la imagen id la referencia repo:tag. Si la referencia ya
// apuntaba a otra imagen, pasa a apuntar a esta (así se vuelve atrás).
func (c *Client) Tag(ctx context.Context, id, repo, tag string) error {
	query := url.Values{"repo": {repo}, "tag": {tag}}
	return c.do(ctx, http.MethodPost, "/images/"+escapeRef(id)+"/tag", query)
}

// Untag saca una referencia. Si era lo único que retenía a la imagen y ningún
// contenedor la usa, Docker la borra; si no, solo desaparece la etiqueta.
func (c *Client) Untag(ctx context.Context, ref string) error {
	err := c.do(ctx, http.MethodDelete, "/images/"+escapeRef(ref), url.Values{"noprune": {"1"}})
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// ImagesByReference devuelve las referencias de las imágenes que cumplen el
// patrón de Docker (statussv-rollback/web-*).
func (c *Client) ImagesByReference(ctx context.Context, pattern string) ([]string, error) {
	filters, err := json.Marshal(map[string][]string{"reference": {pattern}})
	if err != nil {
		return nil, err
	}
	var images []struct {
		RepoTags []string `json:"RepoTags"`
	}
	if err := c.getJSON(ctx, "/images/json", url.Values{"filters": {string(filters)}}, &images); err != nil {
		return nil, fmt.Errorf("listar imágenes: %w", err)
	}
	var refs []string
	for _, img := range images {
		refs = append(refs, img.RepoTags...)
	}
	return refs, nil
}

// do hace un pedido que no devuelve nada útil más que el estado.
func (c *Client) do(ctx context.Context, method, path string, query url.Values) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return fmt.Errorf("armar el pedido: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := checkStatus(resp); err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	return nil
}

// escapeRef escapa una referencia de imagen para la ruta. Las barras de
// registro/dueño/nombre viajan tal cual, como las espera Docker.
func escapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// SplitRef separa una referencia en repositorio y etiqueta
// (registro:5000/dueño/app:1.2 → registro:5000/dueño/app y 1.2). Sin etiqueta
// es latest. Una referencia por digest (app@sha256:...) no se puede etiquetar:
// devuelve ok=false.
func SplitRef(ref string) (repo, tag string, ok bool) {
	if ref == "" || strings.Contains(ref, "@") {
		return "", "", false
	}
	slash := strings.LastIndex(ref, "/")
	if colon := strings.LastIndex(ref, ":"); colon > slash {
		if ref[:colon] == "" || ref[colon+1:] == "" {
			return "", "", false
		}
		return ref[:colon], ref[colon+1:], true
	}
	return ref, "latest", true
}
