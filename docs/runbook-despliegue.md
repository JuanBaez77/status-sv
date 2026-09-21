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
dig +short <DOMINIO>    # tiene que devolver <IP>
```

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

En `deploy/.env`, completar `STATUSSV_DOMAIN` y `STATUSSV_COMMIT` (el commit de arriba), y generar
la contraseña de la base **en el servidor**:

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
docker compose -f docker-compose.prod.yml --env-file .env up -d
```

Las migraciones van en un paso aparte, antes de levantar el control plane: nunca corren solas.

### Verificar

```bash
curl -s https://<DOMINIO>/readyz              # {"status":"ready"}
curl -sI https://<DOMINIO>/healthz | head -1   # HTTP/2 200
docker compose -f docker-compose.prod.yml --env-file .env ps
```

Si el certificado no sale, mirar `docker compose ... logs caddy`: casi siempre es el DNS todavía
no propagado o el puerto 80 cerrado.

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
