-- Métricas de los servidores (ADR-008).
--
-- Dos niveles, cada uno en tres tablas: la muestra (CPU, memoria, carga), sus
-- discos y sus interfaces, una fila por disco o interfaz y por muestra.
--
--   metrics_raw*  lo que manda el agent, cada 10 s. Partición diaria, 48 h.
--   metrics_1m*   agregado por minuto. Partición mensual, 30 días.
--
-- Todas están particionadas por tiempo, en UTC, para que borrar lo vencido sea
-- soltar una partición y no un DELETE masivo. Esta migración no crea ninguna
-- partición: las crea por adelantado y las borra al vencer el control plane,
-- con las dos funciones del final.
--
-- La clave primaria de cada tabla es (servidor, instante[, disco o interfaz]):
-- el agent puede mandar un lote dos veces, y la ingesta se apoya en esa clave
-- para no duplicar nada.
--
-- Los bytes vienen del agent como enteros sin signo y se guardan en bigint: el
-- techo es 8 EiB, y la ingesta rechaza lo que no entre.

-- +goose Up

CREATE TABLE metrics_raw (
    server_id          uuid        NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    ts                 timestamptz NOT NULL,
    cpu_used_percent   real        NOT NULL,
    cpu_iowait_percent real        NOT NULL,
    cpu_steal_percent  real        NOT NULL,
    cpu_cores          smallint    NOT NULL,
    mem_total_bytes    bigint      NOT NULL,
    mem_used_bytes     bigint      NOT NULL,
    swap_total_bytes   bigint      NOT NULL,
    swap_used_bytes    bigint      NOT NULL,
    load1              real        NOT NULL,
    load5              real        NOT NULL,
    load15             real        NOT NULL,
    PRIMARY KEY (server_id, ts),
    CONSTRAINT metrics_raw_cpu_in_range CHECK (
        cpu_used_percent BETWEEN 0 AND 100
        AND cpu_iowait_percent BETWEEN 0 AND 100
        AND cpu_steal_percent BETWEEN 0 AND 100
        AND cpu_cores >= 0),
    CONSTRAINT metrics_raw_memory_consistent CHECK (
        mem_used_bytes BETWEEN 0 AND mem_total_bytes
        AND swap_used_bytes BETWEEN 0 AND swap_total_bytes),
    CONSTRAINT metrics_raw_load_not_negative CHECK (load1 >= 0 AND load5 >= 0 AND load15 >= 0)
) PARTITION BY RANGE (ts);

CREATE TABLE metrics_raw_disks (
    server_id       uuid        NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    ts              timestamptz NOT NULL,
    mountpoint      text        NOT NULL,
    total_bytes     bigint      NOT NULL,
    used_bytes      bigint      NOT NULL,
    available_bytes bigint      NOT NULL,
    PRIMARY KEY (server_id, ts, mountpoint),
    CONSTRAINT metrics_raw_disks_mountpoint_valid CHECK (length(mountpoint) BETWEEN 1 AND 128),
    -- Escrito como resta para que la suma no pueda desbordar bigint.
    CONSTRAINT metrics_raw_disks_space_consistent CHECK (
        used_bytes BETWEEN 0 AND total_bytes
        AND available_bytes BETWEEN 0 AND total_bytes - used_bytes)
) PARTITION BY RANGE (ts);

-- Los contadores de red son acumulados desde que arrancó el servidor: la tasa
-- se calcula restando muestras al agregar o al consultar.
CREATE TABLE metrics_raw_networks (
    server_id uuid        NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    ts        timestamptz NOT NULL,
    interface text        NOT NULL,
    rx_bytes  bigint      NOT NULL,
    tx_bytes  bigint      NOT NULL,
    PRIMARY KEY (server_id, ts, interface),
    CONSTRAINT metrics_raw_networks_interface_valid CHECK (length(interface) BETWEEN 1 AND 128),
    CONSTRAINT metrics_raw_networks_counters_not_negative CHECK (rx_bytes >= 0 AND tx_bytes >= 0)
) PARTITION BY RANGE (ts);

-- Un minuto de muestras. bucket es el comienzo del minuto y samples cuántas
-- muestras crudas entraron. Además del promedio se guarda el máximo de CPU y
-- de memoria: un pico de un minuto no puede desaparecer en el promedio.
CREATE TABLE metrics_1m (
    server_id          uuid        NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    bucket             timestamptz NOT NULL,
    samples            smallint    NOT NULL,
    cpu_used_avg       real        NOT NULL,
    cpu_used_max       real        NOT NULL,
    cpu_iowait_avg     real        NOT NULL,
    cpu_steal_avg      real        NOT NULL,
    cpu_cores          smallint    NOT NULL,
    mem_total_bytes    bigint      NOT NULL,
    mem_used_avg_bytes bigint      NOT NULL,
    mem_used_max_bytes bigint      NOT NULL,
    swap_total_bytes   bigint      NOT NULL,
    swap_used_avg_bytes bigint     NOT NULL,
    load1_avg          real        NOT NULL,
    load5_avg          real        NOT NULL,
    load15_avg         real        NOT NULL,
    PRIMARY KEY (server_id, bucket),
    CONSTRAINT metrics_1m_samples_positive CHECK (samples > 0),
    CONSTRAINT metrics_1m_cpu_in_range CHECK (
        cpu_used_avg BETWEEN 0 AND 100
        AND cpu_used_max BETWEEN cpu_used_avg AND 100
        AND cpu_iowait_avg BETWEEN 0 AND 100
        AND cpu_steal_avg BETWEEN 0 AND 100
        AND cpu_cores >= 0),
    CONSTRAINT metrics_1m_memory_consistent CHECK (
        mem_used_avg_bytes BETWEEN 0 AND mem_used_max_bytes
        AND mem_used_max_bytes <= mem_total_bytes
        AND swap_used_avg_bytes BETWEEN 0 AND swap_total_bytes),
    CONSTRAINT metrics_1m_load_not_negative CHECK (load1_avg >= 0 AND load5_avg >= 0 AND load15_avg >= 0)
) PARTITION BY RANGE (bucket);

-- De los discos se guarda el peor momento del minuto: lo más usado y lo menos
-- disponible. Para saber si un disco se llena, es lo que importa.
CREATE TABLE metrics_1m_disks (
    server_id           uuid        NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    bucket              timestamptz NOT NULL,
    mountpoint          text        NOT NULL,
    total_bytes         bigint      NOT NULL,
    used_max_bytes      bigint      NOT NULL,
    available_min_bytes bigint      NOT NULL,
    PRIMARY KEY (server_id, bucket, mountpoint),
    CONSTRAINT metrics_1m_disks_mountpoint_valid CHECK (length(mountpoint) BETWEEN 1 AND 128),
    CONSTRAINT metrics_1m_disks_space_consistent CHECK (
        used_max_bytes BETWEEN 0 AND total_bytes
        AND available_min_bytes BETWEEN 0 AND total_bytes)
) PARTITION BY RANGE (bucket);

-- De la red se guardan tasas, ya calculadas: bytes por segundo en promedio y
-- en el pico del minuto.
CREATE TABLE metrics_1m_networks (
    server_id      uuid             NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    bucket         timestamptz      NOT NULL,
    interface      text             NOT NULL,
    rx_avg_per_sec double precision NOT NULL,
    rx_max_per_sec double precision NOT NULL,
    tx_avg_per_sec double precision NOT NULL,
    tx_max_per_sec double precision NOT NULL,
    PRIMARY KEY (server_id, bucket, interface),
    CONSTRAINT metrics_1m_networks_interface_valid CHECK (length(interface) BETWEEN 1 AND 128),
    CONSTRAINT metrics_1m_networks_rates_consistent CHECK (
        rx_avg_per_sec BETWEEN 0 AND rx_max_per_sec
        AND tx_avg_per_sec BETWEEN 0 AND tx_max_per_sec)
) PARTITION BY RANGE (bucket);

-- metrics_partition_spec dice cómo se parte cada tabla de métricas, y es la
-- lista cerrada de tablas que las dos funciones de abajo aceptan tocar.
-- +goose StatementBegin
CREATE FUNCTION metrics_partition_spec(parent text, OUT unit text, OUT step interval, OUT suffix_format text)
LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN
    CASE parent
        WHEN 'metrics_raw', 'metrics_raw_disks', 'metrics_raw_networks' THEN
            unit := 'day';   step := interval '1 day';   suffix_format := 'YYYYMMDD';
        WHEN 'metrics_1m', 'metrics_1m_disks', 'metrics_1m_networks' THEN
            unit := 'month'; step := interval '1 month'; suffix_format := 'YYYYMM';
        ELSE
            RAISE EXCEPTION '% no es una tabla de métricas particionada', parent
                USING ERRCODE = 'invalid_parameter_value';
    END CASE;
END
$$;
-- +goose StatementEnd

-- metrics_ensure_partitions crea las particiones de parent que cubren de
-- from_ts a to_ts y que todavía no existen, y devuelve cuántas creó. Cada una
-- se llama <tabla>_p<AAAAMMDD> o <tabla>_p<AAAAMM>, según cómo se parta.
--
-- Corre con la zona horaria fija en UTC: así los cortes caen siempre a
-- medianoche UTC aunque la sesión esté en otra zona o haya cambio de horario.
-- Los nombres se escapan con %I y los límites con %L: el SQL dinámico es
-- inevitable en el DDL, pero nunca se arma con texto sin escapar.
-- +goose StatementBegin
CREATE FUNCTION metrics_ensure_partitions(parent text, from_ts timestamptz, to_ts timestamptz)
RETURNS integer
LANGUAGE plpgsql
SET TimeZone = 'UTC'
AS $$
DECLARE
    spec           record;
    range_start    timestamptz;
    partition_name text;
    created        integer := 0;
BEGIN
    spec := metrics_partition_spec(parent);

    -- Dos control planes a la vez (por ejemplo, durante un despliegue) no
    -- pueden crear la misma partición: el candado se suelta con la transacción.
    PERFORM pg_advisory_xact_lock(hashtext('metrics_partitions'));

    range_start := date_trunc(spec.unit, from_ts);
    WHILE range_start < to_ts LOOP
        partition_name := parent || '_p' || to_char(range_start, spec.suffix_format);
        IF to_regclass(partition_name) IS NULL THEN
            EXECUTE format('CREATE TABLE %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L)',
                partition_name, parent, range_start, range_start + spec.step);
            created := created + 1;
        END IF;
        range_start := range_start + spec.step;
    END LOOP;

    RETURN created;
END
$$;
-- +goose StatementEnd

-- metrics_drop_partitions borra las particiones de parent que terminan antes
-- de before_ts, y devuelve cuántas borró. Solo toca las que siguen el nombre
-- que les da metrics_ensure_partitions: una partición creada a mano no se
-- borra nunca sola.
-- +goose StatementBegin
CREATE FUNCTION metrics_drop_partitions(parent text, before_ts timestamptz)
RETURNS integer
LANGUAGE plpgsql
SET TimeZone = 'UTC'
AS $$
DECLARE
    spec        record;
    child       record;
    suffix      text;
    range_start timestamptz;
    dropped     integer := 0;
BEGIN
    spec := metrics_partition_spec(parent);
    PERFORM pg_advisory_xact_lock(hashtext('metrics_partitions'));

    FOR child IN
        SELECT c.relname
        FROM pg_inherits i
        JOIN pg_class c ON c.oid = i.inhrelid
        WHERE i.inhparent = parent::regclass
        ORDER BY c.relname
    LOOP
        -- parent sale de la lista cerrada de metrics_partition_spec, así que
        -- puede ir en la expresión regular sin escapar.
        suffix := substring(child.relname FROM '^' || parent || '_p([0-9]+)$');
        CONTINUE WHEN suffix IS NULL OR length(suffix) <> length(spec.suffix_format);

        range_start := to_timestamp(suffix, spec.suffix_format);
        IF range_start + spec.step <= before_ts THEN
            EXECUTE format('DROP TABLE %I', child.relname);
            dropped := dropped + 1;
        END IF;
    END LOOP;

    RETURN dropped;
END
$$;
-- +goose StatementEnd

-- +goose Down

DROP FUNCTION metrics_drop_partitions(text, timestamptz);
DROP FUNCTION metrics_ensure_partitions(text, timestamptz, timestamptz);
DROP FUNCTION metrics_partition_spec(text);
DROP TABLE metrics_1m_networks;
DROP TABLE metrics_1m_disks;
DROP TABLE metrics_1m;
DROP TABLE metrics_raw_networks;
DROP TABLE metrics_raw_disks;
DROP TABLE metrics_raw;
