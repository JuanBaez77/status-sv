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
- **Servidor que ya tiene nginx** (por ejemplo, el de un cliente): el control plane queda en
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

**Detrás de nginx** — sin Caddy:

```bash
docker compose -f docker-compose.prod.yml --env-file .env up -d
curl -s http://127.0.0.1:8090/readyz    # {"status":"ready"}, con el puerto elegido
```

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
| todo lo demás | protegida |

Los agents mandan su propia credencial, no una sesión de navegador: si Access queda delante de
`/v1/agents/enroll` o de `/v1/agents/connect`, **los agents dejan de conectarse**. El WebSocket del
panel (`/v1/panel/connect`) sí va protegido, y funciona porque el navegador lleva la cookie de
Cloudflare.

Conviene activarlo **después** de verificar que el panel funciona sin él: si algo se rompe, se sabe
qué lo rompió.

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
- `deploy.pre` y `deploy.post` tienen que estar vacíos hasta el Sprint 4.
- Si declara `healthcheck`, el agent hace un `GET` a esa URL cada 60 s desde el propio servidor, sin
  proxy, verificando el certificado y **sin seguir redirecciones**: si la URL responde 301, hay que
  declarar `expect_status: 301` o apuntar a la URL final. Si no se fijan, `expect_status` es 200 y
  `timeout_seconds` 10. Los pedidos llegan con el User-Agent `status-sv-agent/<versión>`.

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

## 6. Qué no hacer

- **No publicar el puerto de PostgreSQL.** Docker lo expondría a internet por fuera del firewall.
- **No pasar tokens como argumento.** Quedan visibles en `ps` para cualquier usuario del sistema.
- **No borrar el volumen `caddy-data`.** Tiene los certificados, y Let's Encrypt limita cuántos se
  pueden pedir por semana.
- **No commitear `deploy/.env`.** Ya está en `.gitignore`, pero conviene revisarlo en cada PR.

## 7. Lecciones de los despliegues

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
