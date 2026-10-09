import { Check, Copy } from 'lucide-react'
import { useState } from 'react'

import { Button } from '@/components/ui/button'

/**
 * CopyButton copia un texto al portapapeles: un comando para el servidor, una
 * llave pública. Si el navegador no deja, no pasa nada: el texto está a la
 * vista para copiarlo a mano.
 */
export function CopyButton({ text, label }: { text: string; label: string }) {
  const [copied, setCopied] = useState(false)

  async function copy() {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(true)
      setTimeout(() => setCopied(false), 2_000)
    } catch {
      setCopied(false)
    }
  }

  return (
    <Button type="button" variant="outline" size="sm" onClick={copy} aria-label={label}>
      {copied ? <Check className="size-3.5" aria-hidden /> : <Copy className="size-3.5" aria-hidden />}
      {copied ? 'Copiado' : 'Copiar'}
    </Button>
  )
}

/** CommandBlock es un comando para correr en el servidor, con su botón de copiar. */
export function CommandBlock({ command, label }: { command: string; label: string }) {
  return (
    <div className="flex flex-wrap items-center gap-2 rounded-md bg-muted p-2">
      <code className="min-w-0 flex-1 break-all font-mono text-xs">{command}</code>
      <CopyButton text={command} label={label} />
    </div>
  )
}
