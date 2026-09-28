-- Estado de runtime de cada servidor: contenedores, servicios y procesos no
-- gestionados, como los manda el agent en runtime_state (proto/README.md).
--
-- Es el último estado conocido, no una historia: cada foto reemplaza a la
-- anterior. Por eso no hay particiones ni retención.
--
--   runtime_sources  si cada fuente (docker, systemd, procesos) se pudo leer.
--   runtime_units    lo que dijo cada fuente, una fila por unidad.
--
-- Una fuente que falló deja sus unidades como estaban: el panel las muestra
-- con el aviso de la fuente, en vez de hacerlas desaparecer porque Docker no
-- contestó una vez.

-- +goose Up

-- La marca de la última foto guardada. La ingesta la avanza en la misma
-- transacción y descarta una foto más vieja, que puede llegar tarde si el
-- agent la reenvió después de un corte.
ALTER TABLE servers ADD COLUMN runtime_taken_at timestamptz;

CREATE TABLE runtime_sources (
    server_id uuid        NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    source    text        NOT NULL,
    status    text        NOT NULL,
    detail    text        NOT NULL DEFAULT '',
    -- Cuántas unidades de esta fuente quedaron afuera de la foto por los topes.
    omitted   integer     NOT NULL DEFAULT 0,
    taken_at  timestamptz NOT NULL,
    PRIMARY KEY (server_id, source),
    CONSTRAINT runtime_sources_source_valid CHECK (source IN ('docker', 'systemd', 'processes')),
    CONSTRAINT runtime_sources_status_valid CHECK (status IN ('ok', 'unavailable', 'error')),
    CONSTRAINT runtime_sources_detail_length CHECK (length(detail) <= 256),
    CONSTRAINT runtime_sources_omitted_not_negative CHECK (omitted >= 0)
);

-- Una tabla para los tres tipos, con las columnas de cada uno: los textos que
-- no aplican quedan vacíos y los números en cero, así el código no maneja
-- nulos que no dicen nada. started_at, cpu_percent y memory_bytes sí pueden
-- faltar: un contenedor detenido no tiene consumo, y eso no es lo mismo que
-- consumir cero.
CREATE TABLE runtime_units (
    server_id       uuid        NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    -- container, service o process.
    kind            text        NOT NULL,
    -- El ID del contenedor, el nombre de la unidad o el PID.
    unit_key        text        NOT NULL,
    name            text        NOT NULL,
    -- Estado del contenedor o ActiveState del servicio; los procesos, running.
    state           text        NOT NULL,
    health          text        NOT NULL DEFAULT '',
    load_state      text        NOT NULL DEFAULT '',
    sub_state       text        NOT NULL DEFAULT '',
    image           text        NOT NULL DEFAULT '',
    compose_project text        NOT NULL DEFAULT '',
    compose_service text        NOT NULL DEFAULT '',
    -- PID del proceso, o PID principal del servicio.
    pid             integer     NOT NULL DEFAULT 0,
    user_name       text        NOT NULL DEFAULT '',
    -- Ya enmascarada por el agent (ADR-011).
    command         text        NOT NULL DEFAULT '',
    started_at      timestamptz,
    cpu_percent     real,
    memory_bytes    bigint,
    seen_at         timestamptz NOT NULL,
    PRIMARY KEY (server_id, kind, unit_key),
    CONSTRAINT runtime_units_kind_valid CHECK (kind IN ('container', 'service', 'process')),
    CONSTRAINT runtime_units_key_valid CHECK (length(unit_key) BETWEEN 1 AND 256),
    CONSTRAINT runtime_units_name_valid CHECK (length(name) BETWEEN 1 AND 256),
    CONSTRAINT runtime_units_command_length CHECK (length(command) <= 256),
    CONSTRAINT runtime_units_pid_not_negative CHECK (pid >= 0),
    CONSTRAINT runtime_units_cpu_in_range CHECK (cpu_percent BETWEEN 0 AND 100),
    CONSTRAINT runtime_units_memory_not_negative CHECK (memory_bytes >= 0)
);

-- +goose Down

DROP TABLE runtime_units;
DROP TABLE runtime_sources;
ALTER TABLE servers DROP COLUMN runtime_taken_at;
