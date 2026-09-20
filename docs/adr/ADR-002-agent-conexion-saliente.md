# ADR-002: El agent solo abre conexiones salientes

- **Estado:** aceptada
- **Fecha:** 2026-09-20

## Contexto

El control plane tiene que llegar a cada VPS para pedir métricas y mandar órdenes de deploy. La
forma habitual sería que el control plane se conecte a cada servidor (por SSH, o por una API que el
agent exponga en un puerto). Eso implica que cada VPS de cliente abra un puerto nuevo hacia
internet, o que el control plane tenga una llave SSH con acceso a todos los servidores.

Los VPS son de clientes distintos, con firewalls y configuraciones que no siempre controlamos, y
algunos pueden estar detrás de NAT. Además, el control plane va a recibir órdenes de un LLM: si el
control plane fuera comprometido y tuviera acceso SSH a todos los servidores, el daño sería total.

## Decisión

El agent **inicia siempre la conexión** hacia el control plane, por WebSocket seguro (WSS) sobre
el puerto 443, y mantiene ese canal abierto. Por el mismo canal suben las métricas y bajan las
órdenes.

**Ningún VPS abre un puerto nuevo, y el control plane nunca necesita alcanzar a un VPS.** El
control plane no guarda llaves SSH de los servidores administrados.

El agent se autentica en dos etapas: un token de enrolamiento de un solo uso que vence a los
15 minutos, y después una credencial propia por agent, revocable desde el panel. El objetivo de
la Fase 4 es mTLS.

## Consecuencias

**Positivas**

- Funciona detrás de NAT y con firewalls restrictivos, sin pedirle nada al cliente.
- La superficie de ataque del VPS no crece: no hay puerto nuevo escuchando.
- Un control plane comprometido no obtiene shell en ningún servidor: solo puede mandar comandos de
  la lista permitida sobre proyectos declarados.
- Un agent comprometido tiene su credencial revocable de forma individual, sin tocar a los demás.
- Detectar que un servidor se cayó es directo: el canal se corta y el latido deja de llegar.

**Negativas**

- El control plane tiene que sostener una conexión abierta por agent, con su costo de memoria.
- Hay que manejar reconexión con backoff, buffer de métricas mientras no hay canal, y comandos
  idempotentes con acuse, porque un corte puede dejar una orden a medio camino.
- No se puede "entrar" a un servidor para depurar: si el agent no conecta, hay que ir por SSH a
  mano, fuera de status-sv. Es el precio de no tener acceso permanente.

## Alternativas consideradas

- **SSH desde el control plane:** simple y conocido, pero concentra en un solo lugar llaves con
  acceso total a todos los servidores, y expone SSH a internet. Incompatible con darle acceso a un
  LLM. Descartado.
- **El agent expone una API HTTP y el control plane la consulta:** obliga a abrir un puerto en cada
  VPS, a resolver NAT y a emitir certificados por servidor. Descartado.
- **Polling del agent al control plane (HTTP cada N segundos):** no abre puertos, pero agrega
  latencia a las órdenes y gasta CPU y red en consultas vacías, en contra de la meta de bajo
  consumo y de los 3 segundos para iniciar un deploy. Descartado.
