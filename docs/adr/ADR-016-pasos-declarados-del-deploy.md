# ADR-016: Pasos declarados del deploy (`deploy.pre` y `deploy.post`)

- **Estado:** aceptada
- **Fecha:** 2026-09-30

## Contexto

[ADR-012](ADR-012-proyectos-declarados-en-el-servidor.md) dejó `deploy.pre` y `deploy.post`
obligatoriamente vacíos hasta que este sprint definiera su formato. Muchos proyectos necesitan
algo entre compilar y levantar (las migraciones de Django, por ejemplo) o después de verificar.

El diseño dice que los pasos previos y posteriores son solo los que declaró y aprobó una persona.
La regla 1 prohíbe cualquier camino por el que el control plane o un LLM hagan ejecutar texto libre.

## Decisión

**1. Un paso es un servicio de Compose y un comando como lista de argumentos.**

```yaml
deploy:
  pre:
    - service: backend
      command: ["python", "manage.py", "migrate", "--noinput"]
      timeout_seconds: 300
  post: []
```

- Se ejecuta con `docker compose run --rm --no-TTY <servicio> <argumentos...>`, con la imagen
  recién compilada. Compose levanta las dependencias del servicio (la base, por ejemplo) si hace
  falta.
- `service` sigue el formato de nombre de servicio de Compose (`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`).
- `command` tiene de 1 a 32 argumentos, cada uno de hasta 256 caracteres imprimibles, sin saltos
  de línea. Se pasa tal cual: el agent no lo interpreta.
- `timeout_seconds` va de 1 a 1800; si falta, 300.
- Como máximo 5 pasos en `pre` y 5 en `post`.
- Solo con `runtime: docker`. Los de systemd los define el Sprint 5.

**2. `pre` corre después del `build` y antes del `up`; `post`, después de la verificación.**

Si un paso falla (código de salida distinto de 0 o se pasa de su tiempo), el deploy vuelve atrás
como con cualquier otro error ([ADR-015](ADR-015-motor-de-deploy-docker.md)). Su salida viaja
enmascarada, como la de cualquier paso.

**3. Los pasos son del archivo, no del pedido.**

Ni el comando `deploy` ni la API ni (en la Fase 3) el MCP tienen un campo para agregar, quitar o
cambiar pasos. Para cambiarlos hay que editar el `project.yaml` en el servidor.

**4. El comando puede ser cualquier programa, porque lo escribe root en el servidor.**

Si alguien declara `["sh", "-c", "..."]`, es una persona con acceso de root decidiendo qué corre
en su servidor, que ya podía hacerlo sin status-sv. La regla 1 protege que el texto no venga del
control plane ni del LLM; eso lo garantiza el punto 3.

## Consecuencias

**Positivas**

- Las migraciones y tareas parecidas entran en el deploy sin abrir ningún camino de texto libre.
- Un paso corre dentro de un contenedor del proyecto, con su imagen y su entorno, no en el servidor.

**Negativas**

- **Un paso `pre` que migra la base no se deshace.** Si el deploy falla después, el código y las
  imágenes vuelven atrás pero el esquema queda migrado. Las migraciones tienen que ser compatibles
  hacia atrás (agregar antes de quitar), que es buena práctica igual. El panel lo advierte cuando
  el rollback ocurre después de un `pre`.
- Un comando declarado mal (un error de tipeo) recién se descubre al desplegar. El parser valida
  la forma, no que el programa exista.

## Alternativas consideradas

- **Un solo texto de shell por paso (`pre: "python manage.py migrate"`):** obliga a pasar por un
  intérprete. Descartado aunque lo escriba root, para que el formato no invite a encadenar comandos.
- **Correr los pasos con `docker compose exec` sobre el contenedor que ya corre:** ejecuta el
  código viejo, no el que se está desplegando. Descartado.
- **Dejar los pasos para el Sprint 5:** son la línea de corte del Sprint 4. Si no entran, el
  formato igual queda fijado acá y el parser sigue exigiéndolos vacíos.
