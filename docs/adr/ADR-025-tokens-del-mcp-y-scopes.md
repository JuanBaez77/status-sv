# ADR-025: Tokens del MCP: se crean en el panel con TOTP, vencen y llevan scopes por entorno y proyecto

- **Estado:** aceptada
- **Fecha:** 2026-10-09

## Contexto

El MCP ([ADR-024](ADR-024-mcp-remoto.md)) necesita una credencial que Claude Code pueda mandar en
cada pedido. El diseño la describe en una línea: tokens de API con scopes por proyecto y entorno,
vencimiento y revocación, guardados como hash (regla 5). Falta decidir quién los crea, qué forma
tienen los scopes, y qué ve un token que no alcanza a todo.

Un token del MCP vive en la máquina de quien usa Claude Code, en una variable de entorno o en su
configuración, y no tiene segundo factor: es el eslabón más débil que va a tener status-sv. En el
Sprint 7 solo lee, pero los logs enmascarados igual pueden mostrar datos de los clientes, y en el
Sprint 8 el mismo esquema va a habilitar deploys.

La decisión la tomó Juan el 2026-10-09, aceptando las recomendaciones del plan del Sprint 7.

## Decisión

**1. Se crean en el panel, con un código TOTP nuevo, y se muestran una sola vez.**

- Crear un token pide la sesión completa y, en el mismo pedido, un código TOTP nuevo (no uno de
  recuperación), verificado como el de un deploy de producción
  ([ADR-017](ADR-017-deploy-desde-el-panel-y-auditoria.md)): una cookie robada no alcanza para
  sacar una credencial nueva.
- El token se muestra una vez, con el comando para conectarlo a Claude Code. Después el panel
  muestra solo su prefijo.
- Revocar no pide código: frenar un token tiene que ser fácil.
- Para una emergencia sin panel, `server token-revoke --id <id>` o `--all`, por SSH en el servidor
  del control plane.
- Crear y revocar quedan en `audit_log` (`token.create`, `token.revoke`), con quién y, en el
  detalle, el prefijo, los scopes y el vencimiento.

**2. Forma y guardado.**

- `ssvmcp_` y 43 caracteres base64url (32 bytes al azar). El prefijo fijo deja que un escáner de
  secretos lo reconozca si aparece en un repo o en un log.
- En la base, solo su SHA-256, como las sesiones del panel y las credenciales de los agents
  (ADR-006, [ADR-010](ADR-010-usuarios-y-sesiones-del-panel.md)): un token al azar no necesita un
  hash lento, y así se busca por hash. Se guardan además los primeros 12 caracteres, para
  reconocerlo en el panel y en la auditoría.
- Vence siempre: 30 días si no se elige otra cosa, y 90 como mucho. La base lo exige con un
  `CHECK`. Uno vencido o revocado no se borra, para que la auditoría siga teniendo a quién nombrar.
- Se guarda cuándo y desde qué IP se usó por última vez, actualizado como mucho una vez por minuto.

**3. Scopes: un permiso por entorno, de una lista cerrada.**

| Scope | Sprint | Habilita |
| --- | --- | --- |
| `read:staging`, `read:production` | 7 | Las herramientas de lectura sobre los proyectos de ese entorno y los deploys de esos proyectos. |
| `logs:staging`, `logs:production` | 7 | `get_logs` sobre las unidades de los proyectos de ese entorno. |
| `deploy:…`, `rollback:…`, `restart:…` | 8 | Las escrituras, con su propio ADR. |

- No hay comodines ni scopes armados a mano: el panel los ofrece como casillas, y la base rechaza
  cualquier valor fuera de la lista.
- `logs` va aparte de `read` porque los logs son el contenido más sensible que el MCP puede
  mostrar, y el camino más corto para meterle texto al LLM: se puede dar un token que ve el estado
  pero no los logs.
- Un token tiene al menos un scope.

**4. Una lista opcional de proyectos.**

- Sin lista, el token alcanza a todos los proyectos de los entornos de sus scopes.
- Con lista, solo a esos. Se guardan sus IDs: si un proyecto deja de estar declarado y vuelve, tiene
  otro ID y el token deja de verlo. Falla cerrado.

**5. Qué servidores ve un token.**

Los servidores no tienen entorno. Un token con algún `read` y sin lista de proyectos ve todos los
servidores y sus métricas. Uno con lista ve solo los servidores donde está alguno de sus proyectos.
Un token con solo `logs` no ve servidores.

**6. Cómo se verifica.**

En cada pedido: el hash existe, no está revocado, no venció, y el scope de la herramienta alcanza al
objetivo. Lo que falle contesta 401 si es el token, o un error de la herramienta si es el scope
([ADR-024](ADR-024-mcp-remoto.md)), y queda en la auditoría.

## Consecuencias

**Positivas**

- Un token filtrado sirve como mucho 90 días, solo para lo que se le dio, y se corta desde el panel
  o por SSH en segundos.
- La base filtrada no deja usar ningún token.
- El Sprint 8 suma escrituras sin cambiar la forma: tres pares de scopes más.

**Negativas**

- Sacar un token pide tener el teléfono a mano, y hay que rotarlo al menos cada 90 días.
- Sin IPs permitidas ni alerta por IP nueva: el diseño pide la alerta, que llega con las demás
  alertas en la Fase 4. Mientras tanto, el panel muestra la última IP de cada token.
- La lista de proyectos se pierde si un proyecto se vuelve a declarar: hay que crear el token de
  nuevo.

## Alternativas consideradas

- **Crearlos solo por SSH**, con un subcomando en el servidor del control plane: más difícil de
  robar, pero rotarlos sería tan incómodo que nadie lo haría. Queda el subcomando para revocar.
- **Un solo scope `read`, con los logs adentro**: más simple, pero obliga a dar los logs para ver un
  estado. Descartado.
- **Scopes por proyecto con nombre** (`read:cristales-baez`): mezcla entorno y proyecto en un mismo
  texto y los nombres se repiten entre servidores. Se prefieren dos ejes: entorno en el scope y
  proyectos por ID.
- **argon2id para el hash**: protege contraseñas elegidas por personas, no 256 bits al azar, y
  impide buscar por hash. Descartado.
