import { KeyRound, Loader2, Plus } from 'lucide-react'
import { useId, useState, type FormEvent } from 'react'

import { CommandBlock, CopyButton } from '@/components/copy-button'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { useProjects } from '@/hooks/use-projects'
import { useCreateToken, useRevokeToken, useTokens } from '@/hooks/use-tokens'
import type { CreatedToken, McpToken, McpTokenList, TokenScope } from '@/lib/api'
import { validCode } from '@/lib/deploys'
import { since } from '@/lib/format'
import {
  envCommand,
  environmentsOf,
  expiresText,
  maxNameLength,
  mcpCommand,
  requestError,
  scopeGroups,
  scopeLabel,
  statusLabel,
  tokenVariable,
  validDays,
  validName,
} from '@/lib/tokens'
import { cn } from '@/lib/utils'

/**
 * TokensPage maneja los tokens del MCP (ADR-025): los que tiene Claude Code
 * para consultar status-sv. Crear uno pide un código nuevo de la app de
 * autenticación, y su valor se muestra una sola vez.
 */
export function TokensPage() {
  const query = useTokens()
  const [creating, setCreating] = useState(false)
  const [created, setCreated] = useState<CreatedToken | null>(null)

  return (
    <div className="flex flex-col gap-6">
      <Card>
        <CardHeader className="flex flex-row flex-wrap items-start justify-between gap-3">
          <div className="flex flex-col gap-1.5">
            <CardTitle>Tokens del MCP</CardTitle>
            <CardDescription>
              Las credenciales con las que Claude Code consulta status-sv por MCP. Por ahora solo
              leen: servidores, métricas, proyectos, deploys y, si se les da el permiso, logs.
            </CardDescription>
          </div>
          {!creating && (
            <Button
              size="sm"
              onClick={() => {
                setCreated(null)
                setCreating(true)
              }}
            >
              <Plus className="size-4" aria-hidden />
              Nuevo token
            </Button>
          )}
        </CardHeader>
        {creating && query.data && (
          <CardContent>
            <NewTokenForm
              list={query.data}
              onDone={(result) => {
                setCreating(false)
                setCreated(result)
              }}
            />
          </CardContent>
        )}
      </Card>

      {created && <CreatedTokenCard created={created} onClose={() => setCreated(null)} />}

      {query.isPending && <p className="text-sm text-muted-foreground">Cargando…</p>}
      {query.error && (
        <p role="alert" className="text-sm text-destructive">
          No se pudieron traer los tokens: {query.error.message}
        </p>
      )}
      {query.data && query.data.tokens.length === 0 && !creating && (
        <p className="text-sm text-muted-foreground">Todavía no hay tokens.</p>
      )}
      {query.data && query.data.tokens.length > 0 && (
        <ul className="flex flex-col gap-3" aria-label="Tokens">
          {query.data.tokens.map((token) => (
            <li key={token.id}>
              <TokenCard token={token} />
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

/** NewTokenForm pide el nombre, los permisos, los proyectos, el vencimiento y el código. */
function NewTokenForm({ list, onDone }: { list: McpTokenList; onDone: (created: CreatedToken | null) => void }) {
  const ids = useId()
  const create = useCreateToken()
  const projects = useProjects()
  const [name, setName] = useState('')
  const [scopes, setScopes] = useState<TokenScope[]>([])
  const [restrict, setRestrict] = useState(false)
  const [projectIDs, setProjectIDs] = useState<string[]>([])
  const [days, setDays] = useState(String(list.default_days))
  const [code, setCode] = useState('')

  const environments = environmentsOf(scopes)
  // Solo se ofrecen los proyectos de los entornos elegidos: los demás no los
  // alcanzaría ningún permiso del token.
  const offered = (projects.data?.projects ?? []).filter((p) => environments.has(p.environment))
  const chosen = projectIDs.filter((id) => offered.some((p) => p.id === id))
  const daysNumber = Number(days)

  const ready =
    validName(name) &&
    scopes.length > 0 &&
    (!restrict || chosen.length > 0) &&
    validDays(daysNumber, list.max_days) &&
    validCode(code)

  function toggleScope(scope: TokenScope) {
    setScopes((current) => (current.includes(scope) ? current.filter((s) => s !== scope) : [...current, scope]))
  }

  function toggleProject(id: string) {
    setProjectIDs((current) => (current.includes(id) ? current.filter((p) => p !== id) : [...current, id]))
  }

  function submit(event: FormEvent) {
    event.preventDefault()
    if (!ready) return
    create.mutate(
      {
        name: name.trim(),
        // En el orden de la lista cerrada, como los guarda el control plane.
        scopes: list.scopes.filter((s) => scopes.includes(s)),
        project_ids: restrict ? chosen : [],
        expires_in_days: daysNumber,
        totp_code: code,
      },
      { onSuccess: (result) => onDone(result) },
    )
  }

  return (
    <form onSubmit={submit} className="flex flex-col gap-4" aria-label="Nuevo token">
      <div className="flex flex-col gap-1.5">
        <Label htmlFor={`${ids}-name`}>Nombre</Label>
        <Input
          id={`${ids}-name`}
          value={name}
          onChange={(e) => setName(e.target.value)}
          maxLength={maxNameLength}
          placeholder="claude-code en la notebook"
          autoComplete="off"
          spellCheck={false}
        />
        <p className="text-xs text-muted-foreground">
          Es lo que la auditoría muestra como quien hizo cada llamada.
        </p>
      </div>

      <fieldset className="flex flex-col gap-2">
        <legend className="mb-1 text-sm font-medium leading-none">Permisos</legend>
        <div className="flex flex-wrap gap-6 text-sm">
          {scopeGroups.map((group) => (
            <div key={group.environment} className="flex items-center gap-3">
              <span className="text-muted-foreground">{group.label}:</span>
              {group.scopes.map(({ scope, label }) => (
                <label key={scope} className="inline-flex items-center gap-1.5">
                  <input
                    type="checkbox"
                    checked={scopes.includes(scope)}
                    onChange={() => toggleScope(scope)}
                    aria-label={scopeLabel(scope)}
                  />
                  {label}
                </label>
              ))}
            </div>
          ))}
        </div>
        <p className="text-xs text-muted-foreground">
          Leer es el estado, las métricas y los deploys. Los logs van aparte: son lo más sensible
          que el MCP puede mostrar.
        </p>
      </fieldset>

      <fieldset className="flex flex-col gap-2">
        <legend className="mb-1 text-sm font-medium leading-none">Proyectos</legend>
        <div className="flex flex-wrap gap-4 text-sm">
          <label className="inline-flex items-center gap-2">
            <input type="radio" name={`${ids}-projects`} checked={!restrict} onChange={() => setRestrict(false)} />
            Todos los de esos entornos
          </label>
          <label className="inline-flex items-center gap-2">
            <input type="radio" name={`${ids}-projects`} checked={restrict} onChange={() => setRestrict(true)} />
            Solo algunos
          </label>
        </div>
        {restrict &&
          (scopes.length === 0 ? (
            <p className="text-xs text-muted-foreground">Elegí antes los permisos.</p>
          ) : offered.length === 0 ? (
            <p className="text-xs text-muted-foreground">No hay proyectos en esos entornos.</p>
          ) : (
            <ul className="flex flex-col gap-1 text-sm" aria-label="Proyectos del token">
              {offered.map((p) => (
                <li key={p.id}>
                  <label className="inline-flex items-center gap-2">
                    <input type="checkbox" checked={chosen.includes(p.id)} onChange={() => toggleProject(p.id)} />
                    {p.server_name}/{p.name}
                    <span className="text-xs text-muted-foreground">
                      {p.environment === 'production' ? 'producción' : 'staging'}
                    </span>
                  </label>
                </li>
              ))}
            </ul>
          ))}
      </fieldset>

      <div className="flex flex-wrap gap-6">
        <div className="flex flex-col gap-1.5">
          <Label htmlFor={`${ids}-days`}>Vence en (días)</Label>
          <Input
            id={`${ids}-days`}
            value={days}
            onChange={(e) => setDays(e.target.value.replace(/\D/g, '').slice(0, 2))}
            inputMode="numeric"
            className="w-24"
            aria-invalid={days !== '' && !validDays(daysNumber, list.max_days)}
          />
          <p className="text-xs text-muted-foreground">De 1 a {list.max_days}.</p>
        </div>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor={`${ids}-code`}>Código de la app de autenticación</Label>
          <Input
            id={`${ids}-code`}
            value={code}
            onChange={(e) => setCode(e.target.value.replace(/\D/g, '').slice(0, 6))}
            inputMode="numeric"
            autoComplete="one-time-code"
            className="w-32 font-mono tracking-widest"
          />
          <p className="text-xs text-muted-foreground">Uno nuevo: el que usaste para entrar no sirve.</p>
        </div>
      </div>

      {create.error && (
        <p role="alert" className="text-xs text-destructive">
          {requestError(create.error.code, create.error.message)}
        </p>
      )}

      <div className="flex gap-2">
        <Button type="submit" size="sm" disabled={!ready || create.isPending}>
          {create.isPending && <Loader2 className="size-3.5 animate-spin" aria-hidden />}
          Crear token
        </Button>
        <Button type="button" size="sm" variant="ghost" onClick={() => onDone(null)} disabled={create.isPending}>
          Cancelar
        </Button>
      </div>
    </form>
  )
}

/** CreatedTokenCard muestra el token recién creado, la única vez que se puede ver. */
function CreatedTokenCard({ created, onClose }: { created: CreatedToken; onClose: () => void }) {
  const command = mcpCommand(window.location.origin)
  return (
    <Card className="border-primary">
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <KeyRound className="size-4 text-primary" aria-hidden />
          Token creado: {created.item.name}
        </CardTitle>
        <CardDescription>
          Copialo ahora: <strong>no se vuelve a mostrar</strong>. Si se pierde, se revoca y se crea
          otro.
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        <div className="flex flex-wrap items-center gap-2 rounded-md bg-muted p-2">
          <code className="min-w-0 flex-1 break-all font-mono text-xs">{created.token}</code>
          <CopyButton text={created.token} label="Copiar el token" />
        </div>
        <ol className="flex list-decimal flex-col gap-3 pl-5 text-sm">
          <li className="flex flex-col gap-1.5">
            <span>
              Con el token copiado, guardalo en la variable{' '}
              <code className="font-mono">{tokenVariable}</code> de tu usuario. En PowerShell, lo toma
              del portapapeles y no queda en el historial:
            </span>
            <CommandBlock command={envCommand} label="Copiar el comando de la variable" />
          </li>
          <li className="flex flex-col gap-1.5">
            <span>En una terminal nueva, conectá Claude Code:</span>
            <CommandBlock command={command} label="Copiar el comando para Claude Code" />
            <span className="text-xs text-muted-foreground">
              El token no va en el comando: Claude Code lee la variable al conectarse. Queda en tu
              configuración de usuario, no en un repo; no lo pegues en un{' '}
              <code className="font-mono">.mcp.json</code> que se commitea.
            </span>
          </li>
        </ol>
        <div>
          <Button size="sm" variant="outline" onClick={onClose}>
            Ya lo copié
          </Button>
        </div>
      </CardContent>
    </Card>
  )
}

const statusTones: Record<McpToken['status'], string> = {
  active: 'bg-status-online',
  expired: 'bg-status-unknown',
  revoked: 'bg-status-offline',
}

/** TokenCard es un token de la lista, con lo que alcanza y su último uso. */
function TokenCard({ token }: { token: McpToken }) {
  const revoke = useRevokeToken()
  const [confirming, setConfirming] = useState(false)
  const usable = token.status === 'active'
  const expires = expiresText(token)

  return (
    <Card className={cn(!usable && 'opacity-75')}>
      <CardContent className="flex flex-col gap-3 pt-6">
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
          <span className="font-medium">{token.name}</span>
          <code className="font-mono text-xs text-muted-foreground">{token.prefix}…</code>
          <span className="inline-flex items-center gap-1.5 text-xs">
            <span className={cn('size-2 rounded-full', statusTones[token.status])} aria-hidden />
            {statusLabel(token.status)}
          </span>
          {expires && <span className="ml-auto text-xs text-muted-foreground">{expires}</span>}
        </div>

        <div className="flex flex-wrap gap-1.5" aria-label="Permisos">
          {token.scopes.map((scope) => (
            <span key={scope} className="rounded-md bg-muted px-2 py-0.5 text-xs">
              {scopeLabel(scope)}
            </span>
          ))}
        </div>

        <p className="text-sm">
          {token.all_projects ? (
            'Todos los proyectos de esos entornos.'
          ) : (
            <>
              Solo:{' '}
              {token.projects.map((p, i) => (
                <span key={p.id}>
                  {i > 0 && ', '}
                  {p.missing ? (
                    <span className="text-muted-foreground">un proyecto que ya no está declarado</span>
                  ) : (
                    `${p.server_name}/${p.name}`
                  )}
                </span>
              ))}
              .
            </>
          )}
        </p>

        <dl className="grid gap-x-6 gap-y-1 text-xs text-muted-foreground sm:grid-cols-3">
          <div>
            <dt className="inline">Creado: </dt>
            <dd className="inline">
              {since(token.created_at)} por {token.created_by_name}
            </dd>
          </div>
          <div>
            <dt className="inline">Último uso: </dt>
            <dd className="inline">
              {since(token.last_used_at)}
              {token.last_used_ip && ` desde ${token.last_used_ip}`}
            </dd>
          </div>
          {token.revoked_at && (
            <div>
              <dt className="inline">Revocado: </dt>
              <dd className="inline">
                {since(token.revoked_at)} por {token.revoked_by_name}
              </dd>
            </div>
          )}
        </dl>

        {token.status !== 'revoked' && (
          <div className="flex flex-wrap items-center gap-2">
            {confirming ? (
              <>
                <span className="text-sm">Deja de servir enseguida. ¿Revocarlo?</span>
                <Button
                  size="sm"
                  variant="destructive"
                  onClick={() => revoke.mutate(token.id, { onSettled: () => setConfirming(false) })}
                  disabled={revoke.isPending}
                >
                  {revoke.isPending && <Loader2 className="size-3.5 animate-spin" aria-hidden />}
                  Sí, revocar
                </Button>
                <Button size="sm" variant="ghost" onClick={() => setConfirming(false)} disabled={revoke.isPending}>
                  Cancelar
                </Button>
              </>
            ) : (
              <Button size="sm" variant="outline" onClick={() => setConfirming(true)} aria-label={`Revocar ${token.name}`}>
                Revocar
              </Button>
            )}
          </div>
        )}
        {revoke.error && (
          <p role="alert" className="text-xs text-destructive">
            {requestError(revoke.error.code, revoke.error.message)}
          </p>
        )}
      </CardContent>
    </Card>
  )
}
