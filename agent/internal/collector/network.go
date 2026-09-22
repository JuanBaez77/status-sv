package collector

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/JuanBaez77/status-sv/proto"
)

// Posición de los contadores en cada línea de /proc/net/dev, después de los
// dos puntos: ocho de recepción y ocho de transmisión.
const (
	netRxBytes  = 0
	netTxBytes  = 8
	netMinField = netTxBytes + 1
)

// parseNetDev lee los bytes recibidos y enviados de cada interfaz en
// /proc/net/dev, sin las que ignoredInterface deja afuera, ordenadas por nombre.
//
// Los contadores son acumulados desde que arrancó la máquina: la tasa la calcula
// el control plane restando muestras, así el agent no guarda estado.
func parseNetDev(raw string) ([]proto.NetworkSample, error) {
	var networks []proto.NetworkSample

	for line := range strings.Lines(raw) {
		// Las dos líneas de encabezado no tienen dos puntos, y un nombre de
		// interfaz no puede tenerlos. El número puede venir pegado a los dos
		// puntos ("eth0:1234"), así que se corta ahí y no por espacios.
		name, counters, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if ignoredInterface(name) {
			continue
		}

		fields := strings.Fields(counters)
		if len(fields) < netMinField {
			return nil, fmt.Errorf("la interfaz %q trae %d contadores y hacen falta al menos %d", name, len(fields), netMinField)
		}
		rx, err := strconv.ParseUint(fields[netRxBytes], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bytes recibidos de %q: %w", name, err)
		}
		tx, err := strconv.ParseUint(fields[netTxBytes], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bytes enviados de %q: %w", name, err)
		}

		networks = append(networks, proto.NetworkSample{Interface: name, RxBytes: rx, TxBytes: tx})
	}

	slices.SortFunc(networks, func(a, b proto.NetworkSample) int {
		return strings.Compare(a.Interface, b.Interface)
	})
	return networks, nil
}

// ignoredInterface dice qué interfaces no se informan.
//
// La de loopback es tráfico de la máquina consigo misma. Las de Docker
// (docker0, una br-* por red y un veth* por contenedor) duplicarían el tráfico:
// lo que los contenedores mandan hacia afuera ya pasa por la interfaz del
// servidor. Además los veth aparecen y desaparecen con cada contenedor.
func ignoredInterface(name string) bool {
	return name == "lo" ||
		strings.HasPrefix(name, "veth") ||
		strings.HasPrefix(name, "docker") ||
		strings.HasPrefix(name, "br-")
}
