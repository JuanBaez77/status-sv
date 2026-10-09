import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import type { ProposalPreview } from '@/lib/api'

/**
 * PreviewSections muestra lo que una propuesta va a escribir en el servidor:
 * la declaración, la unidad de systemd y el sitio de nginx. Es texto que se
 * lee, no se edita: lo que cambia se cambia en el formulario.
 */
export function PreviewSections({ preview, intro }: { preview: ProposalPreview; intro: string }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>Lo que se va a escribir</CardTitle>
        <CardDescription>{intro}</CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        <Block title="Declaración (project.yaml)" text={preview.yaml} />
        {preview.unit && <Block title="Unidad de systemd" text={preview.unit} />}
        {preview.site && <Block title="Sitio de nginx" text={preview.site} />}
      </CardContent>
    </Card>
  )
}

function Block({ title, text }: { title: string; text: string }) {
  return (
    <section className="flex flex-col gap-1.5">
      <h3 className="text-xs font-medium text-muted-foreground">{title}</h3>
      <pre className="max-h-96 overflow-auto rounded-md bg-muted p-3 font-mono text-xs leading-5">
        {text}
      </pre>
    </section>
  )
}
