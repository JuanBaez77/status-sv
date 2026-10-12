# Más adelante

Ideas que **no** entran en el alcance actual (Fases 1 a 3). Se anotan acá para no perderlas y
para no dejarlas colarse en un sprint. Nada de esta lista se implementa sin antes moverlo al
roadmap de [DISENO.md](DISENO.md).

## Fuera del MVP por decisión explícita

- Multiusuario, organizaciones, roles finos y facturación (ver [adr/ADR-005-alcance-single-tenant.md](adr/ADR-005-alcance-single-tenant.md)).
- Agents para Windows o macOS.
- Orquestación tipo Kubernetes y autoscaling.
- Edición de archivos de configuración desde el panel, incluidas las plantillas de la unidad y del sitio, que el panel solo muestra (ver [adr/ADR-023-plantillas-sitio-y-certificado.md](adr/ADR-023-plantillas-sitio-y-certificado.md)).

## Planificado para la Fase 4

- Worker de smoke tests con Playwright.
- Alertas por correo o Telegram.
- Chequeos de seguridad del servidor: intentos fallidos de SSH, puertos abiertos, actualizaciones
  pendientes, expiración de certificados, estado de backups, contenedores corriendo como root.
- mTLS entre agent y control plane.
- OAuth 2.1 con registro dinámico de clientes, para conectar el MCP a claude.ai.
- Actualización del agent desde el panel, con verificación de checksum y firma.

## Ideas sin fecha

- Gestión de backups de los proyectos alojados y restore probado.
- Renovación y estado de certificados como vista propia del panel.
- Comparación entre entornos (qué versión corre en staging y cuál en producción).
- Plantillas de proyecto para acelerar el alta de un cliente nuevo.
- Notificaciones de deploy a clientes.
- Modo staging automático por cada PR.
- Aprobaciones por enlace firmado desde el celular, además del panel.
- WebAuthn o passkeys como segundo factor, además de TOTP (ver
  [adr/ADR-013-segundo-factor-totp.md](adr/ADR-013-segundo-factor-totp.md)).
- TimescaleDB, si el particionado nativo deja de alcanzar (ver [adr/ADR-001-go-y-postgresql.md](adr/ADR-001-go-y-postgresql.md)).
- Protobuf para los mensajes de métricas, si el volumen lo justifica (ver [adr/ADR-003-contratos-json-versionado.md](adr/ADR-003-contratos-json-versionado.md)).
- Historia del consumo por contenedor y por servicio (hoy el panel muestra solo el valor actual);
  serían tablas de métricas propias, particionadas, como las del servidor (decisión del Sprint 2, ver
  [adr/ADR-011-acceso-a-docker-y-journald.md](adr/ADR-011-acceso-a-docker-y-journald.md)).

## Deuda que deja el despliegue del Sprint 0

- **Pipeline de releases.** Hoy el control plane se construye en el propio VPS a partir de un
  `git archive`, y el binario del agent se compila en la máquina de quien despliega. Lo correcto es
  que el CI publique las imágenes en un registro (GHCR) y los binarios del agent firmados, con
  checksum. El diseño ya lo pide para la Fase 4 (actualizaciones firmadas del agent).
- **Revocar agents desde el panel.** Hoy se revoca con una sentencia SQL a mano (ver el runbook).
- **Backups de PostgreSQL** del control plane, con un restore probado. Está en los riesgos del diseño.
- **Mover el control plane a un VPS dedicado.** El 2026-10-02 salió del servidor de un cliente y
  pasó a uno propio, pero compartido con otros productos
  ([ADR-018](adr/ADR-018-control-plane-en-servidor-propio-compartido.md)). El VPS dedicado sigue
  escrito en [ADR-014](adr/ADR-014-control-plane-en-vps-propio.md) y en la sección 6 del runbook.
  ADR-018 pide llegar antes de las alertas, de los tokens del MCP con escritura o de querer
  desplegar con status-sv algún proyecto de ese servidor. Con el Docker común vuelve además
  `no-new-privileges`, que en el servidor de hoy (Docker de snap) hubo que quitar.
- **Un ensayo de la mudanza en CI.** El volcado, la restauración y el arranque sobre datos
  restaurados solo se probaron a mano, el día de la mudanza. Un job que lo haga con la base de la
  prueba de punta a punta lo cubriría.

## Lo que deja afuera el MCP de escritura del Sprint 8

- **Staging sin aprobación.** En el Sprint 8 todas las escrituras piden aprobación, porque no hay
  proyectos de staging. Cuando haya, se decide con otro ADR si staging puede andar sin ella
  ([ADR-026](adr/ADR-026-escrituras-por-mcp-y-aprobaciones.md)).
- **Aprobar con un enlace firmado** desde el celular. Descartado por ahora: el panel ya anda en el
  celular, y el enlace pediría el código igual.
- **Avisos del avance de un deploy por MCP** (SSE). Hoy el LLM sigue el pedido con `get_approval`
  y `get_deploy`.
- **Un respaldo automático de la base antes de un deploy que trae migraciones.** Hoy lo dice el
  runbook y lo hace una persona.

## Lo que deja afuera el MCP de lectura del Sprint 7

- **Alerta ante el uso de un token desde una IP nueva.** El diseño la pide; llega con las alertas
  de la Fase 4. Hoy el panel muestra la última IP de cada token (ver
  [adr/ADR-025-tokens-del-mcp-y-scopes.md](adr/ADR-025-tokens-del-mcp-y-scopes.md)).
- **IPs permitidas por token.**
- **Logs por MCP de cualquier unidad del servidor**, no solo de las de un proyecto declarado. El
  panel los puede pedir; el MCP no (ver [adr/ADR-024-mcp-remoto.md](adr/ADR-024-mcp-remoto.md)).
- **Sesiones y avisos del MCP** (Streamable HTTP con estado y SSE), por ejemplo para empujar el
  avance de un deploy. Hoy cada pedido se atiende solo y el avance se consulta.
- **Recursos y prompts del MCP.** Solo hay herramientas.
- **Métricas por contenedor o por servicio** por MCP: no hay historia guardada (ver "Ideas sin fecha").

## Lo que deja afuera el alta del Sprint 6

- **Plantillas editables desde el panel**, con cada versión aceptada en el servidor por su hash.
  Hoy se cambian por PR o como root en el servidor (ver
  [adr/ADR-023-plantillas-sitio-y-certificado.md](adr/ADR-023-plantillas-sitio-y-certificado.md)).
- **Caddy** como alternativa a nginx en el sitio generado.
- **Cargar la deploy key en GitHub sola**, y revocarla desde el panel. Necesita un token de GitHub
  en el control plane; hoy la carga y la saca una persona.
- **El registro DNS del dominio**, por la API de Cloudflare. Hoy lo crea una persona antes del alta.
- **Dar de baja un proyecto:** su usuario, su `workdir`, su unidad, su sitio y su certificado. Hoy
  lo hace una persona; el agent no borra.
- **Quitar el sitio cuando el primer deploy falla.** Hoy queda contestando 502 hasta el próximo
  intento.

## Lo que deja afuera el deploy systemd del Sprint 5

- **Rollback exacto del entorno virtual.** Hoy volver atrás es correr `deploy.build` de nuevo
  sobre el commit anterior: depende de versiones fijadas y de la red o la caché de `pip`. Una
  copia del entorno antes de instalar lo haría exacto y sin red (ver
  [adr/ADR-019-deploy-de-proyectos-systemd.md](adr/ADR-019-deploy-de-proyectos-systemd.md)).
- **`reload` en vez de `restart`.** gunicorn sabe recargar sin cortar los pedidos en curso; hoy el
  deploy reinicia.
- **Servicios que corren como root.** No se pueden declarar: `run_as` no acepta `root`.
- **La regla de polkit por unidad**, como alternativa al ayudante root, cuando ningún servidor
  quede en Ubuntu 22.04.

## Lo que deja afuera el deploy del Sprint 4

- **El deploy roto a propósito en un servidor real.** Era parte del cierre del Sprint 4 y se
  pospuso: la vuelta atrás sola se prueba en cada PR del CI, pero no se ensayó en producción. Con
  un repo de prueba declarado como `staging` en `sv-gcg`: un deploy bueno, un commit que rompa el
  healthcheck, y mirar que vuelva solo y que el sitio siga respondiendo. **Antes** de prender el
  deploy en un proyecto que importe.
- **Un interruptor de deploy por proyecto.** Hoy se prende por servidor, y todos los proyectos
  declarados figuran como desplegables en el panel aunque no tengan llave ni permisos. Fallan en
  el chequeo previo sin tocar nada, pero el panel no lo anticipa.

- **Registros privados de imágenes.** El agent no tiene credenciales de ningún registro: los
  proyectos compilan en el servidor y bajan imágenes públicas (ver
  [adr/ADR-015-motor-de-deploy-docker.md](adr/ADR-015-motor-de-deploy-docker.md)).
- **Verificar el CI del commit en GitHub** antes de desplegarlo. Hoy alcanza con que el commit
  esté en la rama protegida; consultar la API de GitHub necesita un token y entra con el MCP.
- **Sacar los servicios huérfanos** (`up --remove-orphans`) cuando el commit nuevo ya no declara
  un servicio. Hoy queda andando y lo saca una persona.
- **Vista de auditoría en el panel.** `audit_log` existe desde el Sprint 4; la vista es del Sprint 5.
- **Limpieza de imágenes viejas** de los proyectos desplegados. El agent solo quita sus etiquetas
  de rollback; el `prune` lo decide quien administra el servidor.
