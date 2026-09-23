import { useEffect, useState, useSyncExternalStore } from 'react'

import { panelSocket, type LiveSample } from '@/lib/panel-socket'

/**
 * liveBuffer es cuántas muestras en vivo se guardan: dos horas a una cada diez
 * segundos. Alcanza para el rango más largo que se dibuja en vivo y le pone un
 * techo a la memoria de una pestaña que quedó abierta toda la noche.
 */
const liveBuffer = 720

/**
 * useLiveSamples sigue a un servidor por el canal en vivo: devuelve las
 * muestras que llegaron desde que se abrió la vista y en qué estado está el
 * canal.
 *
 * Las muestras se guardan junto al servidor al que pertenecen: al cambiar de
 * servidor, lo de antes se descarta en el mismo render, sin un paso intermedio
 * en el que se verían métricas de otra máquina.
 */
export function useLiveSamples(serverID: string) {
  const [buffer, setBuffer] = useState<{ serverID: string; samples: LiveSample[] }>({
    serverID,
    samples: [],
  })
  const status = useSyncExternalStore(
    panelSocket.onStatus,
    panelSocket.getStatus,
    panelSocket.getStatus,
  )

  useEffect(
    () =>
      panelSocket.subscribe(serverID, (sample) => {
        setBuffer((previous) =>
          previous.serverID === serverID
            ? { serverID, samples: [...previous.samples, sample].slice(-liveBuffer) }
            : { serverID, samples: [sample] },
        )
      }),
    [serverID],
  )

  return { samples: buffer.serverID === serverID ? buffer.samples : [], status }
}
