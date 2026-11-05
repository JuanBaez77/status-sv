# Runbook: despliegue del control plane y del agent

Cómo poner status-sv en un VPS desde cero, cómo actualizarlo y cómo volver atrás. Todos los
comandos están probados en un ensayo local con el mismo compose de producción.

En los ejemplos, `<IP>` es la dirección del VPS y `<DOMINIO>` el dominio del control plane.

## 0. Antes de empezar (una sola vez)

**Acceso por clave, no por contraseña.** El control plane va a operar producción: el acceso al
servidor que lo aloja tiene que ser con clave SSH, y después de verificarla conviene apagar el
ingreso con contraseña.

```bash
# En tu máquina: una clave dedicada, para poder revocarla sin tocar otras.
ssh-keygen -t ed25519 -f ~/.ssh/statussv_deploy -C "status-sv deploy"
ssh-copy-id -i ~/.ssh/statussv_deploy.pub root@<IP>

# Verificar que entra sin pedir contraseña:
ssh -i ~/.ssh/statussv_deploy root@<IP> 'echo ok'
```

Recién cuando eso funcione, en el servidor, en `/etc/ssh/sshd_config`:

```text
PermitRootLogin prohibit-password
PasswordAuthentication no
```

y `systemctl reload ssh`. **Con una segunda sesión abierta**, por si algo sale mal.

**DNS.** Un registro `A` de `<DOMINIO>` apuntando a `<IP>`. Tiene que estar propagado antes de
levantar Caddy, porque el certificado se pide al arrancar:

```bash
dig +short <DOMINIO>
```

Sin proxy, tiene que devolver `<IP>`. Con el proxy de Cloudflare activado devuelve IPs de
Cloudflare, y es lo esperado: lo que importa es que el registro en Cloudflare apunte a `<IP>`.

**Firewall.** Abiertos solo 22, 80 y 443. Si el VPS usa `ufw`, el orden importa: habilitarlo sin
permitir el 22 corta la sesión SSH.

```bash
ufw allow 22/tcp && ufw allow 80/tcp && ufw allow 443/tcp && ufw allow 443/udp
ufw enable
```

> Docker publica sus puertos por fuera de `ufw`. Por eso el compose de producción publica
> únicamente los de Caddy: la base y el control plane no tienen puertos en el host.

**Docker.** Docker Engine y el plugin de Compose, desde el repositorio oficial de Docker para la
distribución del VPS.

## 1. Control plane

Hay dos formas de exponerlo, y el resto del despliegue es igual para las dos:

- **Servidor dedicado:** Caddy toma los puertos 80 y 443 y pide el certificado solo.
- **Servidor que ya tiene nginx** (el caso de [ADR-018](adr/ADR-018-control-plane-en-servidor-propio-compartido.md)): el control plane queda en
  `127.0.0.1` y nginx le pasa el tráfico desde un sitio propio, sin tocar los demás.

### Copiar el código

Se despliega un commit concreto, no la carpeta de trabajo: así lo que corre es exactamente lo
que pasó por el CI.

```bash
# En tu máquina, desde el repositorio, con main actualizado:
git archive --format=tar.gz -o status-sv.tar.gz HEAD
echo "commit: $(git rev-parse --short HEAD)"
scp -i ~/.ssh/statussv_deploy status-sv.tar.gz root@<IP>:/tmp/
```

```bash
# En el servidor:
mkdir -p /opt/status-sv
tar -xzf /tmp/status-sv.tar.gz -C /opt/status-sv
rm /tmp/status-sv.tar.gz
cd /opt/status-sv
```

### Configuración y secretos

```bash
cp deploy/.env.prod.example deploy/.env
chmod 600 deploy/.env
```

En `deploy/.env`, completar `STATUSSV_DOMAIN` y `STATUSSV_COMMIT` (el commit de arriba), y si va
detrás de nginx, elegir un `STATUSSV_LOCAL_PORT` libre (`ss -tln`). La contraseña de la base se
genera **en el servidor**:

```bash
sed -i "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=$(openssl rand -hex 32)|" deploy/.env
```

La contraseña no sale nunca del servidor ni hace falta saberla: la usan solo los contenedores.

La **clave que cifra los secretos TOTP** del panel ([ADR-013](adr/ADR-013-segundo-factor-totp.md))
también se genera en el servidor, una sola vez, en un archivo aparte de la base. El dueño es el
usuario del contenedor (10001), que es quien la lee:

```bash
install -d -m 0700 deploy/secrets
openssl rand -base64 32 > deploy/secrets/totp.key
chown 10001:10001 deploy/secrets/totp.key
chmod 0400 deploy/secrets/totp.key
```

Sin este archivo el control plane no arranca. **Entra en los backups**, separado del volcado de la
base: si se pierde, los secretos guardados no se pueden descifrar y cada usuario tiene que hacer
`user-totp-reset` (ver más abajo) y dar de alta el segundo factor de nuevo. `*.key` está en
`.gitignore`: nunca se commitea.

### Levantar

```bash
cd /opt/status-sv/deploy
docker compose -f docker-compose.prod.yml --env-file .env build
docker compose -f docker-compose.prod.yml --env-file .env up -d postgres
docker compose -f docker-compose.prod.yml --env-file .env run --rm server migrate up
```

Las migraciones van en un paso aparte, antes de levantar el control plane: nunca corren solas.

**Servidor dedicado** — con Caddy:

```bash
docker compose -f docker-compose.prod.yml --env-file .env --profile caddy up -d
```

Si el certificado no sale, `docker compose ... logs caddy`: casi siempre es el DNS todavía no
propagado o el puerto 80 cerrado.

**Con Cloudflare adelante** ([ADR-014](adr/ADR-014-control-plane-en-vps-propio.md)): el primer
certificado se pide con el registro en *DNS only* (nube gris), porque a través del proxy Let's
Encrypt no llega a Caddy. Cuando `logs caddy` dice `certificate obtained successfully`, se prende
el proxy (nube naranja) con el modo SSL en **Full (strict)**. Las renovaciones pasan solas a
través del proxy. Caddy toma la IP real de `CF-Connecting-IP` solo si la conexión viene de un rango
de Cloudflare (están en `deploy/Caddyfile`), y comprime el panel.

**Detrás de nginx** — sin Caddy:

```bash
docker compose -f docker-compose.prod.yml --env-file .env up -d
curl -s http://127.0.0.1:8090/readyz    # {"status":"ready"}, con el puerto elegido
```

### Si el Docker del servidor es el de snap

`snap list docker` lo dice, y `docker info --format '{{.DockerRootDir}}'` da
`/var/snap/docker/common/var-lib-docker`. Es el caso de `srv936263`, donde vive el control plane
([ADR-018](adr/ADR-018-control-plane-en-servidor-propio-compartido.md)). Cambian dos cosas, y las
dos se prueban **antes** de levantar nada.

**El directorio no puede ser `/opt/status-sv`.** El daemon de snap solo ve algunos directorios del
servidor, y el control plane monta un archivo (la clave de TOTP). Sirven `/root` y
`/var/snap/docker/common`; no sirven `/opt`, `/var/www` ni `/srv`. En `srv936263` el control plane
está en **`/root/status-sv`**, y todo lo que este runbook dice de `/opt/status-sv` va ahí. La
prueba, con cualquier imagen que ya esté en el servidor:

```bash
d=/root/statussv-prueba; mkdir -p "$d" && echo contenido > "$d/archivo" \
  && chown 10001:10001 "$d/archivo" && chmod 0400 "$d/archivo"
docker run --rm --user 10001:10001 --entrypoint cat -v "$d/archivo:/x:ro" <IMAGEN> /x   # contenido
rm -rf "$d"
```

**El contenedor no arranca con `no-new-privileges`**: falla con `exec /usr/local/bin/server:
operation not permitted`. El daemon corre con su propio perfil de AppArmor y, con esa opción, el
kernel no deja pasar al perfil del contenedor. El archivo `deploy/docker-compose.snap.yml` quita
solo esa opción (quedan el usuario sin privilegios, ninguna capacidad y la raíz de solo lectura),
y va **en todos los comandos de compose de ese servidor**:

```bash
cd /root/status-sv/deploy
docker compose -f docker-compose.prod.yml -f docker-compose.snap.yml --env-file .env up -d
docker inspect status-sv-server-1 \
  --format '{{.HostConfig.SecurityOpt}} {{.HostConfig.CapDrop}} {{.HostConfig.ReadonlyRootfs}}'   # [] [ALL] true
```

Sin el segundo `-f`, el control plane no arranca: falla a la vista, no queda a medias.

### Detrás de nginx: el sitio y el certificado

La configuración sale de plantillas y la genera `deploy/nginx/render.sh`, que valida el dominio y
el destino antes de escribir nada. Es un archivo propio en `sites-available`: los sitios que ya
existen no se tocan.

**Primero, una configuración provisoria** que solo atiende el desafío de Let's Encrypt, porque el
bloque HTTPS no se puede declarar hasta que exista el certificado:

```bash
cd /opt/status-sv
mkdir -p /var/www/certbot
install -m 0644 deploy/nginx/statussv-cloudflare-realip.conf /etc/nginx/snippets/
deploy/nginx/render.sh bootstrap <DOMINIO> 127.0.0.1:8090 > /etc/nginx/sites-available/status-sv
ln -s /etc/nginx/sites-available/status-sv /etc/nginx/sites-enabled/status-sv
nginx -t && systemctl reload nginx
```

`nginx -t` va **siempre** antes de recargar: si la configuración tiene un error, no se recarga y
los demás sitios siguen como estaban. La recarga no corta conexiones.

**El certificado**, con la cuenta de certbot que ya tenga el servidor:

```bash
certbot certonly --webroot -w /var/www/certbot -d <DOMINIO> \
  --deploy-hook "systemctl reload nginx" --non-interactive
```

El `--deploy-hook` recarga nginx cada vez que el certificado se renueva; la renovación la hace el
timer de certbot que ya existe.

**La configuración definitiva**, que reemplaza a la provisoria:

```bash
deploy/nginx/render.sh final <DOMINIO> 127.0.0.1:8090 > /etc/nginx/sites-available/status-sv
nginx -t && systemctl reload nginx
```

Con Cloudflare adelante, el registro DNS va **con el proxy activado**, igual que los demás sitios:
así la IP del servidor sigue oculta. El archivo `statussv-cloudflare-realip.conf` hace que nginx
tome la IP real del cliente del encabezado de Cloudflare, pero solo cuando la conexión viene de
un rango de Cloudflare; a quien le pegue directo al servidor con ese encabezado inventado, se lo
ignora.

**Para quitarlo**, sin afectar a los demás sitios:

```bash
rm /etc/nginx/sites-enabled/status-sv
nginx -t && systemctl reload nginx
```

### Verificar

```bash
curl -s https://<DOMINIO>/readyz              # {"status":"ready"}
curl -sI https://<DOMINIO>/healthz | head -1   # 200
docker compose -f docker-compose.prod.yml --env-file .env ps
```

### Crear el usuario del panel

Lo hace una persona por SSH, una sola vez. La contraseña se escribe en la terminal y no se ve; nunca
va como argumento, porque quedaría en el historial y en `ps`. `-it` hace falta para que haya una
terminal: sin ella, el comando lee una línea de la entrada, que es lo que usa la prueba de punta a
punta.

```bash
docker compose -f docker-compose.prod.yml --env-file .env exec -it server server user-add --username juan
```

Para cambiarla (cierra todas las sesiones abiertas de ese usuario):

```bash
docker compose -f docker-compose.prod.yml --env-file .env exec -it server server user-password --username juan
```

**Segundo factor.** El primer login después de crear el usuario pide darlo de alta: el panel
muestra un QR para la app de autenticación (y la clave en texto, para cargarla a mano), se confirma
con un código y entrega **10 códigos de recuperación, que se ven una sola vez**: guardarlos fuera
del teléfono. Desde ahí, cada login pide la contraseña y el código de la app, o uno de los códigos
de recuperación, que sirve una vez.

Si se pierden el teléfono **y** los códigos, se borra el segundo factor por SSH. Cierra todas las
sesiones del usuario y el próximo login pide el alta de nuevo:

```bash
docker compose -f docker-compose.prod.yml --env-file .env exec server server user-totp-reset --username juan
```

### Cloudflare Access delante del panel

El panel ya pide contraseña y código TOTP. Con Cloudflare adelante conviene sumarle igual Access,
que pide identidad **antes** de que el pedido llegue al servidor: así el login del panel ni
siquiera queda expuesto a internet.

La aplicación de Access cubre `<DOMINIO>`, con una excepción que no se puede olvidar:

| Ruta | Access |
| --- | --- |
| `/v1/agents/*` | **excluida** |
| `/mcp` | **excluida** (desde el Sprint 7) |
| todo lo demás | protegida |

Los agents mandan su propia credencial, no una sesión de navegador: si Access queda delante de
`/v1/agents/enroll` o de `/v1/agents/connect`, **los agents dejan de conectarse**. Lo mismo con
`/mcp`: Claude Code manda su token, no puede pasar por el login de Access. El WebSocket del panel
(`/v1/panel/connect`) sí va protegido, y funciona porque el navegador lleva la cookie de
Cloudflare.

Conviene activarlo **después** de verificar que el panel funciona sin él: si algo se rompe, se sabe
qué lo rompió.

### El MCP: conectar Claude Code

Desde el Sprint 7 el control plane atiende un MCP en `https://<DOMINIO>/mcp`, con herramientas de
solo lectura: servidores, métricas, proyectos, deploys y logs
([ADR-024](adr/ADR-024-mcp-remoto.md)). Claude Code entra con un token del MCP
([ADR-025](adr/ADR-025-tokens-del-mcp-y-scopes.md)), que es la única credencial que acepta: la
sesión del panel no sirve, ni el token sirve para el panel.

**El sitio de nginx no cambia.** `/mcp` pasa por la `location /` de siempre: las respuestas son
JSON comunes, sin SSE, así que no hace falta desactivar el buffer ni alargar los tiempos.

**Cloudflare.** `/mcp` tiene que llegar al control plane sin desafíos de navegador: si Access está
puesto, va excluida (ver la tabla de arriba), y si hay reglas contra bots, no pueden aplicarse a
esa ruta. Se verifica desde cualquier máquina, sin token:

```bash
curl -s -o /dev/null -w '%{http_code}\n' -X POST https://<DOMINIO>/mcp
```

Tiene que dar `401`: es el control plane pidiendo el token. Un `302` o un `403` con una página de
Cloudflare es Access o una regla de bots delante.

**1. Crear el token** en el panel, **Tokens → Nuevo token**:

- **Nombre:** dónde se usa (`claude-code en la notebook`). Es lo que la auditoría muestra como
  quien hizo cada llamada.
- **Permisos:** los mínimos. `Leer` alcanza para el estado, las métricas y los deploys; `Logs` va
  aparte porque los logs, aunque salen enmascarados, son lo más sensible que el MCP muestra.
- **Proyectos:** todos los de esos entornos, o solo algunos. Con una lista, el token ve solo los
  servidores de esos proyectos.
- **Vencimiento:** 30 días por defecto, 90 como mucho.
- **Código:** uno nuevo de la app, como un deploy de producción.

El token se muestra **una sola vez**. Si se pierde, se revoca y se crea otro.

**2. Guardarlo en una variable de entorno**, en la máquina donde corre Claude Code, sin escribirlo
en la terminal (quedaría en el historial). En PowerShell, con el token copiado desde el panel:

```powershell
[Environment]::SetEnvironmentVariable('STATUSSV_MCP_TOKEN', (Get-Clipboard), 'User')
```

En Linux o macOS, `read -rs` lo pide sin mostrarlo, y queda en el perfil de la cuenta:

```bash
read -rs STATUSSV_MCP_TOKEN && printf "export STATUSSV_MCP_TOKEN='%s'\n" "${STATUSSV_MCP_TOKEN}" >> ~/.profile
```

**3. Conectar Claude Code**, en una terminal nueva (para que tenga la variable):

```bash
claude mcp add --transport http --scope user status-sv https://<DOMINIO>/mcp --header 'Authorization: Bearer ${STATUSSV_MCP_TOKEN}'
```

- **El comando se copia tal cual.** `${STATUSSV_MCP_TOKEN}` es el nombre de la variable, no un lugar
  para pegar el token. Las comillas **simples** son a propósito: queda escrito así en la
  configuración, y Claude Code pone el valor cada vez que se conecta. Si se pega el token ahí, queda
  en texto plano en `~/.claude.json` y en el historial de la terminal, y además no anda (Claude Code
  busca una variable con ese nombre). En ese caso: revocar el token, `claude mcp remove status-sv
  --scope user`, y empezar de nuevo con uno nuevo.
- La salida de `claude mcp add` tiene que mostrar `"Authorization": "Bearer ${STATUSSV_MCP_TOKEN}"`,
  con el nombre de la variable y no el token.
- `--scope user` lo deja en la configuración de la cuenta, no en un repo. **Nunca** va en un
  `.mcp.json` que se commitea; un escáner de secretos reconoce el prefijo `svmcp_`.

**4. Verificar:**

```bash
claude mcp list
```

`status-sv` tiene que figurar conectado. Adentro de Claude Code, `/mcp` lista las siete
herramientas; para probar, pedirle "listá los servidores de status-sv". En el panel, el token
pasa a mostrar su último uso con la IP, y **Ver lo que hizo** lleva a la auditoría filtrada por él.

**Rotar** (al menos antes de que venza): crear un token nuevo, guardarlo en la variable como en el
paso 2 (pisa el anterior), abrir una terminal nueva y revocar el viejo en el panel. La
configuración de Claude Code no cambia: lee la variable.

**Revocar.** En el panel, **Revocar**: deja de servir en el próximo pedido. Sin panel, por SSH en
el servidor del control plane:

```bash
docker compose -f docker-compose.prod.yml --env-file .env exec server server token-revoke --id <ID>
docker compose -f docker-compose.prod.yml --env-file .env exec server server token-revoke --all
```

`--all` es el freno de emergencia: revoca todos los tokens que siguen sin revocar. Queda en la
auditoría como `cli`.

**Qué ve el LLM y qué no:**

- Solo lee. Las escrituras (`deploy`, `rollback`, `restart`) y las aprobaciones llegan en el
  Sprint 8.
- `get_logs` pide logs solo de las unidades de un proyecto declarado, nunca de cualquier
  contenedor o servicio del servidor, y salen enmascarados del agent.
- El texto que viene de un servidor (logs, salida de los pasos de un deploy) llega aparte y
  marcado como no confiable: puede traer instrucciones escritas para engañar al LLM.
- Cada llamada queda en `audit_log` como `llm`, con el nombre del token y el resultado.

## 2. Agent

### Compilar y copiar el binario

```bash
# En tu máquina:
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w \
  -X github.com/JuanBaez77/status-sv/internal/buildinfo.Version=$(git describe --tags --always) \
  -X github.com/JuanBaez77/status-sv/internal/buildinfo.Commit=$(git rev-parse HEAD) \
  -X github.com/JuanBaez77/status-sv/internal/buildinfo.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o statussv-agent ./agent/cmd/agent

scp -i ~/.ssh/statussv_deploy statussv-agent deploy/install-agent.sh root@<IP>:/tmp/
scp -i ~/.ssh/statussv_deploy -r deploy/systemd root@<IP>:/tmp/
```

### Generar el token

```bash
# En el servidor que tiene el control plane:
cd /opt/status-sv/deploy
docker compose -f docker-compose.prod.yml --env-file .env exec server server enroll-token --server <NOMBRE>
```

El token vive 15 minutos y sirve una sola vez.

### Instalar

```bash
# En el VPS donde va el agent, como root:
cd /tmp
STATUSSV_SERVER_URL=https://<DOMINIO> ./install-agent.sh ./statussv-agent
# Pide el token sin mostrarlo en pantalla.
```

El script crea el usuario `statussv` sin shell, instala el binario y la unidad endurecida, enrola
el agent y arranca el servicio. Si algo falla, muestra los últimos registros.

### Acceso a Docker y al diario

Para ver los contenedores, el agent entra al grupo `docker`, y eso **equivale a darle root en ese
servidor**: la decisión y sus límites están en
[ADR-011](adr/ADR-011-acceso-a-docker-y-journald.md). Al diario entra por `systemd-journal`, que va
en la unidad porque ese grupo existe en cualquier servidor con systemd.

El grupo `docker` no va en la unidad: lo agrega el instalador como drop-in, y solo si el servidor
tiene Docker. La razón es que systemd **no arranca** un servicio cuyo grupo suplementario no existe,
así que ponerlo fijo rompería los VPS sin Docker.

```bash
# Qué quedó aplicado (la unidad y sus drop-ins):
systemctl cat statussv-agent

# Los grupos con los que corre el proceso:
systemctl show statussv-agent -p SupplementaryGroups
```

Antes de tocar el endurecimiento de la unidad, la prueba va en [`deploy/laboratorio/`](../deploy/laboratorio/README.md),
que corre la unidad real sin tocar ningún servidor.

Si instalás Docker **después** del agent, hay que correr `install-agent.sh` de nuevo (no vuelve a
enrolar) para que aparezca el drop-in. Si desinstalás Docker, el mismo script lo saca: sin eso el
servicio no arranca.


### Verificar

```bash
systemctl status statussv-agent
journalctl -u statussv-agent -f
```

Y en la base, que es el criterio de terminado del Sprint 0:

```bash
cd /opt/status-sv/deploy
docker compose -f docker-compose.prod.yml --env-file .env exec postgres \
  psql -U statussv -d statussv -c \
  "SELECT name, agent_version, now() - last_seen_at AS hace FROM servers;"
```

`hace` tiene que quedar siempre por debajo de 15 segundos.

Desde el Sprint 2, además, cada fuente del estado de runtime tiene que estar en `ok` (o en
`unavailable` si ese servidor no tiene Docker), y la foto no puede tener más de 5 minutos:

```bash
docker compose -f docker-compose.prod.yml --env-file .env exec -T postgres \
  psql -U statussv -d statussv -c "SELECT source, status, omitted, detail FROM runtime_sources;" \
  -c "SELECT name, now() - runtime_taken_at AS foto FROM servers;"
```

### Qué métricas lee el agent

`sample` toma una muestra y la imprime como JSON, sin conectarse al control plane. Sirve para
comparar lo que va a mostrar el panel con lo que dicen `df`, `free` o `top`:

```bash
sudo -u statussv /usr/local/bin/statussv-agent sample
```

Los avisos (un disco que no se pudo medir, por ejemplo) salen por stderr.

Hay una diferencia con lo que ve el servicio: la unidad lleva `ProtectHome=true`, y bajo systemd
**un disco montado dentro de `/home` o `/root` no aparece** en la tabla de montajes del agent, así
que no llega al panel. Se verificó con systemd 255. Si un servidor tiene un disco ahí, hay que
decidir si se relaja esa protección.

### Qué ve el agent de contenedores, servicios y procesos

`runtime` arma la misma foto que el agent le manda al control plane y la imprime como JSON, sin
conectarse. Toma dos lecturas separadas por `--interval` para que traiga el consumo:

```bash
sudo -u statussv /usr/local/bin/statussv-agent runtime
```

Cada fuente dice si se pudo leer: `unavailable` con "Docker no está instalado" es lo esperado en un
VPS sin Docker, pero un `error` de permiso sobre el socket quiere decir que falta el drop-in del
grupo `docker` (ver arriba). Corrido con `sudo -u statussv` tiene los grupos de ese usuario en
`/etc/group`, no los del drop-in: para ver exactamente lo que ve el servicio, mirar el log del agent.

Para esconder más servicios que los de siempre, `STATUSSV_SYSTEMD_EXCLUDE` en la unidad, con
patrones separados por comas (por ejemplo `snapd*,unattended-upgrades.service`).

### Declarar un proyecto

Para un proyecto **nuevo**, lo que va es "Dar de alta un proyecto", más abajo: se propone desde el
panel y se acepta acá con un comando, que escribe este archivo. A mano queda para los proyectos que
ya corren en el servidor.

Cada proyecto es un archivo en `/etc/status-sv/projects.d/<nombre>.yaml` del propio servidor
([ADR-012](adr/ADR-012-proyectos-declarados-en-el-servidor.md)). El instalador crea el directorio
(`root:statussv 0750`); los archivos los escribe una persona con SSH y el agent solo los lee. El
control plane no puede crear ni cambiar un proyecto.

```bash
sudo install -o root -g statussv -m 0640 /dev/stdin /etc/status-sv/projects.d/cristales-baez.yaml <<'YAML'
name: cristales-baez
environment: production
repo: github.com/extendsystem/cristales-baez
branch: main
runtime: docker
compose_file: docker-compose.yml
workdir: /srv/cristales-baez
healthcheck:
  url: https://cristales.example.com/health
YAML
```

El formato completo está en DISENO.md. Lo que conviene saber:

- `name` tiene que ser igual al nombre del archivo, y cualquier campo desconocido es error.
- Con Docker, el proyecto se liga a los contenedores por la etiqueta de Compose: si
  `compose_project` falta, es el nombre del directorio de `workdir`, como hace Compose. Con systemd,
  va la lista de servicios en `units`.
- `deploy.pre` y `deploy.post` son los pasos declarados del deploy ([ADR-016](adr/ADR-016-pasos-declarados-del-deploy.md)):
  un servicio de Compose y un comando como lista (`command: ["python", "manage.py", "migrate"]`).
  Ver "Prender el deploy" más abajo.
- Con systemd los pasos no llevan `service` y hay uno más, `deploy.build`, para lo que prepara el
  código ([ADR-019](adr/ADR-019-deploy-de-proyectos-systemd.md)). El primer argumento es la ruta
  del programa, absoluta o relativa al `workdir` (`.venv/bin/pip`): no se busca en el `PATH`. Si
  hay pasos, hace falta `run_as`, el usuario con el que corren, que no puede ser `root` ni
  `statussv`; `env_file` es opcional. Ver "Prender el deploy de un proyecto de systemd".
- Si declara `healthcheck`, el agent hace un `GET` a esa URL cada 60 s desde el propio servidor, sin
  proxy, verificando el certificado y **sin seguir redirecciones**: si la URL responde 301, hay que
  declarar `expect_status: 301` o apuntar a la URL final. Si no se fijan, `expect_status` es 200 y
  `timeout_seconds` 10. Los pedidos llegan con el User-Agent `status-sv-agent/<versión>`.
- Si el sitio pide usuario y contraseña en nginx (autenticación básica), por el dominio siempre da
  401, aunque la app esté caída. En ese caso el healthcheck va al puerto local del contenedor
  (`docker port <contenedor>`), por ejemplo `http://127.0.0.1:8010/health`. Antes de declararlo,
  `curl -s -o /dev/null -w '%{http_code}' <url>` dice qué código poner en `expect_status`.

El agent relee el directorio cada 30 s. Un archivo con errores queda afuera entero, y el motivo (el
campo y la regla, nunca el valor) aparece en el panel y en el registro del agent:

```bash
journalctl -u statussv-agent --since '-5 min' | grep -i proyecto
```

Cada cambio del healthcheck (el primer resultado, y cada vez que pasa de bien a mal o al revés)
queda en el registro:

```bash
journalctl -u statussv-agent --since '-1 hour' | grep -i healthcheck
```

### Prender el deploy

El agent puede desplegar los proyectos Docker declarados: trae la rama, compila, levanta, verifica
y, si no queda sano, vuelve solo a lo anterior ([ADR-015](adr/ADR-015-motor-de-deploy-docker.md)).
**Viene apagado en cada servidor.** Lo prende una persona acá, y el control plane no tiene forma de
prenderlo. Los proyectos de systemd y el alta de un proyecto nuevo tienen su sección más abajo.

> **Nunca en el servidor donde vive el control plane**
> ([ADR-018](adr/ADR-018-control-plane-en-servidor-propio-compartido.md)): un compose puede pedir
> `privileged` o montar el socket de Docker, y quien empuje a la rama de un proyecto de ese servidor
> llegaría por el deploy a la base del control plane. En los demás servidores se prende recién con
> el control plane ya mudado (sección 6).

Hace falta, por proyecto:

**1. Git, Docker Compose y un `workdir` clonado.** El proyecto ya corre en el servidor desde un
clon de git en su `workdir`, que no puede estar bajo `/home` ni `/root` (la unidad los esconde).

**2. Una deploy key de solo lectura**, si el repo es privado (sin llave se trae por HTTPS, que
sirve solo para repos públicos). Se genera en el servidor y la pública se carga en GitHub, en el
repo del proyecto, *Settings → Deploy keys*, **sin** permiso de escritura:

```bash
sudo ssh-keygen -t ed25519 -N '' -C "status-sv $(hostname) <PROYECTO>" -f /etc/status-sv/keys/<PROYECTO>
sudo chown statussv:statussv /etc/status-sv/keys/<PROYECTO>
sudo chmod 0400 /etc/status-sv/keys/<PROYECTO>
sudo cat /etc/status-sv/keys/<PROYECTO>.pub   # esto va a GitHub
sudo rm /etc/status-sv/keys/<PROYECTO>.pub
```

Con llave, `repo: github.com/dueño/repo` se trae por `ssh://git@github.com/dueño/repo.git`. El agent
no usa el `origin` del `workdir` ni las credenciales de root.

**3. Los host keys de GitHub**, verificados contra los que publica GitHub
(<https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/githubs-ssh-key-fingerprints>):

```bash
ssh-keyscan -t ed25519 github.com | sudo tee /etc/status-sv/known_hosts
ssh-keygen -lf /etc/status-sv/known_hosts   # la huella tiene que coincidir con la publicada
```

**4. Permisos del `workdir`.** El agent escribe con el grupo `statussv` en `.git` y en los
directorios con archivos versionados; el dueño sigue siendo root y los datos que no están en git
(volúmenes, `.env`) no se tocan. El `.env` tiene que ser legible por el grupo, porque
`docker compose` lo lee:

```bash
cd /srv/<PROYECTO>
sudo chgrp -R statussv .git && sudo chmod -R g+rwX .git
sudo find .git -type d -exec chmod g+s {} +
{ echo .; git ls-tree -r -d --name-only HEAD; } | while read -r d; do
  sudo chgrp statussv "$d" && sudo chmod g+rwxs "$d"
done
[ -f .env ] && sudo chgrp statussv .env && sudo chmod g+r .env
```

**5. Pasos declarados, si hacen falta.** Las migraciones van en `deploy.pre`, que corre con la
imagen nueva antes de levantarla. **Un rollback no deshace una migración**: tienen que ser
compatibles hacia atrás.

```yaml
deploy:
  pre:
    - service: backend
      command: ["python", "manage.py", "migrate", "--noinput"]
      timeout_seconds: 300
```

**6. Prender el deploy.** Con el mismo instalador, en el servidor, desde donde se copió (con su
carpeta `systemd/` al lado; un servidor que solo tiene agent no tiene `/opt/status-sv`):

```bash
cd /tmp
sudo STATUSSV_SERVER_URL=https://<DOMINIO> ./install-agent.sh --enable-deploy ./statussv-agent
```

El interruptor es **por servidor**: quedan con escritura los `workdir` de todos los proyectos
declarados, también los que no se prepararon. Esos fallan en el chequeo previo sin tocar nada, y
la revisión de abajo lo dice proyecto por proyecto.

Escribe el drop-in `20-deploy.conf` (el interruptor, escritura en el `workdir` de cada proyecto
declarado, `UMask=0002`, `MemoryMax=512M` y `TasksMax=256`), reinicia el agent y corre
`deploy-check` para cada proyecto, como el usuario del agent. `deploy-check` no cambia nada de lo
que corre: revisa la declaración, la llave, los permisos, trae la rama (con la llave, así la prueba)
y valida el archivo de Compose. Se puede correr solo cuando haga falta:

```bash
sudo runuser -u statussv -g statussv -G docker -- /usr/local/bin/statussv-agent deploy-check <PROYECTO>
```

Docker y el plugin de Compose hacen falta solo si hay proyectos de Docker declarados. Si hay algún
proyecto de systemd, el instalador deja además el **ayudante de deploy**
([ADR-019](adr/ADR-019-deploy-de-proyectos-systemd.md)): `statussv-unit-helper.socket`, por donde
el agent le pide a un proceso root que corra los pasos declarados de esos proyectos y reinicie sus
unidades. Sin proyectos de systemd, lo saca. En un servidor sin Docker, `deploy-check` se corre sin
`-G docker`.

Después de declarar un proyecto nuevo, correr el instalador otra vez (sin flags, o con
`--enable-deploy`) para sumar su `workdir` y, si es el primero de systemd, el ayudante. Para
apagarlo, y sacar el ayudante:

```bash
sudo ./install-agent.sh --disable-deploy /usr/local/bin/statussv-agent
```

**Cómo se despliega:** desde la vista del proyecto en el panel. Hay que escribir el nombre del
proyecto y, en producción, un código nuevo de la app de autenticación
([ADR-017](adr/ADR-017-deploy-desde-el-panel-y-auditoria.md)). Todo queda en la auditoría, que se
lee en el panel: la sección **Auditoría**, o el enlace "auditoría de este proyecto" debajo de la
historia de deploys, que la abre ya filtrada. Ahí están también los logins, el segundo factor, el
enrolamiento de agents y los subcomandos del servidor. Si el panel no anda, la misma tabla se lee
por SQL:

```bash
cd /opt/status-sv/deploy
docker compose -f docker-compose.prod.yml exec postgres psql -U statussv -d statussv \
  -c "SELECT at, actor, action, target_id, result, detail FROM audit_log ORDER BY at DESC LIMIT 20"
```

Lo que deja el deploy en el servidor:

- La rama local del `workdir` apunta al commit desplegado (`git log` dice la verdad).
- Las imágenes de los últimos tres estados buenos quedan etiquetadas
  `statussv-rollback/<proyecto>/<servicio>:<commit>`, para volver atrás sin compilar.
- El diario de cada deploy en `/var/lib/status-sv/deploys/`.

### Prender el deploy de un proyecto de systemd

Un proyecto que corre como servicio de systemd (gunicorn, uvicorn, un worker) se despliega con el
mismo motor ([ADR-019](adr/ADR-019-deploy-de-proyectos-systemd.md)): trae la rama, corre los pasos
declarados, reinicia las unidades, verifica y, si no queda sano, vuelve solo al commit anterior.
Valen los puntos 2, 3 y 6 de arriba (la deploy key, los host keys y el instalador). Lo que cambia:

**1. El agent no ejecuta nada del proyecto ni reinicia nada.** Se lo pide al **ayudante de deploy**,
un proceso root que el instalador deja con `--enable-deploy` cuando hay algún proyecto de systemd
declarado (`statussv-unit-helper.socket`). El ayudante solo corre lo que dice el `project.yaml`,
que es de root: por eso el archivo y su directorio no pueden ser escribibles por el grupo.

```bash
systemctl status statussv-unit-helper.socket --no-pager | head -4
ls -ld /etc/status-sv/projects.d /etc/status-sv/projects.d/*.yaml   # root:statussv, 0750 y 0640
```

**2. La declaración.** `units` son los servicios que se reinician, en orden. Los pasos corren con el
usuario de `run_as`, parados en el `workdir`; el primer argumento es la ruta del programa, absoluta
o relativa al `workdir` (no se busca en el `PATH`).

```yaml
name: facturas
environment: production
repo: github.com/extendsystem/facturas
branch: main
runtime: systemd
workdir: /srv/facturas
units: [gunicorn-facturas.service]
run_as: facturas                  # ni root ni statussv
env_file: /etc/facturas/env       # opcional; lo lee systemd, no el agent
healthcheck:
  url: http://127.0.0.1:8001/health
deploy:
  build:                          # lo que prepara el código, sin tocar lo que corre
    - command: [".venv/bin/pip", "install", "-r", "requirements.txt"]
      timeout_seconds: 600
    - command: [".venv/bin/python", "manage.py", "collectstatic", "--noinput"]
  pre:                            # justo antes de reiniciar: las migraciones
    - command: [".venv/bin/python", "manage.py", "migrate", "--noinput"]
rollback: previous_commit
```

**3. Permisos del `workdir`.** Son dos usuarios: el agent trae el código y el usuario del proyecto
escribe lo que preparan sus pasos (el entorno virtual, los estáticos).

```bash
cd /srv/<PROYECTO>
# El agent escribe el código, con el grupo statussv (igual que con Docker).
sudo chgrp -R statussv .git && sudo chmod -R g+rwX .git
sudo find .git -type d -exec chmod g+s {} +
{ echo .; git ls-tree -r -d --name-only HEAD; } | while read -r d; do
  sudo chgrp statussv "$d" && sudo chmod g+rwxs "$d"
done
# El usuario del proyecto tiene que poder escribir donde escriben sus pasos.
sudo chown -R <RUN_AS> .venv staticfiles   # los que correspondan
```

El usuario del proyecto lee el código que trae el agent por los permisos de "otros" (los archivos
quedan `0664`), así que el `workdir` y lo que tiene arriba no pueden cerrarle el paso.

**4. El archivo de entorno**, si hay, puede ser solo de root (`0600`): lo lee systemd al arrancar
cada paso. El agent no lo abre, y lo que un paso imprima sale enmascarado.

**5. Revisar.** En un servidor sin Docker, sin `-G docker`:

```bash
sudo runuser -u statussv -g statussv -G systemd-journal -- /usr/local/bin/statussv-agent deploy-check <PROYECTO>
```

Dice si el ayudante responde, si el usuario, el archivo de entorno y las unidades existen, y qué
pasos va a correr.

Lo que conviene saber antes del primer deploy:

- **Volver atrás corre los pasos `build` de nuevo** sobre el commit anterior: no hay una copia
  guardada del entorno virtual. Con las versiones de las dependencias fijadas, vuelve a lo mismo.
- **Los pasos `pre` no se deshacen** (una migración): tienen que ser compatibles hacia atrás.
- El servicio se da por bueno si sus unidades quedan `active (running)` con el mismo proceso tres
  miradas seguidas, y el healthcheck responde. Uno que arranca, se cae y systemd levanta de nuevo no
  pasa: a los 120 s el deploy vuelve atrás.
- La salida de cada paso queda en el diario, en una unidad propia:

  ```bash
  journalctl -u 'statussv-step-<ID DEL DEPLOY>-*' --no-pager
  journalctl -u 'statussv-unit-helper@*' --since '-1 hour' --no-pager   # lo que se le pidió al ayudante
  ```

### Dar de alta un proyecto

Un proyecto nuevo se da de alta en dos llaves
([ADR-022](adr/ADR-022-alta-de-proyectos-propuesta-y-aceptacion.md)): se **propone** desde el
panel y una persona lo **acepta** en el servidor, como root, con un solo comando. Después se
levanta con el **Primer deploy** ([ADR-021](adr/ADR-021-primer-deploy-y-reparto-del-deploy-desde-cero.md)).
El control plane nunca declara nada: hasta que alguien acepta, el servidor no cambia.

Una vez por servidor hace falta:

- El agent instalado con `--enable-deploy`, de la versión del Sprint 6 o más nueva.
- Si el proyecto va a tener sitio: nginx con `sites-available` y `sites-enabled`, y certbot.
- El registro DNS del dominio apuntando al servidor. Con el proxy de Cloudflare, el modo SSL va en
  **Full**: si no, certbot no llega.

Los pasos:

| | Dónde | Qué |
| --- | --- | --- |
| 1 | Panel | En el servidor, **Nuevo proyecto**. Completar, **Ver lo que se va a escribir** y **Proponer** |
| 2 | Servidor | `sudo statussv-agent project accept <PROYECTO>` |
| 3 | GitHub | Si el repo es privado, cargar la llave pública como deploy key de solo lectura |
| 4 | Servidor | Solo si el paso 2 avisó que falta el ayudante root: correr el instalador de nuevo |
| 5 | Panel | En el proyecto, **Primer deploy** |

**1. Proponer.** El formulario pide:

- el nombre, el entorno, el repo y la rama;
- el comando del servicio (systemd) o el archivo de Compose (Docker);
- los nombres de las variables requeridas;
- el dominio y el puerto del sitio, el healthcheck y los pasos.

Los comandos son listas de argumentos, sin shell: un argumento con espacios va entre comillas
dobles. **De las variables van solo los nombres**: los valores se cargan en el servidor y nunca
pasan por el panel. Lo que el formulario no pide no se elige: el directorio es siempre
`/srv/<PROYECTO>` y, con systemd, el usuario se llama como el proyecto. La página de la propuesta
muestra lo que se va a escribir, en qué anda y el comando para el paso 2. Vence a los 7 días, se
puede retirar, y una propuesta nueva con el mismo nombre la reemplaza.

**2. Aceptar.** Cuando la propuesta dice "Esperando que la acepten en el servidor", el agent ya la
tiene:

```bash
sudo statussv-agent project accept <PROYECTO>
```

Antes de hacer nada, el comando muestra la declaración, los comandos que va a correr, la unidad, el
sitio y la lista de pasos. **No cambia nada hasta que se escribe el nombre del proyecto.**
Después pide cada variable sin mostrarla en pantalla (con `--env-file ARCHIVO` las toma de un
archivo `NOMBRE=valor`). Lo que deja:

| Qué | Dónde |
| --- | --- |
| La declaración | `/etc/status-sv/projects.d/<PROYECTO>.yaml` |
| El usuario (systemd) | `<PROYECTO>`, de sistema y sin shell |
| El `workdir` | `/srv/<PROYECTO>`, 2775 y del grupo `statussv`. Su dueño es el usuario del proyecto (systemd) o root (Docker) |
| Las variables | `/etc/status-sv/env/<PROYECTO>.env`, de root y 0600 (systemd), o `/srv/<PROYECTO>/.env`, root:statussv 0640 (Docker) |
| La deploy key | `/etc/status-sv/keys/<PROYECTO>`. La pública se muestra al final y en el panel |
| La unidad (systemd) | `/etc/systemd/system/<PROYECTO>.service`, desde la plantilla |
| El sitio | `/etc/nginx/sites-available/statussv-<PROYECTO>.conf` y su enlace, con el certificado `statussv-<PROYECTO>` |
| El drop-in del deploy | Suma el `workdir` y reinicia el agent |

Al terminar corre `deploy-check`, que tiene que decir **"todavía no se desplegó: está listo para
su primer deploy"**. Si el repo es público, `--no-deploy-key` no genera la llave.

**4. El ayudante root.** Con systemd, el agent necesita el ayudante root
([ADR-019](adr/ADR-019-deploy-de-proyectos-systemd.md)). El instalador lo deja solo si ya hay
proyectos de systemd. Si es el primero del servidor, `project accept` avisa, y va el instalador de
nuevo, que conserva la credencial:

```bash
cd /tmp && sudo STATUSSV_SERVER_URL=https://<DOMINIO> ./install-agent.sh --enable-deploy ./statussv-agent
```

**5. Primer deploy.** En el panel, el proyecto muestra lo que falta:

- las variables que todavía no se cargaron (los nombres, nunca los valores);
- la llave pública;
- el sitio.

**Primer deploy** pide el nombre del proyecto y **siempre** un código nuevo de la app, también en
staging.

Si algo quedó a medias:

- **El sitio no quedó** (el DNS todavía no resolvía, certbot no llegó): el alta sigue igual, nginx
  queda como estaba y el comando dice por qué. Cuando esté resuelto:
  `sudo statussv-agent project site <PROYECTO>`.
- **Una variable falta o hay que cambiarla**: `sudo statussv-agent project env <PROYECTO>` (o
  `--env-file`). Con variables faltantes, el deploy se frena antes de empezar.
- **La propuesta llegó rechazada**: el agent dice por qué en su página (por ejemplo, que ya
  guarda 10). Se corrige y se propone de nuevo.

Lo que conviene saber del primer deploy:

- **El `workdir` no tiene que estar vacío, sino sin clonar.** Lo que dejó una persona (el `.env`)
  queda como está: si un archivo choca con uno del repositorio, el deploy falla sin pisarlo.
- **Si no queda bien, baja lo que levantó**: `docker compose down` (los volúmenes quedan) o parar
  las unidades. **El código clonado queda**, para mirar qué pasó, y el intento siguiente lo acepta.
- Con algo del proyecto ya corriendo, el primer deploy se niega (`already_deployed`): lo que va es
  un deploy común. Y al revés, un deploy común sobre un `workdir` sin clonar dice que hace falta el
  primer deploy.
- Es siempre la punta de la rama declarada.
- Con systemd, cuando queda bien **habilita las unidades** para que arranquen con el servidor: ya
  no hace falta `systemctl enable` a mano.
- El repositorio que deja el agent es suyo (`statussv`). Para correr `git` a mano ahí como root:
  `git -c safe.directory=/srv/<PROYECTO> -C /srv/<PROYECTO> log -1`.

Un proyecto que ya corre en el servidor, o que necesita lo que una propuesta no elige (otro
`workdir`, varias unidades, un sitio armado a mano), se sigue declarando a mano, como en "Declarar
un proyecto", con su deploy prendido como en las dos secciones anteriores.

### Sacar un proyecto

No hay un comando: lo hace una persona como root, en el orden inverso al alta. Antes, mirar en el
panel que el proyecto no tenga un deploy en curso. Se hizo así el 2026-10-09 con los dos
proyectos de demostración de `sv-gcg`.

```bash
# 1. Pararlo. De systemd:
sudo systemctl disable --now <UNIDAD>.service
sudo rm -rf /etc/systemd/system/<UNIDAD>.service /etc/systemd/system/<UNIDAD>.service.d
sudo systemctl daemon-reload
# De Docker (los volúmenes quedan; con -v se borran también):
cd /srv/<PROYECTO> && sudo docker compose down; cd /

# 2. Sus imágenes, con las etiquetas de vuelta atrás que dejó el deploy:
sudo docker images | grep -e <PROYECTO>
sudo docker rmi <IMAGEN>:<TAG> statussv-rollback/<PROYECTO>/<SERVICIO>:<COMMIT>

# 3. La declaración, lo que el agent guarda del proyecto y lo que dejó el alta:
sudo rm -f /etc/status-sv/projects.d/<PROYECTO>.yaml /var/lib/status-sv/deploys/projects/<PROYECTO>.json
sudo rm -f /etc/status-sv/env/<PROYECTO>.env /etc/status-sv/keys/<PROYECTO> /etc/status-sv/keys/<PROYECTO>.pub
sudo rm -rf /srv/<PROYECTO>
sudo userdel <RUN_AS>   # de systemd, si el usuario era solo de este proyecto

# 4. El sitio y el certificado, si los tenía:
sudo rm -f /etc/nginx/sites-enabled/statussv-<PROYECTO>.conf /etc/nginx/sites-available/statussv-<PROYECTO>.conf
sudo nginx -t && sudo systemctl reload nginx
sudo certbot delete --cert-name statussv-<PROYECTO>

# 5. El instalador otra vez, con el binario de la versión instalada (sale del mismo tarball):
sudo ./install-agent.sh ./statussv-agent
```

El paso 5 reescribe el drop-in sin el `workdir` del proyecto y, si ya no queda ningún proyecto de
systemd, saca el ayudante root. Si el proyecto tenía deploy key, queda borrar su llave pública en
el repo (en GitHub, *Settings → Deploy keys*). El proyecto desaparece del panel cuando el agent
reconecta; su historial de deploys y la auditoría quedan.

## 3. Actualizar

**Control plane:** copiar el nuevo commit igual que en el paso 1, actualizar `STATUSSV_COMMIT` y
`STATUSSV_VERSION` en `deploy/.env`, y:

```bash
# Si la versión nueva trae una migración, el respaldo primero: migrate down deshace el
# esquema, pero no devuelve lo que la migración haya transformado.
docker compose -f docker-compose.prod.yml --env-file .env exec -T postgres \
  pg_dump -U statussv -d statussv | gzip > /root/statussv-db-$(date +%Y%m%d-%H%M%S).sql.gz

docker compose -f docker-compose.prod.yml --env-file .env build server
docker compose -f docker-compose.prod.yml --env-file .env run --rm server migrate up
docker compose -f docker-compose.prod.yml --env-file .env up -d server
```

En `srv936263` el directorio es `/root/status-sv/deploy` y cada comando lleva además
`-f docker-compose.snap.yml` (sección 1, *Si el Docker del servidor es el de snap*).

**En `srv936263`, antes de actualizar: ningún vecino con el socket de Docker ni `privileged`.** El
control plane se queda en ese servidor compartido con esa condición
([ADR-027](adr/ADR-027-control-plane-sigue-en-srv936263-con-escrituras.md)). El chequeo es de solo
lectura y no tiene que imprimir nada:

```bash
docker ps -q | xargs docker inspect --format '{{.Name}} privileged={{.HostConfig.Privileged}} {{range .Mounts}}{{.Source}} {{end}}' | grep -E 'privileged=true|docker.sock'
```

Si imprime algo, no se actualiza: se avisa y se revisa ADR-027.

Los agents se desconectan durante el reinicio y reconectan solos con backoff: no hace falta
tocarlos.

El panel viaja adentro del binario, así que una versión nueva del control plane trae el panel
nuevo: no hay nada que copiar aparte. Lo que sí hay que mirar es si cambió la plantilla de nginx
(`deploy/nginx/status-sv.conf.template`): si cambió, hay que volver a generar el sitio, porque el
archivo que está en el servidor se generó con la plantilla vieja.

```bash
# Solo si cambió la plantilla. El respaldo primero, siempre.
tar -czf /root/nginx-backup-$(date +%Y%m%d-%H%M%S).tar.gz /etc/nginx
curl -sI https://<OTRO-SITIO-DEL-SERVIDOR> | head -1    # la línea de base, antes de tocar

# A un archivo temporal y después install: si render.sh falla a mitad de camino,
# el sitio que está en uso no queda truncado.
/opt/status-sv/deploy/nginx/render.sh final <DOMINIO> 127.0.0.1:8090 > /tmp/status-sv.conf.nuevo
install -m 0644 /tmp/status-sv.conf.nuevo /etc/nginx/sites-available/status-sv
nginx -t && systemctl reload nginx

curl -sI https://<OTRO-SITIO-DEL-SERVIDOR> | head -1    # tiene que seguir igual
```

**Agent:** copiar el binario nuevo y volver a correr `install-agent.sh`. Reemplaza el binario y
reinicia el servicio **sin volver a enrolar**: la credencial se conserva.

Un binario copiado desde Windows llega **sin permiso de ejecución**, y el instalador se planta
antes de tocar nada: `chmod +x` al binario y de nuevo.

## 4. Volver atrás

**Control plane:** cada versión queda como imagen `status-sv-server:<versión>`. Para volver a la
anterior, poner esa versión en `STATUSSV_VERSION` y `up -d server`. Si la versión nueva traía una
migración, antes: `run --rm server migrate down`.

**Agent:** volver a correr `install-agent.sh` con el binario anterior. Conviene guardarlo antes
de actualizar (`cp -p /usr/local/bin/statussv-agent /root/statussv-agent-<versión>`), porque el
instalador lo reemplaza. Un binario anterior al Sprint 2 no lee Docker, así que el drop-in
`10-docker.conf` le da un acceso que no usa: para volver del todo, borrar también
`/etc/systemd/system/statussv-agent.service.d/10-docker.conf` y `systemctl daemon-reload`.

## 5. Revocar un agent

Si un VPS se pierde o se sospecha que su credencial se filtró, se revoca desde la base hasta que
el panel lo permita:

```sql
UPDATE agent_credentials SET revoked_at = now(), revoked_reason = 'motivo'
WHERE server_id = (SELECT id FROM servers WHERE name = '<NOMBRE>') AND revoked_at IS NULL;
```

El agent queda rechazado desde su próxima reconexión. Para volver a sumarlo, un token nuevo e
`install-agent.sh --reenroll`.

## 6. Mudar el control plane a otro VPS

Sirve para cualquier mudanza. La de
[ADR-018](adr/ADR-018-control-plane-en-servidor-propio-compartido.md) se hizo el 2026-10-02: de
`cristales-baez-prod` a `srv936263`, un servidor propio que **ya tiene nginx**, con dos minutos de
corte. La del VPS dedicado con Caddy ([ADR-014](adr/ADR-014-control-plane-en-vps-propio.md)) queda
escrita para más adelante. Los pasos son los mismos, y donde cambian según el destino está dicho.
Se muda la base entera: los agents conservan su credencial y, como el dominio no cambia,
**reconectan solos**; no se tocan.

El corte (del paso 3 al 5) tiene que durar **menos de 10 minutos**: es lo que cada agent guarda de
métricas en memoria sin conexión. Un agent reintenta como mucho cada minuto, así que en el peor caso
vuelve un minuto después de que el control plane nuevo responda. Conviene tener los comandos a mano
y los dos servidores abiertos antes de empezar.

**1. Preparar el VPS nuevo** sin cortar nada: la sección 0 entera (usuario, SSH, firewall con 22, 80
y 443, Docker), y de la sección 1 *Copiar el código* y *Configuración y secretos*, con el mismo
`STATUSSV_DOMAIN` y una clave de TOTP **nueva**. Si el servidor viejo ya tenía TOTP en uso, en
cambio, hay que copiar su `deploy/secrets/totp.key` tal cual (con `scp` a tu máquina y de ahí al
nuevo, con dueño 10001 y 0400): sin esa clave, los segundos factores guardados no se pueden leer.
Después, solo la base:

```bash
# En el VPS nuevo:
cd /opt/status-sv/deploy
docker compose -f docker-compose.prod.yml --env-file .env build
docker compose -f docker-compose.prod.yml --env-file .env up -d postgres
```

**Si el destino ya tiene nginx y otros productos**, de la sección 0 no se toca ni su firewall ni su
Docker. En cambio se releva, sin cambiar nada, que el puerto local esté libre y que ningún
contenedor monte el socket de Docker ni corra `privileged` (ADR-018, regla 3); el segundo comando
no tiene que imprimir nada:

```bash
# En el servidor nuevo:
ss -tln | grep ':8090 '        # vacío: el puerto de STATUSSV_LOCAL_PORT está libre
docker ps -q | xargs docker inspect \
  --format '{{.Name}} privileged={{.HostConfig.Privileged}} {{range .Mounts}}{{.Source}} {{end}}' \
  | grep -E 'privileged=true|docker.sock'
```

Si su Docker es el de snap, valen además el directorio y el archivo de ajuste de la sección 1 (*Si
el Docker del servidor es el de snap*).

**El certificado y el sitio se dejan listos antes del corte**, así el dominio nunca apunta a un
servidor sin HTTPS. Mientras el dominio apunte al servidor viejo, Let's Encrypt no puede validar
por HTTP en el nuevo, así que el primer certificado se pide por DNS:

```bash
# En el servidor nuevo. Es interactivo: muestra un valor y espera.
certbot certonly --manual --preferred-challenges dns -d <DOMINIO>
```

Con certbot esperando, se crea en Cloudflare un registro **TXT** llamado `_acme-challenge.<nombre>`
(para `status.ejemplo.com`, `_acme-challenge.status`) con ese valor, **aparte** del registro A, que
no se toca. Antes de apretar Enter se comprueba desde otra máquina que el registro ya se ve; cada
intento fallido gasta uno de los cinco por hora que da Let's Encrypt:

```bash
nslookup -type=TXT _acme-challenge.<DOMINIO>. 1.1.1.1    # con el punto final
```

Con el certificado, se instala el sitio definitivo de la sección 1 (*Detrás de nginx*, sin la
configuración provisoria). Todavía no recibe nada, y los demás sitios no se tocan.

**Y se ensaya el arranque entero**, sin tocar el DNS, con un volcado tomado en caliente del
servidor viejo (el `pg_dump` del paso 3, sin el `stop`): restaurar, `migrate up`, `up -d`, y las
dos comprobaciones:

```bash
curl -s http://127.0.0.1:8090/readyz                                   # {"status":"ready"}
curl -sk -o /dev/null -w '%{http_code}\n' \
  --resolve <DOMINIO>:443:127.0.0.1 https://<DOMINIO>/readyz           # 200, por nginx
```

Un servidor distinto falla en cosas que no se ven hasta arrancar el contenedor. Después del ensayo
se para el control plane nuevo y se vacía su base, para que el corte restaure sobre una base
limpia y no pueda usar un volcado viejo:

```bash
docker compose -f docker-compose.prod.yml --env-file .env stop server
docker compose -f docker-compose.prod.yml --env-file .env exec -T postgres dropdb -U statussv statussv
docker compose -f docker-compose.prod.yml --env-file .env exec -T postgres createdb -U statussv -O statussv statussv
rm -f /root/statussv-mudanza.dump
```

**2. Bajar el TTL.** En Cloudflare, el registro del dominio con TTL de 1 minuto (con la nube
naranja ya es *Auto*, que es 5 minutos; alcanza). Si el registro va a seguir con la nube naranja
durante toda la mudanza, este paso no hace falta: el cambio de IP lo hace Cloudflare en el momento
y los agents no ven ningún cambio de DNS.

**3. Cortar y volcar el servidor viejo.** Primero se detiene el control plane, para que no quede
nada escrito después del volcado:

```bash
# En el servidor viejo:
cd /opt/status-sv/deploy
docker compose -f docker-compose.prod.yml --env-file .env stop server
docker compose -f docker-compose.prod.yml --env-file .env exec -T postgres \
  pg_dump -U statussv -d statussv -Fc > /root/statussv-mudanza.dump
ls -lh /root/statussv-mudanza.dump
```

```bash
# En tu máquina: de uno al otro, sin guardarlo en ningún lado más.
scp -i ~/.ssh/statussv_deploy root@<IP-VIEJA>:/root/statussv-mudanza.dump /tmp/
scp -i ~/.ssh/statussv_deploy /tmp/statussv-mudanza.dump root@<IP-NUEVA>:/root/
rm /tmp/statussv-mudanza.dump
```

**4. Restaurar y migrar en el nuevo.** Las migraciones pendientes se aplican sobre los datos
restaurados, no antes:

```bash
# En el VPS nuevo:
cd /opt/status-sv/deploy
docker compose -f docker-compose.prod.yml --env-file .env exec -T postgres \
  pg_restore -U statussv -d statussv --no-owner --exit-on-error < /root/statussv-mudanza.dump
docker compose -f docker-compose.prod.yml --env-file .env run --rm -T server migrate up </dev/null
docker compose -f docker-compose.prod.yml --env-file .env --profile caddy up -d
```

Con nginx en el destino, el último comando va sin `--profile caddy`, y se comprueba por adentro:
`curl -s http://127.0.0.1:8090/readyz`.

**5. Apuntar el dominio.** Depende de quién atienda el 443 en el destino.

*Con nginx en el destino*, el certificado y el sitio ya están (paso 1): el registro A pasa a
`<IP-NUEVA>` **sin apagar el proxy**, y **solo si** el paso 4 terminó con `{"status":"ready"}` por
adentro y 200 por nginx. Si no, no se toca el DNS: se levanta de nuevo el control plane viejo
(`up -d server`) y se mira el error sin apuro.

```bash
curl -s https://<DOMINIO>/readyz        # {"status":"ready"}: contesta el nuevo, el viejo está parado
```

El certificado pedido por DNS **no se renueva solo**. Con el dominio ya apuntando al servidor
nuevo, se reemite por el camino de siempre, que deja la renovación a cargo del timer de certbot.
Primero en seco:

```bash
certbot certonly --webroot -w /var/www/certbot -d <DOMINIO> --cert-name <DOMINIO> --dry-run
certbot certonly --webroot -w /var/www/certbot -d <DOMINIO> --cert-name <DOMINIO> \
  --force-renewal --deploy-hook "systemctl reload nginx" --non-interactive
```

Y se borra el registro TXT de Cloudflare.

*Con Caddy en el destino*, el registro A del dominio pasa a `<IP-NUEVA>` **con la nube gris**, para
que Caddy pueda pedir el certificado:

```bash
docker compose -f docker-compose.prod.yml --env-file .env logs -f caddy   # hasta "certificate obtained"
curl -s https://<DOMINIO>/readyz                                          # {"status":"ready"}
```

Con el certificado, la nube naranja de nuevo y el modo SSL en Full (strict). Mientras está gris,
Cloudflare Access no protege el panel: que dure solo lo que tarda el certificado.

**6. Verificar.** Los agents tienen que volver solos en uno o dos minutos:

```bash
docker compose -f docker-compose.prod.yml --env-file .env exec -T postgres psql -U statussv -d statussv -c \
  "SELECT name, agent_version, round(extract(epoch FROM now() - last_seen_at)) AS hace_s FROM servers ORDER BY name"
```

En el panel: cada servidor en línea, y en el gráfico de la última hora a lo sumo un hueco del
largo del corte si pasó de 10 minutos. Si la base traía sesiones, la migración 00007 las cerró:
el primer login pide dar de alta el segundo factor.

**7. Limpiar el servidor viejo**, recién con todo verificado. Sus contenedores se quitan, pero el
volumen de la base queda una semana como respaldo:

```bash
# En el servidor viejo:
cd /opt/status-sv/deploy
docker compose -f docker-compose.prod.yml --env-file .env down      # sin -v: el volumen queda
rm /root/statussv-mudanza.dump

# El sitio de nginx de status-sv. El respaldo primero, y el sitio del cliente antes y después.
tar -czf /root/nginx-backup-$(date +%Y%m%d-%H%M%S).tar.gz /etc/nginx
curl -sI https://<OTRO-SITIO-DEL-SERVIDOR> | head -1
rm /etc/nginx/sites-enabled/status-sv /etc/nginx/sites-available/status-sv
nginx -t && systemctl reload nginx
curl -sI https://<OTRO-SITIO-DEL-SERVIDOR> | head -1    # tiene que seguir igual

# El certificado de certbot del dominio: el dominio ya no apunta acá, y su
# renovación fallaría todos los días.
certbot delete --cert-name <DOMINIO> --non-interactive
```

El agent de ese servidor sigue corriendo como antes. Pasada la semana:
`docker volume rm status-sv_postgres-data` y `rm -rf /opt/status-sv`. En el VPS nuevo, también:
`rm /root/statussv-mudanza.dump`.

**Si algo sale mal antes del paso 7**, se vuelve atrás apuntando el dominio a la IP vieja y
levantando de nuevo el control plane viejo (`up -d server`): su base quedó como estaba en el
volcado. Lo que los agents hayan mandado al nuevo en el medio se pierde.

**Endurecimiento opcional.** Con todo andando detrás de Cloudflare, los puertos 80 y 443 del VPS
se pueden cerrar a todo lo que no venga de los rangos de Cloudflare. Si se hace, para pedir un
certificado desde cero (por ejemplo, si se perdió `caddy-data`) hay que abrirlos de nuevo.

## 7. Qué no hacer

- **No publicar el puerto de PostgreSQL.** Docker lo expondría a internet por fuera del firewall.
- **No pasar tokens como argumento.** Quedan visibles en `ps` para cualquier usuario del sistema.
- **No escribir un token del MCP en un comando ni en un `.mcp.json`.** Va en una variable de
  entorno, y Claude Code la lee al conectarse (ver "El MCP: conectar Claude Code").
- **No borrar el volumen `caddy-data`.** Tiene los certificados, y Let's Encrypt limita cuántos se
  pueden pedir por semana.
- **No commitear `deploy/.env`.** Ya está en `.gitignore`, pero conviene revisarlo en cada PR.
- **No correr `docker image prune -a` en un servidor con el deploy prendido** sin mirar antes:
  puede borrar las imágenes `statussv-rollback/...` a las que vuelve un rollback manual (el rollback
  lo detecta y no arranca, pero ya no puede volver).
- **No editar a mano archivos versionados en el `workdir` de un proyecto que se despliega.** El
  deploy no los pisa: falla antes de empezar con "archivos cambiados a mano". Un arreglo va por PR.
- **No prender el deploy en el servidor donde vive el control plane**
  ([ADR-018](adr/ADR-018-control-plane-en-servidor-propio-compartido.md)), ni sumar ahí un
  contenedor que monte el socket de Docker o corra `privileged`.

## 8. Lecciones de los despliegues

Salieron al desplegar en `cristales-baez-prod` y cuestan tiempo si se redescubren:

- **Un binario copiado desde Windows llega sin permiso de ejecución.** `scp` conserva los permisos
  del origen, y NTFS no tiene el bit de ejecución: `chmod +x` antes de instalar. El instalador lo
  verifica y lo dice, en vez de dejar creer que el binario no corre en ese servidor.
- **El control plane acepta cinco enrolamientos por minuto desde la misma IP.** Los tokens son de un
  solo uso y el límite los protege de la fuerza bruta. Cada agent viene de su propio VPS, así que no
  molesta; pero enrolar varios desde la misma salida a internet lleva un par de minutos, y los
  rechazos dicen `rate_limited`. Hay que esperar y reintentar, no generar tokens nuevos.
- **Al actualizar, mirar si cambió la plantilla de nginx.** El sitio del servidor se generó con la
  plantilla del día que se instaló: una `location` nueva (por ejemplo, la del WebSocket del panel)
  no aparece sola. Sin ella el panel carga pero los gráficos no avanzan.

- **Crear el registro DNS antes de consultarlo.** Si alguien pregunta por el dominio cuando todavía
  no existe, los resolvers guardan la respuesta negativa hasta el mínimo del SOA: en Cloudflare, 30
  minutos. El resolver del proveedor del VPS lo hizo, y el agent no pudo resolver el dominio hasta
  que venció. `dig @153.92.2.6 <DOMINIO> +noall +authority` muestra cuánto le falta.
- **Por SSH con un script por la entrada estándar, `docker compose run` y `exec` van con `-T` y
  `</dev/null`.** Si no, leen la entrada estándar y se tragan el resto del script, que termina en
  silencio a la mitad. Interactivamente no pasa.
- **Recién levantado, el control plane ocupa varias veces lo que ocupa andando.** En la
  actualización del Sprint 2 midió 43 MB al minuto de arrancar y 5 MB a los cinco: es lo que el
  runtime de Go todavía no devolvió. El consumo se mide pasados unos minutos, no al arrancar.
- **Cloudflare responde 403 (`error code: 1000`) si el cliente manda su propio `CF-Connecting-IP`.**
  Es esperado y es una protección más: ese encabezado lo escribe solo Cloudflare. Para probar la
  falsificación de IP a través del proxy, alcanza con `X-Forwarded-For`.

Salieron al mudar el control plane a `srv936263` (2026-10-02). El primer intento hubo que
revertirlo, con diez minutos de corte; el segundo tardó dos:

- **Ensayar el arranque entero en el destino antes de cortar.** El primer intento se cayó en el
  último paso: el contenedor no arrancaba con el Docker de snap. Se había probado que montara
  archivos, no que ejecutara el binario con las opciones del compose. Un ensayo con un volcado en
  caliente lo habría mostrado sin cortar nada.
- **El Docker de snap no es un Docker cualquiera.** No monta archivos desde `/opt`, guarda sus
  volúmenes en `/var/snap/docker/common/var-lib-docker` y no ejecuta contenedores con
  `no-new-privileges`. Está en la sección 1.
- **No cambiar el DNS si el paso anterior no terminó bien.** Con el control plane nuevo caído y el
  dominio ya apuntando a él, volver atrás son dos pasos en vez de uno, y el segundo se olvida:
  levantar el viejo no alcanza si el dominio sigue mirando al nuevo.
- **El certificado se pide antes, por DNS.** Así no hay ninguna ventana con el dominio apuntando a
  un servidor sin HTTPS, y no importa el modo SSL de la zona. El registro TXT va **aparte** del
  registro A; que el A siga apuntando al servidor viejo es lo correcto.
- **`nslookup` sin punto final miente en Windows.** Le agrega el sufijo DNS de la máquina y
  consulta otro nombre. `nslookup -type=TXT _acme-challenge.status.ejemplo.com. 1.1.1.1`, con el
  punto.
- **Con dos servidores y una PC, cada comando dice en qué ventana va**, y antes de pegarlo se mira
  el prompt. Un comando del servidor viejo pegado en el nuevo falló en el `cd` y no hizo nada
  porque estaba encadenado con `&&`; uno pegado con el prompt copiado adelante, tampoco. Encadenar
  con `&&` y empezar por un `cd` a un directorio que solo existe en el servidor correcto es una
  red barata.
- **Antes de borrar algo que "ya no se usa", mirar si recibe pedidos.** En `srv936263` quedaba una
  copia vieja de un proyecto con mil pedidos en tres días. Eran robots: otro sitio de nginx del
  mismo servidor seguía mandando su tráfico a ese puerto. Se respaldó, se apagó, se verificó y
  recién después se borró.

Salieron al desplegar el Sprint 5 (2026-10-05), con los dos proyectos de demostración de `sv-gcg`
(`status-sv-demo`, de systemd, y `status-sv-demo-docker`):

- **Docker Hub limita las descargas sin cuenta por IP, y en IPv6 cuenta la /64 entera.** `sv-gcg`
  sale por IPv6 y comparte la /64 con otros clientes del proveedor: tenía 100 descargas por hora y
  le quedaban 0. El primer deploy de Docker falló al compilar, en el `FROM`, con
  `429 Too Many Requests`; terminó `failed` sin levantar nada. El cupo de un servidor se mira así
  (solo consulta; `docker-ratelimit-source` dice qué IP o qué /64 cuenta Docker Hub):

  ```bash
  T=$(curl -fsS 'https://auth.docker.io/token?service=registry.docker.io&scope=repository:ratelimitpreview/test:pull' \
    | sed -e 's/.*"access_token":"//' -e 's/".*//')
  curl -sS --head -H "Authorization: Bearer $T" \
    https://registry-1.docker.io/v2/ratelimitpreview/test/manifests/latest | grep -i -e ^HTTP -e ratelimit
  ```

  Las salidas: tomar la imagen base de una copia sin ese límite (`public.ecr.aws/docker/library/…`
  o `mirror.gcr.io/library/…`), que es un commit en el repo del proyecto; un espejo en el
  `daemon.json` del servidor, que obliga a reiniciar Docker; o `docker login`, que deja una
  credencial en el servidor para el usuario del agent.
- **Después de actualizar el control plane, recargar el panel.** El panel se carga entero al abrir
  la página: una pestaña abierta desde antes sigue con la versión vieja, aunque el servidor ya
  sirva la nueva. Mostró "El deploy de proyectos de systemd llega en el Sprint 5" con el deploy de
  systemd ya desplegado.
- **La unidad de un proyecto nuevo se habilita después del primer deploy, no antes.** Habilitada y
  sin código, intentaría arrancar si el servidor se reinicia. Mientras tanto el panel avisa que el
  agent no ve el servicio: el agent lista un servicio parado solo si está habilitado.
- **En un proyecto gunicorn, lo que cuenta es el healthcheck HTTP.** Una app que se cae al
  importarse con una excepción hace salir a gunicorn ("Worker failed to boot", código 3) y systemd
  lo relanza cada pocos segundos. Si se cae con `SystemExit`, en cambio, gunicorn relanza los
  workers para siempre y la unidad sigue `active`: solo el healthcheck HTTP lo ve.
- **Un deploy roto tarda lo que la ventana de verificación.** El roto de systemd tardó 2 min 33 s
  y el de Docker 2 min 32 s, de los que 2 minutos son esperar que quede sano: systemd y Docker
  relanzan el servicio caído, y el agent no corta antes.
- **El `pg_dump` nuevo (17.6 en adelante) no termina con "dump complete".** La última línea del
  volcado es `\unrestrict <token>`. Para revisar un respaldo, buscar la línea con
  `zcat <respaldo> | grep -c 'dump complete'`, no mirar la última.
- **Desde PowerShell 5.1, el comando de `ssh` va entre comillas simples y sin comillas dobles
  adentro.** PowerShell rompe las comillas dobles al pasárselas a `ssh`, y el comando llega cortado
  al servidor. Un punto en vez de un espacio sirve para `grep` (`grep -e agent.conectado`).

Salieron al desplegar el Sprint 6 (2026-10-09) y de la prueba de punta a punta del alta:

- **`systemctl reload nginx` vuelve antes de que nginx tome la configuración nueva.** Un pedido
  hecho enseguida puede llegar todavía a la anterior. El alta, antes de llamar a certbot, espera a
  que nginx sirva un archivo de prueba en la ruta del desafío: sin eso, certbot pierde el intento,
  y Let's Encrypt acepta pocos intentos fallidos por hora para un mismo dominio.
- **La memoria del agent se mide con la RSS del proceso, no con el cgroup.**
  `systemctl show -p MemoryCurrent` cuenta también la caché de los archivos que leyó y escribió el
  servicio: después de dos altas, con sus clones y sus builds, marcó 61 MiB en el CI.
  `ps -o rss= -C statussv-agent` dio 17 MB en `sv-gcg`, con una hora andando.
- **systemd no borra el archivo de un socket al pararlo**, salvo con `RemoveOnStop=yes`. El
  instalador sacó el ayudante de `sv-gcg` y su socket siguió en `/run/status-sv`; la unidad ya lo
  trae.
- **Un bloque para pegar en un servidor va adentro de un `if` que mira el hostname.** El bloque
  del agent de `cristales-baez-prod` se pegó en la sesión de `srv936263` y falló en cada renglón
  sin tocar nada, porque ahí no estaba el archivo; con otro bloque podría no haber fallado. Así no
  corre en otro servidor ni si se pega en PowerShell, que no entiende el `if` de bash, y con `&&`
  entre los pasos se frena en el primero que falla:

  ```bash
  if [ "$(hostname)" = <SERVIDOR> ]; then
    echo '<SHA256>  /tmp/<ARCHIVO>' | sha256sum -c &&
    <PASO> &&
    <PASO>
  else
    echo "esto va en <SERVIDOR> y estás en $(hostname)"
  fi
  ```

- **Una salida larga se corta al copiarla de la terminal.** Un `find` de cuarenta renglones llegó
  cortado y hubo que pegarlo en dos veces. Para confirmar algo, mejor contar (`| wc -l`,
  `| sort | uniq -c`) que listar.
