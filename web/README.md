# Panel de status-sv

React + Vite + TypeScript + Tailwind, compilado a estáticos. En producción no corre Node: el
binario del control plane lleva `dist/` adentro (`make web-embed`, ver `server/internal/panel`) y lo
sirve en la raíz del dominio.

## Comandos

```bash
npm install
npm run dev         # servidor de desarrollo en http://localhost:5173
npm run lint        # oxlint
npm run typecheck   # tsc
npm run test        # vitest
npm run build       # compila a dist/
npm run size-check  # falla si dist/ pasa de 300 KB comprimido
npm run ci          # todo lo anterior, como en el CI
```

Con `npm run dev`, los pedidos a `/v1` se mandan al control plane en `127.0.0.1:8080`, incluido el
WebSocket. Así la cookie de sesión viaja como en producción, donde el panel y la API comparten
origen, y no hace falta tocar CORS.

## Cómo está organizado

```text
src/
├── components/
│   ├── ui/          # piezas de interfaz (botón, tarjeta, campo…)
│   └── app-shell.tsx
├── hooks/           # datos y estado compartido (sesión, servidores, métricas)
├── lib/
│   ├── api.ts       # cliente de la API del control plane
│   ├── panel-socket.ts  # canal en vivo, uno por pestaña
│   ├── series.ts    # de la respuesta de la API a lo que dibuja un gráfico
│   └── utils.ts
├── pages/           # una carpeta por vista (login, servidores…)
└── test/
```

## Decisiones

- **Sin token en el navegador.** La sesión viaja en una cookie `HttpOnly` que el JavaScript no
  puede leer ([ADR-010](../docs/adr/ADR-010-usuarios-y-sesiones-del-panel.md)); el cliente de la
  API solo manda `credentials: 'same-origin'`.
- **Componentes propios, al estilo de shadcn/ui.** Mismo patrón (variantes con
  `class-variance-authority`, colores por variable de tema, `cn` para juntar clases), pero escritos
  en el repo en vez de traídos con su CLI: son pocos y así no dependen de una herramienta que
  genera código. Si hace falta algo complejo (diálogos, selects accesibles), se suma el CLI.
- **Tema oscuro por defecto**, que es lo que se mira todo el día en una pantalla de monitoreo. Los
  colores son variables en `src/index.css`: cambiar el tema es tocar ese archivo.
- **Tipografía del sistema**, sin fuentes descargadas: menos peso y una dependencia externa menos.
- **`wouter` en vez de react-router**: el panel tiene pocas rutas y pesa unos pocos kilobytes.
- **uPlot para los gráficos.** Dibuja en canvas: una hora en crudo son 360 puntos por línea y
  varios gráficos en la misma pantalla, que con SVG serían miles de nodos. Pesa unos 20 KB
  comprimido y no arrastra dependencias. Como el canvas no le dice nada a un lector de pantalla,
  cada gráfico lleva en texto el último valor de cada línea.
- **Un solo WebSocket por pestaña**, en `lib/panel-socket.ts`: las vistas se suscriben a los
  servidores que están mirando. Si se corta, reintenta con esperas crecientes y se vuelve a
  suscribir solo; el panel dice si lo que se ve es de ahora.
- **Un hueco se dibuja como hueco.** Si entre dos muestras pasó más tiempo del esperado, la línea se
  corta en vez de unir los dos puntos con una recta: que el agent estuviera caído no es un dato.
- **El rango de tiempo va en la dirección** (`?rango=6h`), así un enlace lleva puesto lo que
  estaba mirando quien lo mandó.
