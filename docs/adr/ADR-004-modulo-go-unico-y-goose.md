# ADR-004: Un único módulo Go y migraciones con goose

- **Estado:** aceptada
- **Fecha:** 2026-09-20

## Contexto

El repositorio es un monorepo con dos binarios de Go (agent y control plane) que comparten los
contratos de `proto/` y, más adelante, utilidades de logging y validación. Hay que decidir si cada
binario es su propio módulo o si hay uno solo, y con qué herramienta se versionan las migraciones
de PostgreSQL.

El agent tiene una meta dura de tamaño: menos de 15 MB. Existe la duda razonable de si un módulo
compartido engorda el binario.

## Decisión

**Un único módulo Go en la raíz:** `github.com/JuanBaez77/status-sv`.

El enlazador de Go incluye solo los paquetes que el binario realmente importa, así que compartir
módulo no agrega peso: el agent no va a linkear `pgx` ni el código del MCP porque nunca los
importa. La meta de 15 MB se verifica igual en CI en cada build.

Para las migraciones se usa **goose**, con los archivos SQL en `server/migrations/` **embebidos en
el binario del servidor** con `embed.FS`. Las migraciones corren con un subcomando explícito del
servidor, nunca de forma automática y silenciosa al arrancar.

Las consultas se generan con **sqlc** a partir de esos mismos archivos de esquema.

## Consecuencias

**Positivas**

- Un cambio que cruza agent, contratos y servidor entra en un solo PR, compila junto y se testea junto.
- Una sola lista de dependencias y una sola versión de Go para todo el repositorio.
- El binario del servidor lleva sus migraciones adentro: para desplegar se copia un archivo y se
  corre `server migrate up`, sin sincronizar una carpeta de SQL aparte.
- goose convive bien con sqlc, porque los dos leen el mismo SQL plano.

**Negativas**

- No se puede versionar el agent y el servidor por separado con etiquetas de módulo de Go. Se
  resuelve con versionado semántico del repositorio y negociación de versión de protocolo al
  conectar, que es lo que ya define el diseño.
- Una dependencia pesada agregada sin cuidado en el servidor no engorda el agent, pero sí hace más
  lento `go build ./...` y el CI de todos.

## Alternativas consideradas

- **Un módulo por componente (`agent/`, `server/`, `proto/`):** permite versionar por separado,
  pero obliga a usar `replace` o a publicar `proto/` para desarrollar en local, y cada cambio de
  contrato se vuelve un baile de tres PRs. Descartado.
- **golang-migrate en lugar de goose:** equivalente en lo esencial; goose gana por su integración
  más simple con `embed.FS` y por permitir migraciones escritas en Go si algún día hacen falta.
- **Migraciones automáticas al arrancar el servidor:** cómodo, pero arriesgado si arrancan dos
  instancias a la vez o si hay que revisar una migración antes de aplicarla en producción.
  Descartado: el subcomando es explícito.
