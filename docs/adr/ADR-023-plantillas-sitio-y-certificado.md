# ADR-023: Las plantillas viven en el repo, y el sitio y el certificado los hace el comando de aceptación

- **Estado:** aceptada. Cambia la regla 7 del proyecto, con el OK de Juan del 2026-10-08.
- **Fecha:** 2026-10-08

## Contexto

Es la tercera decisión que [ADR-021](ADR-021-primer-deploy-y-reparto-del-deploy-desde-cero.md)
dejó abierta: si las plantillas de la unidad de systemd y del proxy viven en el control plane o en
cada servidor.

El diseño describe que, durante el deploy desde cero, el agent genera la unidad y el bloque del
proxy desde plantillas, prueba la configuración, recarga, pide el certificado y, si algo falla,
deshace lo que creó. Y la regla 7 del proyecto dice: "Solo una persona edita las plantillas, desde
el panel, con auditoría".

Tres cosas chocan con eso:

- **Una plantilla de unidad es código que corre como root.** Decide el usuario (`User=`) y el
  programa (`ExecStart=`). Una de nginx también pesa: el proceso maestro es root, y un `alias` mal
  puesto sirve cualquier archivo del servidor. Si las plantillas se editan en el panel, viajan
  desde el control plane, y quien lo tome las cambia: es el mismo problema que declarar un
  proyecto ([ADR-022](ADR-022-alta-de-proyectos-propuesta-y-aceptacion.md)).
- **El ayudante root no tiene red** ([ADR-019](ADR-019-deploy-de-proyectos-systemd.md)), y certbot
  la necesita. Que el ayudante toque nginx y pida certificados le daría red y bastante más poder,
  al alcance de un agent comprometido.
- **Los servidores tienen nginx, no Caddy.** El relevamiento de `sv-gcg` del 2026-10-08 muestra
  nginx 1.18.0 con los sitios en `/etc/nginx/sites-enabled` y certbot 1.21.0 con cuatro
  certificados, pedidos con el plugin de nginx (ningún sitio tiene `acme-challenge`). `srv936263`
  y `cristales-baez-prod` también usan nginx del host y certbot (runbook, sección 1).

La decisión la tomó Juan el 2026-10-08, aceptando las recomendaciones del plan, incluido el cambio
de la regla 7.

## Decisión

**1. Hay dos plantillas, y su texto vive en el repo.**

Una unidad de servicio de systemd y un sitio de nginx, embebidas en el binario del agent
(`internal/templates`). Se cambian por PR, con revisión y CI, como cualquier código.

- Cada servidor puede reemplazarlas con archivos propios en `/etc/status-sv/templates/`
  (`service.tmpl` y `nginx-site.tmpl`). Se usan solo si son de root y ni el grupo ni otros pueden
  escribirlos, igual que su directorio: la misma regla con la que el ayudante lee los
  `project.yaml`.
- El panel muestra la vista previa de lo que se va a generar para una propuesta, con la plantilla
  de fábrica. Lo que se escribe de verdad lo muestra el comando de aceptación, que es el que sabe
  si el servidor tiene la suya.

**2. La regla 7 queda así:**

> Las plantillas de systemd y del proxy reciben parámetros validados, nunca texto libre del LLM.
> Solo una persona las cambia: por PR en el repo, o como root en el servidor. El panel las muestra
> con vista previa.

**3. Los parámetros son pocos y todos validados.**

| Parámetro | Regla |
| --- | --- |
| Nombre del proyecto | el de [ADR-012](ADR-012-proyectos-declarados-en-el-servidor.md) |
| Usuario, `workdir`, archivo de entorno | fijos en una propuesta ([ADR-022](ADR-022-alta-de-proyectos-propuesta-y-aceptacion.md)) |
| `service.command` | lista de argumentos, como un paso declarado: el primero es una ruta absoluta o relativa al `workdir` sin salir de él (en la unidad queda absoluta); sin caracteres de control; hasta 32 argumentos |
| `site.domain` | un nombre de host en minúsculas, con al menos un punto, sin comodines, hasta 253 caracteres |
| `site.port` | de 1024 a 65535; el sitio siempre apunta a `127.0.0.1` |

En la unidad, cada argumento va entre comillas dobles, con `\` y `"` escapados, `%` como `%%` y
`$` como `$$`, para que systemd no los interprete. Se prueba con archivos golden y con
`systemd-analyze verify` en el CI.

**4. La unidad generada.**

`/etc/systemd/system/<nombre>.service`: el usuario del proyecto, su `workdir`, su archivo de
entorno y su comando, con `Restart=on-failure`, `NoNewPrivileges`, `/tmp` propio y el sistema en
solo lectura salvo lo que permitan los permisos del usuario. La escribe el comando de aceptación,
recarga systemd y **no la arranca ni la habilita**: la arranca el primer deploy y, si queda bien,
la habilita (`enable_units`, ADR-022).

**5. El sitio y el certificado.**

Solo nginx. El archivo es `/etc/nginx/sites-available/statussv-<nombre>.conf`, con su enlace en
`sites-enabled`; el prefijo dice quién lo generó. Se arma en dos tiempos:

1. Un sitio solo HTTP que sirve el desafío de Let's Encrypt desde `/var/www/statussv-acme`.
   `nginx -t` y recarga.
2. `certbot certonly --webroot`, primero con `--dry-run` (Let's Encrypt limita los certificados
   repetidos y los intentos fallidos) y después de verdad, con un `--deploy-hook` que recarga
   nginx en cada renovación.
3. El sitio completo: el 80 sirve el desafío y redirige al 443, y el 443 pasa al puerto del
   proyecto con los encabezados de siempre (`Host`, `X-Forwarded-For`, `X-Forwarded-Proto`,
   WebSocket). `nginx -t` y recarga.

- **Nunca queda nginx con una configuración que no pasa `nginx -t`.** Si algo falla, se borra lo
  que se escribió en esa corrida, se prueba de nuevo y se recarga solo si pasa. Los demás sitios
  del servidor no se tocan.
- Si otro sitio habilitado ya atiende ese dominio, o el dominio todavía no resuelve, no se escribe
  nada y se explica. Se reintenta con `sudo statussv-agent project site <nombre>`, que es lo mismo
  que hace `project accept` para el sitio.
- Los certificados que ya existen en el servidor, pedidos con el plugin de nginx, no se tocan.

**6. Todo esto lo hace el comando de aceptación, no el primer deploy.**

Lo corre una persona, como root, y ve cada paso. El agent y el ayudante no tocan nunca nginx ni
certbot, y el ayudante sigue sin red. Por eso un primer deploy que falla no quita el sitio: queda
contestando 502 hasta el próximo intento, que el diseño resolvía quitándolo.

**7. Es la línea de corte del sprint.**

Si el sprint se estira, el sitio y el certificado los sigue haciendo una persona a mano, como hoy,
y la unidad generada queda.

## Consecuencias

**Positivas**

- Nada que llegue desde el control plane puede cambiar lo que corre como root: ni la plantilla ni
  sus parámetros fijos.
- La historia de cada plantilla es la de git, con revisión y CI, en vez de una tabla de
  auditoría.
- El ayudante no gana red ni operaciones sobre nginx.
- `nginx -t` antes de cada recarga protege los demás sitios del servidor, que en `sv-gcg` son de
  un cliente.

**Negativas**

- **Las plantillas no se editan desde el panel**, como pedía la regla 7. Ajustar una para un
  servidor es un archivo de root por SSH.
- Solo nginx; Caddy queda para más adelante.
- certbot necesita una cuenta de Let's Encrypt en el servidor. `sv-gcg` ya la tiene; en uno sin
  cuenta, el comando pide el correo.
- Un primer deploy fallido deja el sitio dando 502.

## Alternativas consideradas

- **Plantillas editables en el panel, sin más:** un control plane comprometido cambia la unidad y
  tiene root. Descartado.
- **Editables en el panel, con cada versión aceptada en el servidor** por su hash: respeta la
  regla 7 al pie de la letra, pero suma unos dos PR y otro paso en el servidor para algo que
  cambia muy de vez en cuando. Queda en "más adelante".
- **Que el ayudante escriba la unidad y el sitio y pida el certificado durante el primer deploy**,
  como decía el diseño: le da red y poder sobre nginx a un proceso root al que llega el agent.
  Descartado.
- **El plugin `--nginx` de certbot**, como los certificados que ya hay en `sv-gcg`: reescribe el
  archivo del sitio, que deja de ser lo que generó la plantilla. Descartado.
- **Caddy:** ningún servidor con proyectos lo usa. Descartado por ahora.
