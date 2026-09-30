# ADR-007: El control plane vive en el servidor de un cliente, detrás de su nginx

- **Estado:** aceptada. La mudanza a un VPS propio está escrita en [ADR-014](ADR-014-control-plane-en-vps-propio.md), propuesta y postergada el 2026-09-30.
- **Fecha:** 2026-09-21

## Contexto

El diseño prevé el control plane en "un VPS central", dedicado. Al desplegar el Sprint 0, el
servidor disponible era `cristales-baez-prod`: la producción de un cliente, con nginx del host en
los puertos 80 y 443 sirviendo su sitio, y el dominio detrás del proxy de Cloudflare.

Se evaluaron dos opciones:

- **A. Un VPS propio para el control plane**, y en el del cliente solo el agent.
- **B. El control plane en el mismo servidor del cliente**, detrás de su nginx.

## Decisión

**B**, por decisión de Juan.

El control plane corre en contenedores con la base, publicado solo en `127.0.0.1:8090`. El nginx del
cliente le pasa el tráfico de `status.extendsystem.com` desde un sitio propio (un archivo aparte en
`sites-available`) sin tocar la configuración del sitio del cliente, con certificado de Let's
Encrypt y Cloudflare adelante con el proxy activado, igual que el resto del dominio.

## Consecuencias

**Positivas**

- Cero costo de infraestructura nueva y el sprint cerró sin esperar otro servidor.
- La IP del servidor sigue oculta detrás de Cloudflare.
- El sitio del cliente no se tocó: se verificó que respondiera 200 después de cada recarga de nginx,
  y la configuración de status-sv se quita sin afectarlo.

**Negativas, aceptadas a sabiendas**

- **Un monitor no puede avisar de la caída del servidor donde vive.** Si `cristales-baez-prod` se
  cae, se cae también lo que tendría que avisarlo. Hoy no hay alertas (son de la Fase 4), así que
  el costo es teórico, pero deja de serlo cuando existan.
- **Radio de impacto compartido.** El control plane va a tener poder para operar todos los
  servidores. Comprometerlo compromete el servidor del cliente, y un problema del cliente (disco
  lleno, una actualización del sistema) afecta al control plane.
- Los recursos se comparten. Hoy sobran (2 CPU, 7 GB libres), y el control plane usa poco.
- Cada cambio en nginx toca la producción de un cliente, con el cuidado que eso exige.

## Cuándo revisarla

Mover el control plane a un VPS propio, con el mismo runbook (perfil `caddy`), **antes** de
cualquiera de estas cosas:

- Que existan las alertas de la Fase 4.
- Que haya un segundo cliente con agent: sus datos no deberían vivir en el servidor de otro.
- Que el control plane tenga herramientas de escritura habilitadas (Fase 2 o 3).

## Alternativas consideradas

- **A. VPS propio:** la recomendada. Queda como destino, no descartada.
- **Caddy en puertos alternativos en el mismo servidor:** puertos no estándar para los agents y
  otro hueco en el firewall, sin ninguna ventaja sobre usar el nginx existente. Descartada.
