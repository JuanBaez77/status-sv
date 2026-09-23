import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'

import App from '@/App'
import { ApiError } from '@/lib/api'
import '@/index.css'

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // Las métricas llegan por WebSocket (PR 10), así que no hace falta
      // preguntar de nuevo cada vez que la ventana toma el foco.
      refetchOnWindowFocus: false,
      staleTime: 10_000,
      retry: (failureCount, error) => {
        // Un 401 o un pedido inválido no mejoran reintentando.
        if (error instanceof ApiError && error.status >= 400 && error.status < 500) {
          return false
        }
        return failureCount < 2
      },
    },
  },
})

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <App />
    </QueryClientProvider>
  </StrictMode>,
)
