-- Agregado por hora de las métricas (Sprint 3). Mismas tres tablas que los
-- otros niveles: la muestra, sus discos y sus interfaces.
--
--   metrics_1h*   agregado por hora desde metrics_1m*. Partición mensual, 1 año.
--
-- La ingesta recalcula la hora de cada lote desde el agregado por minuto, en la
-- misma transacción que el minuto (ADR-009), así que nunca queda atrás. Se
-- calcula desde metrics_1m y no desde el crudo porque el crudo dura 48 horas:
-- una muestra que llega tarde siempre tiene su minuto al día, y de ahí sale la
-- hora.
--
-- De cada hora se guarda el promedio y el máximo, como en el minuto, y además
-- el mínimo de CPU y de memoria: en un rango de un año, cada punto es una hora
-- y el panel necesita poder mostrar la banda entera.

-- +goose Up

-- samples es la cantidad de muestras crudas que entraron en la hora (360 con
-- muestreo de 10 s), sumando las de cada minuto.
CREATE TABLE metrics_1h (
    server_id           uuid        NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    bucket              timestamptz NOT NULL,
    samples             integer     NOT NULL,
    cpu_used_min        real        NOT NULL,
    cpu_used_avg        real        NOT NULL,
    cpu_used_max        real        NOT NULL,
    cpu_iowait_avg      real        NOT NULL,
    cpu_iowait_max      real        NOT NULL,
    cpu_steal_avg       real        NOT NULL,
    cpu_cores           smallint    NOT NULL,
    mem_total_bytes     bigint      NOT NULL,
    mem_used_min_bytes  bigint      NOT NULL,
    mem_used_avg_bytes  bigint      NOT NULL,
    mem_used_max_bytes  bigint      NOT NULL,
    swap_total_bytes    bigint      NOT NULL,
    swap_used_avg_bytes bigint      NOT NULL,
    swap_used_max_bytes bigint      NOT NULL,
    load1_avg           real        NOT NULL,
    load5_avg           real        NOT NULL,
    load15_avg          real        NOT NULL,
    PRIMARY KEY (server_id, bucket),
    CONSTRAINT metrics_1h_samples_positive CHECK (samples > 0),
    CONSTRAINT metrics_1h_cpu_in_range CHECK (
        cpu_used_min BETWEEN 0 AND cpu_used_avg
        AND cpu_used_max BETWEEN cpu_used_avg AND 100
        AND cpu_iowait_avg BETWEEN 0 AND 100
        AND cpu_iowait_max BETWEEN cpu_iowait_avg AND 100
        AND cpu_steal_avg BETWEEN 0 AND 100
        AND cpu_cores >= 0),
    CONSTRAINT metrics_1h_memory_consistent CHECK (
        mem_used_min_bytes BETWEEN 0 AND mem_used_avg_bytes
        AND mem_used_avg_bytes <= mem_used_max_bytes
        AND mem_used_max_bytes <= mem_total_bytes
        AND swap_used_avg_bytes BETWEEN 0 AND swap_used_max_bytes
        AND swap_used_max_bytes <= swap_total_bytes),
    CONSTRAINT metrics_1h_load_not_negative CHECK (load1_avg >= 0 AND load5_avg >= 0 AND load15_avg >= 0)
) PARTITION BY RANGE (bucket);

-- Como en el minuto, de los discos se guarda el peor momento de la hora.
CREATE TABLE metrics_1h_disks (
    server_id           uuid        NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    bucket              timestamptz NOT NULL,
    mountpoint          text        NOT NULL,
    total_bytes         bigint      NOT NULL,
    used_max_bytes      bigint      NOT NULL,
    available_min_bytes bigint      NOT NULL,
    PRIMARY KEY (server_id, bucket, mountpoint),
    CONSTRAINT metrics_1h_disks_mountpoint_valid CHECK (length(mountpoint) BETWEEN 1 AND 128),
    CONSTRAINT metrics_1h_disks_space_consistent CHECK (
        used_max_bytes BETWEEN 0 AND total_bytes
        AND available_min_bytes BETWEEN 0 AND total_bytes)
) PARTITION BY RANGE (bucket);

-- La tasa promedio de la hora es el promedio de las tasas de sus minutos, y el
-- pico, el más alto de los picos por minuto.
CREATE TABLE metrics_1h_networks (
    server_id      uuid             NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    bucket         timestamptz      NOT NULL,
    interface      text             NOT NULL,
    rx_avg_per_sec double precision NOT NULL,
    rx_max_per_sec double precision NOT NULL,
    tx_avg_per_sec double precision NOT NULL,
    tx_max_per_sec double precision NOT NULL,
    PRIMARY KEY (server_id, bucket, interface),
    CONSTRAINT metrics_1h_networks_interface_valid CHECK (length(interface) BETWEEN 1 AND 128),
    CONSTRAINT metrics_1h_networks_rates_consistent CHECK (
        rx_avg_per_sec BETWEEN 0 AND rx_max_per_sec
        AND tx_avg_per_sec BETWEEN 0 AND tx_max_per_sec)
) PARTITION BY RANGE (bucket);

-- La lista cerrada de tablas particionadas suma las tres nuevas, con partición
-- mensual como el minuto.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION metrics_partition_spec(parent text, OUT unit text, OUT step interval, OUT suffix_format text)
LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN
    CASE parent
        WHEN 'metrics_raw', 'metrics_raw_disks', 'metrics_raw_networks' THEN
            unit := 'day';   step := interval '1 day';   suffix_format := 'YYYYMMDD';
        WHEN 'metrics_1m', 'metrics_1m_disks', 'metrics_1m_networks',
             'metrics_1h', 'metrics_1h_disks', 'metrics_1h_networks' THEN
            unit := 'month'; step := interval '1 month'; suffix_format := 'YYYYMM';
        ELSE
            RAISE EXCEPTION '% no es una tabla de métricas particionada', parent
                USING ERRCODE = 'invalid_parameter_value';
    END CASE;
END
$$;
-- +goose StatementEnd

-- Carga inicial: la hora de todo lo que ya hay por minuto. Las particiones las
-- crea el mantenimiento del control plane, pero para esta carga hacen falta
-- antes, así que se crean acá las que cubren los datos existentes. Llevan el
-- mismo nombre que les daría el mantenimiento, que después las encuentra y las
-- borra al vencer como a cualquier otra.
-- +goose StatementBegin
DO $$
DECLARE
    first_bucket timestamptz;
    last_bucket  timestamptz;
    parent       text;
BEGIN
    SELECT min(bucket), max(bucket) INTO first_bucket, last_bucket FROM metrics_1m;
    IF first_bucket IS NULL THEN
        RETURN;
    END IF;
    FOREACH parent IN ARRAY ARRAY['metrics_1h', 'metrics_1h_disks', 'metrics_1h_networks'] LOOP
        PERFORM metrics_ensure_partitions(parent, first_bucket, last_bucket + interval '1 hour');
    END LOOP;
END
$$;
-- +goose StatementEnd

INSERT INTO metrics_1h (
    server_id, bucket, samples, cpu_used_min, cpu_used_avg, cpu_used_max, cpu_iowait_avg, cpu_iowait_max,
    cpu_steal_avg, cpu_cores, mem_total_bytes, mem_used_min_bytes, mem_used_avg_bytes, mem_used_max_bytes,
    swap_total_bytes, swap_used_avg_bytes, swap_used_max_bytes, load1_avg, load5_avg, load15_avg
)
SELECT server_id,
       date_trunc('hour', bucket, 'UTC'),
       sum(samples),
       LEAST(min(cpu_used_avg), sum(cpu_used_avg * samples) / sum(samples))::real,
       LEAST(sum(cpu_used_avg * samples) / sum(samples), max(cpu_used_max))::real,
       max(cpu_used_max),
       LEAST(sum(cpu_iowait_avg * samples) / sum(samples), max(cpu_iowait_max))::real,
       max(cpu_iowait_max),
       (sum(cpu_steal_avg * samples) / sum(samples))::real,
       max(cpu_cores),
       max(mem_total_bytes),
       LEAST(min(mem_used_avg_bytes), round(sum(mem_used_avg_bytes::numeric * samples) / sum(samples)))::bigint,
       LEAST(round(sum(mem_used_avg_bytes::numeric * samples) / sum(samples)), max(mem_used_max_bytes))::bigint,
       max(mem_used_max_bytes),
       max(swap_total_bytes),
       LEAST(round(sum(swap_used_avg_bytes::numeric * samples) / sum(samples)), max(swap_used_max_bytes))::bigint,
       max(swap_used_max_bytes),
       (sum(load1_avg * samples) / sum(samples))::real,
       (sum(load5_avg * samples) / sum(samples))::real,
       (sum(load15_avg * samples) / sum(samples))::real
FROM metrics_1m
GROUP BY server_id, date_trunc('hour', bucket, 'UTC');

INSERT INTO metrics_1h_disks (server_id, bucket, mountpoint, total_bytes, used_max_bytes, available_min_bytes)
SELECT server_id, date_trunc('hour', bucket, 'UTC'), mountpoint,
       max(total_bytes), max(used_max_bytes), min(available_min_bytes)
FROM metrics_1m_disks
GROUP BY server_id, date_trunc('hour', bucket, 'UTC'), mountpoint;

INSERT INTO metrics_1h_networks (
    server_id, bucket, interface, rx_avg_per_sec, rx_max_per_sec, tx_avg_per_sec, tx_max_per_sec
)
SELECT server_id, date_trunc('hour', bucket, 'UTC'), interface,
       LEAST(avg(rx_avg_per_sec), max(rx_max_per_sec)), max(rx_max_per_sec),
       LEAST(avg(tx_avg_per_sec), max(tx_max_per_sec)), max(tx_max_per_sec)
FROM metrics_1m_networks
GROUP BY server_id, date_trunc('hour', bucket, 'UTC'), interface;

-- +goose Down

DROP TABLE metrics_1h_networks;
DROP TABLE metrics_1h_disks;
DROP TABLE metrics_1h;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION metrics_partition_spec(parent text, OUT unit text, OUT step interval, OUT suffix_format text)
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
