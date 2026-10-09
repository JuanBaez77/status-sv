import { ArrowLeft } from 'lucide-react'
import { useState, type FormEvent, type ReactNode } from 'react'
import { Link, useLocation } from 'wouter'

import { PreviewSections } from '@/components/proposal-preview'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { useCreateProposal, usePreviewProposal } from '@/hooks/use-proposals'
import { useServers } from '@/hooks/use-servers'
import type { ProposalRequest } from '@/lib/api'
import { emptyForm, FormError, toRequest, type ProposalForm } from '@/lib/proposals'
import { cn } from '@/lib/utils'

const selectClass =
  'h-9 w-full rounded-md border border-input bg-background px-2 text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring'

const textareaClass =
  'min-h-20 w-full rounded-md border border-input bg-transparent px-3 py-2 font-mono text-xs outline-none placeholder:text-muted-foreground focus-visible:ring-2 focus-visible:ring-ring'

/**
 * NewProjectPage propone un proyecto nuevo en un servidor (ADR-022). El
 * control plane no lo declara: arma la declaración y se la manda al agent, y
 * una persona la acepta en el servidor con un comando. Antes de proponer se
 * ve lo que se va a escribir.
 */
export function NewProjectPage({ serverID }: { serverID: string }) {
  const servers = useServers()
  const server = servers.data?.find((s) => s.id === serverID)
  const [, navigate] = useLocation()
  const [form, setForm] = useState<ProposalForm>(emptyForm)
  const [fieldError, setFieldError] = useState<FormError | null>(null)
  // previewed es el pedido que se previsualizó: si el formulario cambia, hay
  // que volver a mirar antes de proponer.
  const [previewed, setPreviewed] = useState<string | null>(null)
  const preview = usePreviewProposal(serverID)
  const create = useCreateProposal(serverID)

  function change(patch: Partial<ProposalForm>) {
    setForm((current) => ({ ...current, ...patch }))
    setFieldError(null)
    setPreviewed(null)
  }

  function request(): ProposalRequest | null {
    try {
      const req = toRequest(form)
      setFieldError(null)
      return req
    } catch (err) {
      if (err instanceof FormError) {
        setFieldError(err)
        return null
      }
      throw err
    }
  }

  function showPreview(event: FormEvent) {
    event.preventDefault()
    const req = request()
    if (!req) return
    create.reset()
    preview.mutate(req, { onSuccess: () => setPreviewed(JSON.stringify(req)) })
  }

  function propose() {
    const req = request()
    if (!req || JSON.stringify(req) !== previewed) return
    create.mutate(req, { onSuccess: (created) => navigate(`/proposals/${created.id}`) })
  }

  if (servers.isPending) {
    return <p className="text-sm text-muted-foreground">Cargando el servidor…</p>
  }
  if (server === undefined) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>No existe ese servidor</CardTitle>
          <CardDescription>{servers.error?.message ?? 'Puede que el enlace esté mal.'}</CardDescription>
        </CardHeader>
      </Card>
    )
  }

  const errorFor = (field: keyof ProposalForm) => (fieldError?.field === field ? fieldError.message : undefined)
  const apiError = preview.error ?? create.error
  const ready = previewed !== null && preview.data !== undefined

  return (
    <section className="flex flex-col gap-4">
      <Link
        href={`/servers/${serverID}`}
        className="inline-flex w-fit items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
      >
        <ArrowLeft className="size-3" aria-hidden />
        {server.name}
      </Link>
      <header className="flex flex-col gap-1">
        <h1 className="text-lg font-medium">Nuevo proyecto en {server.name}</h1>
        <p className="max-w-prose text-xs text-muted-foreground">
          El panel no declara el proyecto: lo propone. Nada cambia en el servidor hasta que alguien
          lo acepta ahí con un comando, que muestra lo que va a hacer, pide los valores de las
          variables y crea el usuario, el directorio, la deploy key, la unidad y el sitio.
        </p>
      </header>

      <form onSubmit={showPreview} noValidate>
        <Card>
          <CardHeader>
            <CardTitle>Declaración</CardTitle>
            <CardDescription>
              El directorio es siempre <code className="font-mono">/srv/&lt;nombre&gt;</code>; con
              systemd, el usuario se llama como el proyecto y las variables van en{' '}
              <code className="font-mono">/etc/status-sv/env/&lt;nombre&gt;.env</code>.
            </CardDescription>
          </CardHeader>
          <CardContent className="grid gap-4 sm:grid-cols-2">
            <Field id="np-name" label="Nombre" error={errorFor('name')} hint="Minúsculas, números y guiones. Con systemd empieza con letra.">
              <Input
                id="np-name"
                value={form.name}
                onChange={(e) => change({ name: e.target.value })}
                autoComplete="off"
                spellCheck={false}
                aria-invalid={errorFor('name') !== undefined}
              />
            </Field>
            <Field id="np-environment" label="Entorno">
              <select
                id="np-environment"
                className={selectClass}
                value={form.environment}
                onChange={(e) => change({ environment: e.target.value as ProposalForm['environment'] })}
              >
                <option value="staging">Staging</option>
                <option value="production">Producción</option>
              </select>
            </Field>
            <Field
              id="np-repo"
              label="Repositorio"
              error={errorFor('repo')}
              hint="github.com/dueño/repo o git@github.com:dueño/repo.git, sin credenciales: para uno privado, al aceptar se crea una deploy key."
            >
              <Input
                id="np-repo"
                value={form.repo}
                onChange={(e) => change({ repo: e.target.value })}
                autoComplete="off"
                spellCheck={false}
                aria-invalid={errorFor('repo') !== undefined}
              />
            </Field>
            <Field id="np-branch" label="Rama" error={errorFor('branch')}>
              <Input
                id="np-branch"
                value={form.branch}
                onChange={(e) => change({ branch: e.target.value })}
                autoComplete="off"
                spellCheck={false}
              />
            </Field>

            <fieldset className="flex flex-col gap-1.5 sm:col-span-2">
              <legend className="mb-1.5 text-sm font-medium leading-none">Cómo corre</legend>
              <div className="flex flex-wrap gap-4 text-sm">
                <label className="inline-flex items-center gap-2">
                  <input
                    type="radio"
                    name="np-runtime"
                    checked={form.runtime === 'systemd'}
                    onChange={() => change({ runtime: 'systemd' })}
                  />
                  Un servicio de systemd
                </label>
                <label className="inline-flex items-center gap-2">
                  <input
                    type="radio"
                    name="np-runtime"
                    checked={form.runtime === 'docker'}
                    onChange={() => change({ runtime: 'docker' })}
                  />
                  Docker Compose
                </label>
              </div>
            </fieldset>

            {form.runtime === 'docker' ? (
              <Field id="np-compose" label="Archivo de Compose" hint="Relativo al directorio del proyecto.">
                <Input
                  id="np-compose"
                  value={form.composeFile}
                  onChange={(e) => change({ composeFile: e.target.value })}
                  autoComplete="off"
                  spellCheck={false}
                />
              </Field>
            ) : (
              <Field
                id="np-command"
                label="Comando del servicio"
                error={errorFor('command')}
                hint='Relativo al directorio del proyecto o absoluto. No pasa por un shell: un argumento con espacios va entre comillas dobles.'
                wide
              >
                <Input
                  id="np-command"
                  value={form.command}
                  onChange={(e) => change({ command: e.target.value })}
                  placeholder=".venv/bin/gunicorn app:app --bind 127.0.0.1:8000"
                  autoComplete="off"
                  spellCheck={false}
                  className="font-mono text-xs"
                  aria-invalid={errorFor('command') !== undefined}
                />
              </Field>
            )}

            <Field
              id="np-env"
              label="Variables requeridas"
              error={errorFor('requiredEnv')}
              hint="Solo los nombres, separados por coma. Los valores se cargan en el servidor al aceptar y nunca pasan por el panel."
              wide
            >
              <Input
                id="np-env"
                value={form.requiredEnv}
                onChange={(e) => change({ requiredEnv: e.target.value })}
                placeholder="SECRET_KEY, DATABASE_URL"
                autoComplete="off"
                spellCheck={false}
                className="font-mono text-xs"
                aria-invalid={errorFor('requiredEnv') !== undefined}
              />
            </Field>

            <Field
              id="np-domain"
              label="Dominio del sitio (opcional)"
              error={errorFor('siteDomain')}
              hint="nginx atiende el dominio y le pasa los pedidos al proyecto, con su certificado."
            >
              <Input
                id="np-domain"
                value={form.siteDomain}
                onChange={(e) => change({ siteDomain: e.target.value })}
                placeholder="app.ejemplo.com"
                autoComplete="off"
                spellCheck={false}
                aria-invalid={errorFor('siteDomain') !== undefined}
              />
            </Field>
            <Field id="np-port" label="Puerto del proyecto" error={errorFor('sitePort')} hint="En 127.0.0.1, entre 1024 y 65535.">
              <Input
                id="np-port"
                value={form.sitePort}
                onChange={(e) => change({ sitePort: e.target.value })}
                inputMode="numeric"
                autoComplete="off"
                aria-invalid={errorFor('sitePort') !== undefined}
              />
            </Field>

            <Field id="np-health" label="Healthcheck (opcional)" hint="Una URL que el agent prueba desde el servidor." wide>
              <Input
                id="np-health"
                value={form.healthcheckURL}
                onChange={(e) => change({ healthcheckURL: e.target.value })}
                placeholder="http://127.0.0.1:8000/health"
                autoComplete="off"
                spellCheck={false}
              />
            </Field>

            {form.runtime === 'systemd' && (
              <Field
                id="np-build"
                label="Preparar (opcional)"
                error={errorFor('build')}
                hint="Un paso por renglón; corren en el directorio del proyecto antes de arrancar."
                wide
              >
                <textarea
                  id="np-build"
                  className={textareaClass}
                  value={form.build}
                  onChange={(e) => change({ build: e.target.value })}
                  placeholder={'/usr/bin/python3 -m venv .venv\n.venv/bin/pip install -r requirements.txt'}
                  spellCheck={false}
                  aria-invalid={errorFor('build') !== undefined}
                />
              </Field>
            )}
            <Field
              id="np-pre"
              label="Pasos previos (opcional)"
              error={errorFor('pre')}
              hint={
                form.runtime === 'docker'
                  ? 'Un paso por renglón, "servicio: comando"; corren antes de levantar, por ejemplo las migraciones.'
                  : 'Un paso por renglón; corren antes de reiniciar, por ejemplo las migraciones.'
              }
              wide
            >
              <textarea
                id="np-pre"
                className={textareaClass}
                value={form.pre}
                onChange={(e) => change({ pre: e.target.value })}
                spellCheck={false}
                aria-invalid={errorFor('pre') !== undefined}
              />
            </Field>
            <Field
              id="np-post"
              label="Pasos posteriores (opcional)"
              error={errorFor('post')}
              hint="Un paso por renglón; corren después del healthcheck."
              wide
            >
              <textarea
                id="np-post"
                className={textareaClass}
                value={form.post}
                onChange={(e) => change({ post: e.target.value })}
                spellCheck={false}
                aria-invalid={errorFor('post') !== undefined}
              />
            </Field>

            {apiError && (
              <p role="alert" className="text-sm text-destructive sm:col-span-2">
                {apiError.message}
              </p>
            )}
            <div className="flex flex-wrap gap-2 sm:col-span-2">
              <Button type="submit" variant={ready ? 'outline' : 'default'} disabled={preview.isPending}>
                {preview.isPending ? 'Armando…' : 'Ver lo que se va a escribir'}
              </Button>
              {ready && (
                <Button type="button" onClick={propose} disabled={create.isPending}>
                  {create.isPending ? 'Proponiendo…' : 'Proponer'}
                </Button>
              )}
            </div>
          </CardContent>
        </Card>
      </form>

      {ready && preview.data && (
        <PreviewSections
          preview={preview.data}
          intro={`Así queda en ${server.name} cuando alguien la acepte. Si el servidor tiene plantillas propias, el comando de aceptación muestra las suyas.`}
        />
      )}
    </section>
  )
}

function Field({
  id,
  label,
  hint,
  error,
  wide,
  children,
}: {
  id: string
  label: string
  hint?: string
  error?: string
  wide?: boolean
  children: ReactNode
}) {
  return (
    <div className={cn('flex flex-col gap-1.5', wide && 'sm:col-span-2')}>
      <Label htmlFor={id}>{label}</Label>
      {children}
      {error ? (
        <p role="alert" className="text-xs text-destructive">
          {error}
        </p>
      ) : (
        hint && <p className="text-xs text-muted-foreground">{hint}</p>
      )}
    </div>
  )
}
