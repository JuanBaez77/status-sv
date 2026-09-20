# ADR-001: Go y PostgreSQL para agent, control plane y almacenamiento

- **Estado:** aceptada
- **Fecha:** 2026-09-20

## Contexto

status-sv tiene que correr un proceso permanente en cada VPS de clientes, donde la memoria es
escasa y no se quiere instalar un runtime ni dependencias del sistema. Al mismo tiempo, el control
plane tiene que sostener muchas conexiones WebSocket abiertas (una por agent, más las del panel) y
guardar series de tiempo que crecen todos los días.

Las metas de rendimiento del diseño son duras: menos de 30 MB de RAM y menos de 1% de CPU por
agent en reposo, menos de 100 MB en el control plane con 10 agents, y un binario de agent por
debajo de 15 MB.

El stack fuerte de quien desarrolla hoy es Python, así que la elección implica una curva de
aprendizaje que hay que asumir de forma consciente.

## Decisión

**Go** para el agent y para el control plane, **PostgreSQL** como única base de datos.

El acceso a datos es con **pgx** y consultas generadas por **sqlc**: SQL tipado, sin ORM y sin
concatenación de strings. Las métricas van en tablas **particionadas por tiempo** con retención
escalonada (crudo 48 h, un minuto 30 días, una hora un año), usando particionado nativo de
PostgreSQL, no TimescaleDB.

## Consecuencias

**Positivas**

- Un binario estático por VPS: se copia, se registra en systemd y no hay nada más que instalar.
- Consumo de memoria y CPU compatible con las metas, y medible desde el primer sprint.
- Agent y control plane comparten tipos y paquetes: el contrato se escribe una sola vez.
- El modelo de concurrencia de Go encaja con miles de conexiones abiertas sin threads pesados.
- PostgreSQL da JSONB, transacciones reales y borrado de datos viejos soltando particiones,
  que es instantáneo comparado con un DELETE masivo.

**Negativas**

- Curva de aprendizaje real: el Sprint 0 se diseñó chico justamente para amortiguarla.
- Más código explícito que en Python para lo mismo, sobre todo en manejo de errores.
- El particionado hay que administrarlo: crear particiones futuras y soltar las vencidas es un job
  propio que tenemos que escribir y monitorear.

**Pendiente de revisar**

- Si el volumen de métricas supera lo previsto, se evalúa TimescaleDB con un ADR nuevo. El criterio
  concreto: si el job de agregación tarda más que su intervalo, o si las consultas del panel sobre
  30 días pasan de 1 s.

## Alternativas consideradas

- **Python (FastAPI) en el control plane y agent en Python:** el agent no cumpliría las metas de
  memoria y obligaría a instalar un intérprete en cada VPS de cliente. Descartado.
- **Rust:** cumpliría de sobra las metas, pero la curva es mucho más empinada y el proyecto no
  necesita ese nivel de control. Descartado por costo de aprendizaje.
- **SQLite en lugar de PostgreSQL:** simple y sin contenedor extra, pero sin particionado ni
  concurrencia de escritura real, y el control plane escribe métricas de varios agents a la vez.
  Descartado.
- **Prometheus + Grafana en lugar de base propia:** resuelve las métricas, pero no el resto
  (deploys, aprobaciones, auditoría, MCP) y sumaría dos servicios más para operar. Se evaluó y se
  descartó para el MVP.
