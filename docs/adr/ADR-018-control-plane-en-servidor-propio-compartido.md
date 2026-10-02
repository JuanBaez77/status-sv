# ADR-018: El control plane vive en un servidor propio compartido, detrás de su nginx

- **Estado:** aceptada. La mudanza se hizo el 2026-10-02 (runbook, sección 6): el control plane
  corre en `srv936263`.
- **Fecha:** 2026-10-01
- **Reemplaza a:** [ADR-007](ADR-007-control-plane-en-servidor-de-cliente.md)

## Contexto

[ADR-007](ADR-007-control-plane-en-servidor-de-cliente.md) dejó el control plane en
`cristales-baez-prod`, la producción de un cliente, y pidió mudarlo antes de habilitar herramientas
de escritura. El Sprint 4 trae la primera: el deploy
([ADR-015](ADR-015-motor-de-deploy-docker.md)), que se construyó apagado en cada agent justamente
porque la mudanza a un VPS dedicado ([ADR-014](ADR-014-control-plane-en-vps-propio.md)) estaba
postergada.

Prender el deploy en el servidor donde vive el control plane tiene un costo propio: el agent
levanta lo que diga el `docker-compose.yml` del repositorio, y un compose puede pedir
`privileged` o montar el socket de Docker. Quien pueda empujar a la rama de un proyecto de ese
servidor llegaría, por el deploy, a la base del control plane y a la conexión con todos los agents.

Al desplegar el sprint apareció una tercera opción: `srv936263`, un VPS de Juan donde ya corren
otros productos suyos. Relevado el 2026-10-01: 2 CPU, 7,8 GB de RAM con 5,3 disponibles, 76 GB
libres, nginx del host en los puertos 80 y 443, el puerto 8090 libre, y ningún contenedor que monte
el socket de Docker ni corra `privileged`.

Se evaluaron tres opciones:

- **A. Quedarse en `cristales-baez-prod`** y enmendar ADR-007: deploy prendido solo en `sv-gcg`.
- **B. Un VPS dedicado** (ADR-014).
- **C. `srv936263`**, propio pero compartido con otros productos.

## Decisión

**C, por decisión de Juan.** El control plane se muda a `srv936263` con la misma forma que tiene
hoy: en contenedores con su propia base, publicado solo en `127.0.0.1`, detrás del nginx del host
con un sitio propio y certificado de Let's Encrypt, y Cloudflare adelante con el proxy activado.
El dominio no cambia.

Con la mudanza quedan tres reglas:

1. **El deploy no se prende nunca en el servidor donde vive el control plane.** Si ese servidor
   tiene agent, es de solo lectura.
2. **El control plane no comparte nada con sus vecinos:** proyecto de Compose, red y PostgreSQL
   propios, sin puertos publicados fuera de `127.0.0.1`, y secretos con dueño root y modo 0600.
3. **Ningún contenedor del servidor monta el socket de Docker ni corre `privileged`.** Se revisa
   en el relevamiento previo a la mudanza y cada vez que se suma un producto a ese servidor.

Los servidores de clientes (`cristales-baez-prod`, `sv-gcg`) quedan solo con su agent, y en ellos
el deploy se puede prender proyecto por proyecto, como dice el runbook.

## Consecuencias

**Positivas**

- El control plane sale del servidor de un cliente: sus datos ya no viven en la máquina de un
  tercero, y actualizarlo deja de tocar el nginx de producción de ese cliente.
- Se cumple lo que ADR-007 pedía antes de la escritura, sin pagar otro VPS.
- El deploy queda habilitable en todos los servidores de clientes, sin excepciones que recordar
  más que la regla 1.

**Negativas, aceptadas a sabiendas**

- **No es un servidor dedicado.** Comparte el radio de impacto con los otros productos de esa
  máquina: un agujero en cualquiera de ellos que dé root en el host llega al control plane, y
  desde ahí a pedir deploys y rollbacks en los demás servidores. La regla 3 cierra el camino más
  directo, no todos.
- **Un monitor sigue sin poder avisar de la caída del servidor donde vive.** Si `srv936263` se
  cae, caen juntos esos productos y lo que tendría que avisarlo.
- Cada cambio del sitio de status-sv pasa por un nginx que sirve a otros productos: `nginx -t`
  antes de cada recarga, como hasta ahora.
- Los recursos se comparten. Hoy sobran: el control plane usa unos 6 MB y su base unos 40 MB.

**Lo que salió al mudarlo (2026-10-02)**

- **El Docker de ese servidor es el de snap, y el control plane corre sin `no-new-privileges`.**
  Con esa opción el contenedor no arranca: el daemon de snap tiene su propio perfil de AppArmor y
  el kernel no deja pasar al del contenedor. Juan aceptó quitarla solo ahí, con
  `deploy/docker-compose.snap.yml`. Quedan el usuario sin privilegios, ninguna capacidad, la raíz
  de solo lectura y el perfil de AppArmor de Docker: se pierde una capa de defensa, no el
  aislamiento. Las otras salidas eran peores (correr sin perfil de AppArmor) o más grandes
  (cambiar el Docker de todos los productos de ese servidor).
- **El control plane vive en `/root/status-sv`**, no en `/opt`: el daemon de snap no monta archivos
  desde ahí, y la clave de TOTP se monta como archivo.
- **Hay más vecinos de los que mostró el primer relevamiento:** el nginx de ese servidor atiende
  unos veinte nombres, varios fuera de Docker. No cambia la decisión, pero sí el peso de la primera
  consecuencia negativa.

## Cuándo revisarla

Pasar al VPS dedicado de [ADR-014](ADR-014-control-plane-en-vps-propio.md), que sigue siendo el
destino, **antes** de cualquiera de estas cosas:

- Que existan las alertas de la Fase 4.
- Que haya tokens del MCP con permiso de escritura (Fase 3).
- Que haga falta desplegar con status-sv algún proyecto de `srv936263`.

En el VPS dedicado, con el Docker común, `no-new-privileges` vuelve solo: es el compose de
producción sin el archivo de ajuste.

## Alternativas consideradas

- **A. Enmendar ADR-007:** sin mudanza, pero el control plane sigue en la máquina de un cliente y
  el deploy queda vedado justo en ese servidor. Descartada al aparecer un servidor propio.
- **B. VPS dedicado:** la más limpia. Cuesta un VPS más y su mantenimiento; queda como destino.
