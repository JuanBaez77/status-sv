-- Cómo terminó la última vez un servicio parado o caído, como lo da systemd
-- ("success", "exit-code", "oom-kill"...). Es lo que separa un servicio que
-- terminó bien de uno que se cayó. Vacío en contenedores, procesos y lo que
-- mande un agent anterior.

-- +goose Up
ALTER TABLE runtime_units ADD COLUMN result text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE runtime_units DROP COLUMN result;
