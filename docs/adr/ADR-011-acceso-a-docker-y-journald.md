# ADR-011: Cómo el agent observa Docker y systemd

- **Estado:** aceptada
- **Fecha:** 2026-09-24

## Contexto

El Sprint 2 le pide al agent cuatro cosas nuevas: listar los contenedores de Docker con su estado,
listar los servicios de systemd, medir lo que consume cada uno y traer sus últimos renglones de
log. Hasta ahora el agent solo leía `/proc`, que no necesita permisos especiales, y su unidad de
systemd le cierra casi todo el sistema.

Las cuatro cosas chocan con el endurecimiento actual y con la regla 1 del proyecto (ninguna
operación ejecuta shell libre). Además hay dos metas que acotan las opciones: el binario del agent
tiene que quedar por debajo de 15 MB (hoy mide 7,2 MB) y el consumo en reposo por debajo de 30 MB
de RAM y 1% de CPU.

## Decisión

**1. El agent llega al socket de Docker por el grupo `docker`.**

La unidad lo suma con `SupplementaryGroups`, pero desde un *drop-in* que escribe
`deploy/install-agent.sh` solo cuando el grupo existe: un servidor sin Docker no puede tener el
grupo en la unidad principal, porque systemd no arranca un servicio cuyo grupo suplementario no
existe.

Esto es equivalente a darle root al agent, y se acepta a conciencia: el motor de deploy del
Sprint 4 necesita crear, arrancar y parar contenedores, así que cualquier barrera de solo lectura
que se ponga ahora se cae en dos sprints. La contención real es la de la regla 2: el agent solo
implementa operaciones tipadas de una lista cerrada, y en la Fase 1 esa lista es de solo lectura.

**2. El cliente de Docker es propio y mínimo.**

Tres endpoints sobre el socket unix (`GET /containers/json`, `GET /events`,
`GET /containers/{id}/logs`), hablados con `net/http` y un `DialContext` a `unix://`. No se usa el
SDK oficial `docker/docker`, que arrastra entre 8 y 10 MB de dependencias contra una meta de 15 MB
para todo el binario.

**3. El consumo de cada unidad sale de cgroup v2, no de la API de Docker.**

Una lectura de `cpu.stat` y `memory.current` por unidad, en el mismo ciclo que las métricas del
servidor. `docker stats` abre un stream por contenedor y cuesta CPU en el demonio; el árbol de
cgroups sirve además para los servicios de systemd y para atribuir los procesos, así que es un solo
mecanismo para los tres adaptadores. `ProtectControlGroups=true` deja `/sys/fs/cgroup` de solo
lectura, que es lo único que hace falta.

**4. Los servicios de systemd se leen por D-Bus, de solo lectura.**

`ListUnits` y las propiedades de cada unidad contra `org.freedesktop.systemd1`, con
`github.com/coreos/go-systemd/v22/dbus` (Go puro, sobre `godbus`). No se ejecuta `systemctl`: la
información está disponible por la interfaz sin privilegios.

**5. Los logs de journald se leen ejecutando `journalctl` con argumentos fijos.**

El agent corre `journalctl --unit <unidad> --lines <n> --output json --no-pager` con `exec.Command`,
sin intérprete de shell, con la unidad validada contra la lista de unidades que el propio agent
descubrió y `n` acotado. El agent pertenece al grupo `systemd-journal`, que da lectura del diario.

Esto **no** rompe la regla 1. La regla prohíbe que el LLM, o cualquier otro, mande texto que se
ejecute; acá el programa es fijo, los argumentos son una lista (no una cadena que alguien parsea) y
cada valor está validado. La alternativa pura en Go, `sdjournal`, exige cgo y `libsystemd`, y con
eso el binario deja de ser estático: no se puede copiar a un VPS y correrlo.

**6. Los secretos se enmascaran en el agent, antes de que el log salga del VPS.**

El filtro de la regla 3 vive en el agent, no en el control plane: lo que sale por el canal ya está
enmascarado, así que un control plane comprometido tampoco ve secretos de los proyectos.

## Consecuencias

**Positivas**

- El agent sigue siendo un binario estático único, sin dependencias en el VPS y con margen contra
  la meta de 15 MB.
- Un solo mecanismo (cgroup v2) mide contenedores, servicios y procesos, y es barato: son lecturas
  de archivos, sin polling contra ningún demonio.
- Los logs salen del VPS ya enmascarados.

**Negativas**

- **El agent tiene, de hecho, privilegios de root en cada VPS donde hay Docker.** Quien comprometa
  el binario o el canal se lleva el servidor entero. Lo que queda en pie es: la lista cerrada de
  operaciones, que en la Fase 1 es de solo lectura; la aprobación humana de la Fase 3; y la
  auditoría. En la Fase 4, mTLS y actualizaciones firmadas del agent achican la superficie.
- Ejecutar `journalctl` implica un proceso hijo por pedido de logs. Es a demanda y acotado, no un
  proceso permanente, pero hay que mantener el tiempo límite y el tope de renglones.
- La unidad de systemd queda algo menos cerrada de lo que estaba y depende de un drop-in que
  escribe el instalador. El runbook tiene que explicarlo, porque un servidor donde se instala
  Docker **después** del agent necesita correr el instalador de nuevo.
- Leer cgroup v2 deja afuera a los servidores con cgroup v1. Ubuntu 22.04 en adelante y Debian 11
  en adelante usan v2 por defecto; si aparece uno con v1, el consumo por unidad queda vacío y el
  estado se sigue viendo.

## Alternativas consideradas

- **Proxy del socket de Docker de solo lectura** (un contenedor que filtra los endpoints
  permitidos). Protege mejor mientras el agent solo lee, pero en el Sprint 4 el deploy necesita
  crear y arrancar contenedores, y abrir esos endpoints en el proxy lo vuelve equivalente al socket
  crudo. A cambio cuesta un contenedor más por VPS, en servidores de clientes. Descartado por
  ganancia corta y costo permanente.
- **SDK oficial de Docker.** Cómodo, pero son 8 a 10 MB para tres endpoints que devuelven JSON.
  Descartado por la meta de tamaño.
- **`docker stats` para el consumo.** Un stream por contenedor y trabajo en el demonio por cada uno;
  no sirve para los servicios de systemd. Descartado.
- **Ejecutar `systemctl` y parsear su salida.** Formato pensado para personas, y un proceso hijo por
  consulta. D-Bus da lo mismo tipado y sin procesos. Descartado.
- **`sdjournal` (cgo).** Rompe el binario estático. Descartado.
- **Dejar los logs de systemd fuera del Sprint 2.** El diseño los pide en este sprint y son la mitad
  del valor de la vista: sin logs, un servicio caído se ve pero no se explica. Descartado.
