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

## 2. Agent

### Compilar y copiar el binario

```bash
# En tu máquina:
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w \
  -X github.com/JuanBaez77/status-sv/internal/buildinfo.Version=$(git describe --tags --always) \
  -X github.com/JuanBaez77/status-sv/internal/buildinfo.Commit=$(git rev-parse HEAD)" \
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

## 3. Actualizar

**Control plane:** copiar el nuevo commit igual que en el paso 1, actualizar `STATUSSV_COMMIT` y
`STATUSSV_VERSION` en `deploy/.env`, y:

```bash
docker compose -f docker-compose.prod.yml --env-file .env build server
docker compose -f docker-compose.prod.yml --env-file .env run --rm server migrate up
docker compose -f docker-compose.prod.yml --env-file .env up -d server
```

Los agents se desconectan durante el reinicio y reconectan solos con backoff: no hace falta
tocarlos.

**Agent:** copiar el binario nuevo y volver a correr `install-agent.sh`. Reemplaza el binario y
reinicia el servicio **sin volver a enrolar**: la credencial se conserva.

## 4. Volver atrás

**Control plane:** cada versión queda como imagen `status-sv-server:<versión>`. Para volver a la
anterior, poner esa versión en `STATUSSV_VERSION` y `up -d server`. Si la versión nueva traía una
migración, antes: `run --rm server migrate down`.

**Agent:** volver a correr `install-agent.sh` con el binario anterior.

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

## 7. Lecciones del primer despliegue

Salieron al desplegar en `cristales-baez-prod` y cuestan tiempo si se redescubren:

- **Crear el registro DNS antes de consultarlo.** Si alguien pregunta por el dominio cuando todavía
  no existe, los resolvers guardan la respuesta negativa hasta el mínimo del SOA: en Cloudflare, 30
  minutos. El resolver del proveedor del VPS lo hizo, y el agent no pudo resolver el dominio hasta
  que venció. `dig @153.92.2.6 <DOMINIO> +noall +authority` muestra cuánto le falta.
- **Por SSH con un script por la entrada estándar, `docker compose run` y `exec` van con `-T` y
  `</dev/null`.** Si no, leen la entrada estándar y se tragan el resto del script, que termina en
  silencio a la mitad. Interactivamente no pasa.
- **Cloudflare responde 403 (`error code: 1000`) si el cliente manda su propio `CF-Connecting-IP`.**
  Es esperado y es una protección más: ese encabezado lo escribe solo Cloudflare. Para probar la
  falsificación de IP a través del proxy, alcanza con `X-Forwarded-For`.
