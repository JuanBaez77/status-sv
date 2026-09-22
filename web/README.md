# Panel de status-sv

React + Vite + TypeScript + Tailwind, compilado a estáticos. En producción no corre Node: el
binario del control plane embebe `dist/` (eso llega en el PR 11 del Sprint 1).

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
├── hooks/           # datos y estado compartido (sesión, más adelante métricas)
├── lib/
│   ├── api.ts       # cliente de la API del control plane
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
