import {
  Alert,
  AlertDescription,
  AlertTitle,
  Badge,
  Button,
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
  CopyField,
  Input,
  Label,
  PageHeader,
  RelativeTime,
  Spinner,
  StatusDot,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@skinloom/ui'
import { confirm, defineWebPlugin, navigate, toast, usePluginApi, useSession, useT } from '@skinloom/plugin-sdk/web'
import { type FormEvent, useState } from 'react'

interface ServerEntry {
  code: string
  bs_root: string
  name?: string
}

interface HubProfileNode {
  internal_id?: string | number
  uuid?: string
  mapped_uuid?: string
  name?: string
  bind_mapped_name?: string
  bind_status?: number
  backend?: string
  backend_scopes?: { self?: string }
  bind?: HubProfileNode[]
}

interface UnionProfileOverview {
  playerId: number
  name: string
  uuid: string
  self?: HubProfileNode
  dupName: HubProfileNode[]
  error?: string
}

interface UserOverviewResponse {
  configured: boolean
  introMarkdown: string
  securityLevel: number | null
  profiles: UnionProfileOverview[]
  servers: ServerEntry[]
}

interface OutboxItem {
  id: number
  action: string
  uuid: string
  name: string
  attempts: number
  nextRetryAt: number
  lastError: string
  createdAt: number
}

interface AdminStatusResponse {
  wireVersion: string
  unionApiRoot: string
  siteUrl: string
  memberKeyConfigured: boolean
  local: {
    serverListVersion: number
    privateKeyVersion: number
    yggdrasilPubKeyFp: string
  }
  hub: {
    serverListVersion: number | null
    privateKeyVersion: number | null
    adminNotice: string
    error: string
  }
  checks: {
    keepUuidOnRename: boolean
    uuidAlgorithm: string
    requireEmailVerify: boolean
    hasYggdrasilKeyWrite: boolean
    hasPlayersUuidWrite: boolean
    allowRemoteKeySync: boolean
    allowRemoteUuidRemap: boolean
    allowRemoteBackendKey: boolean
    enableUnionOAuth2: boolean
    enableLocalBlacklist: boolean
    invitationCodesForUnion: boolean
  }
  oauth2: {
    enabled: boolean
    publicKeySha256Fp: string
  }
  servers: ServerEntry[]
  outbox: OutboxItem[]
}

interface DiagnoseResult {
  status: 'ok' | 'error'
  data: {
    healthy?: boolean
    delay?: number
    timestamp?: string
    tls_handshake?: string
    exception?: string
    status_code?: number
    body?: string
  }
}

interface BlacklistEntry {
  id: number
  email: string
  source?: string
  reason?: string
  created_at?: string
  valid_until?: string | null
}

interface BlacklistPageResponse {
  current_page?: number
  last_page?: number
  data?: BlacklistEntry[]
}

function normUuid(v?: string | null): string {
  return (v ?? '').replace(/-/g, '').toLowerCase()
}

function renderInlineMarkdown(line: string) {
  const parts: Array<string | { kind: 'link'; text: string; href: string } | { kind: 'code'; text: string } | { kind: 'strong'; text: string }> = []
  const re = /\[([^\]]+)\]\((https:\/\/[^)\s]+)\)|`([^`]+)`|\*\*([^*]+)\*\*/g
  let last = 0
  let m: RegExpExecArray | null
  while ((m = re.exec(line)) !== null) {
    if (m.index > last) parts.push(line.slice(last, m.index))
    if (m[1] && m[2]) parts.push({ kind: 'link', text: m[1], href: m[2] })
    else if (m[3]) parts.push({ kind: 'code', text: m[3] })
    else if (m[4]) parts.push({ kind: 'strong', text: m[4] })
    last = re.lastIndex
  }
  if (last < line.length) parts.push(line.slice(last))
  return parts.map((p, idx) => {
    if (typeof p === 'string') return <span key={idx}>{p}</span>
    if (p.kind === 'link') {
      return (
        <a key={idx} href={p.href} target="_blank" rel="noreferrer noopener" className="break-all text-primary underline underline-offset-2">
          {p.text}
        </a>
      )
    }
    if (p.kind === 'code') {
      return <code key={idx} className="break-words rounded bg-muted px-1 py-0.5 font-mono text-xs">{p.text}</code>
    }
    return <strong key={idx}>{p.text}</strong>
  })
}

function SafeMarkdown({ text }: { text: string }) {
  const lines = text.replace(/\r\n/g, '\n').split('\n')
  return (
    <div className="flex flex-col gap-1.5 break-words text-sm leading-relaxed">
      {lines.map((raw, idx) => {
        const trimmed = raw.trim()
        if (!trimmed) return <div key={idx} className="h-1" />
        if (trimmed.startsWith('### ')) return <h4 key={idx} className="font-semibold">{renderInlineMarkdown(trimmed.slice(4))}</h4>
        if (trimmed.startsWith('## ')) return <h3 key={idx} className="text-base font-semibold">{renderInlineMarkdown(trimmed.slice(3))}</h3>
        if (trimmed.startsWith('# ')) return <h2 key={idx} className="text-base font-semibold">{renderInlineMarkdown(trimmed.slice(2))}</h2>
        if (trimmed.startsWith('- ') || trimmed.startsWith('* ')) {
          return <p key={idx} className="pl-3">• {renderInlineMarkdown(trimmed.slice(2))}</p>
        }
        return <p key={idx}>{renderInlineMarkdown(trimmed)}</p>
      })}
    </div>
  )
}

function securityBadgeVariant(level: number | null): 'success' | 'warning' | 'destructive' | 'secondary' {
  if (level === 3) return 'success'
  if (level === 1) return 'warning'
  if (level === 0) return 'destructive'
  return 'secondary'
}

function ServersTable({ servers }: { servers: ServerEntry[] }) {
  const t = useT()
  if (servers.length === 0) {
    return <p className="text-sm text-muted-foreground">{t('serversEmpty')}</p>
  }
  return (
    <div className="overflow-x-auto">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>{t('colBackendCode')}</TableHead>
            <TableHead>{t('colServerName')}</TableHead>
            <TableHead>{t('colBackendRoot')}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {servers.map((s) => (
            <TableRow key={`${s.code}-${s.bs_root}`}>
              <TableCell className="font-mono text-xs font-medium">{s.code}</TableCell>
              <TableCell className="text-sm">{s.name || s.code}</TableCell>
              <TableCell className="font-mono text-xs break-all">
                {s.bs_root.startsWith('https://') || s.bs_root.startsWith('http://') ? (
                  <a href={s.bs_root} target="_blank" rel="noreferrer noopener" className="text-primary hover:underline">
                    {s.bs_root}
                  </a>
                ) : (
                  s.bs_root
                )}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  )
}

function UnionUserPage() {
  const t = useT()
  const api = usePluginApi()
  const session = useSession()
  const [tokenInputs, setTokenInputs] = useState<Record<string, string>>({})
  const [issuedTokens, setIssuedTokens] = useState<Record<string, string>>({})
  const [busyUuid, setBusyUuid] = useState<string | null>(null)

  const overviewQuery = api.useQuery<UserOverviewResponse>('/user/overview', {
    enabled: Boolean(session),
  })

  if (!session) {
    return (
      <div className="flex flex-col gap-6">
        <PageHeader title={t('userTitle')} description={t('userSubtitle')} />
        <Alert>
          <AlertDescription className="flex flex-wrap items-center justify-between gap-3">
            <span>{t('loginRequired')}</span>
            <Button size="sm" className="pointer-coarse:h-10" onClick={() => navigate('/auth/login')}>
              {t('btnSignIn')}
            </Button>
          </AlertDescription>
        </Alert>
      </div>
    )
  }

  const data = overviewQuery.data

  const handleFetchToken = async (uuid: string) => {
    setBusyUuid(uuid)
    try {
      const res = await api.post<{ token?: string }>('/user/bind', { uuid })
      if (res.token) {
        setIssuedTokens((prev) => ({ ...prev, [uuid]: res.token! }))
        toast.success(t('toastBindTokenReady'))
      }
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err))
    } finally {
      setBusyUuid(null)
    }
  }

  const handleBindTo = async (uuid: string) => {
    const token = (tokenInputs[uuid] ?? '').trim()
    if (!token) return
    setBusyUuid(uuid)
    try {
      await api.post('/user/bindto', { uuid, token })
      toast.success(t('toastBindSuccess'))
      setTokenInputs((prev) => ({ ...prev, [uuid]: '' }))
      void overviewQuery.refetch()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err))
    } finally {
      setBusyUuid(null)
    }
  }

  const handleUnbind = async (uuid: string) => {
    const ok = await confirm({
      title: t('confirmUnbindTitle'),
      description: t('confirmUnbindDesc'),
      confirmLabel: t('btnUnbind'),
      destructive: true,
    })
    if (!ok) return
    setBusyUuid(uuid)
    try {
      await api.post('/user/unbind', { uuid })
      toast.success(t('toastUnbindSuccess'))
      void overviewQuery.refetch()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err))
    } finally {
      setBusyUuid(null)
    }
  }

  const handleRemapUuid = async (me: string, target: string) => {
    const ok = await confirm({
      title: t('confirmRemapTitle'),
      description: t('confirmRemapDesc'),
      confirmLabel: t('btnRemapUuid'),
      destructive: true,
    })
    if (!ok) return
    setBusyUuid(me)
    try {
      await api.post('/user/remapuuid', { me, target })
      toast.success(t('toastRemapSuccess'))
      void overviewQuery.refetch()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err))
    } finally {
      setBusyUuid(null)
    }
  }

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title={t('userTitle')} description={t('userSubtitle')} />

      {overviewQuery.isLoading ? (
        <div className="flex items-center justify-center py-12">
          <Spinner />
        </div>
      ) : overviewQuery.isError ? (
        <Alert variant="destructive">
          <AlertTitle>{t('userTitle')}</AlertTitle>
          <AlertDescription className="flex flex-wrap items-center justify-between gap-3">
            <span>{overviewQuery.error instanceof Error ? overviewQuery.error.message : String(overviewQuery.error)}</span>
            <Button size="sm" variant="outline" className="pointer-coarse:h-10" onClick={() => void overviewQuery.refetch()}>
              {t('retry')}
            </Button>
          </AlertDescription>
        </Alert>
      ) : !data ? null : (
        <>
          {!data.configured && (
            <Alert>
              <AlertDescription>{t('notConfiguredUser')}</AlertDescription>
            </Alert>
          )}

          <div className="grid gap-4 md:grid-cols-2">
            <Card>
              <CardHeader>
                <CardTitle>{t('securityLevelTitle')}</CardTitle>
              </CardHeader>
              <CardContent className="flex flex-col gap-3">
                <div className="flex items-center gap-2">
                  <span className="text-sm font-medium">{t('securityLevelLabel')}:</span>
                  <Badge variant={securityBadgeVariant(data.securityLevel)}>
                    {data.securityLevel !== null ? `SL${data.securityLevel}` : t('securityLevelUnknown')}
                  </Badge>
                </div>
                <div className="flex flex-col gap-1.5 text-xs text-muted-foreground">
                  <div className="flex items-start gap-2">
                    <Badge variant="success">SL3</Badge>
                    <span>{t('sl3Desc')}</span>
                  </div>
                  <div className="flex items-start gap-2">
                    <Badge variant="warning">SL1</Badge>
                    <span>{t('sl1Desc')}</span>
                  </div>
                  <div className="flex items-start gap-2">
                    <Badge variant="destructive">SL0</Badge>
                    <span>{t('sl0Desc')}</span>
                  </div>
                </div>
              </CardContent>
            </Card>

            {data.introMarkdown && (
              <Card>
                <CardHeader>
                  <CardTitle>{t('introTitle')}</CardTitle>
                </CardHeader>
                <CardContent className="text-sm">
                  <SafeMarkdown text={data.introMarkdown} />
                </CardContent>
              </Card>
            )}
          </div>

          {data.profiles.length === 0 ? (
            <Alert>
              <AlertDescription>{t('playersEmpty')}</AlertDescription>
            </Alert>
          ) : (
            <div className="flex flex-col gap-4">
              {data.profiles.map((pl) => {
                const self = pl.self
                const binds = self?.bind ?? []
                const isBound = (self?.bind_status ?? 0) > 0
                const isBusy = busyUuid !== null
                const showSelfRemap =
                  Boolean(self?.mapped_uuid) &&
                  (binds.length > 0 || normUuid(self?.mapped_uuid) !== normUuid(self?.uuid || pl.uuid))
                return (
                  <Card key={pl.playerId}>
                    <CardHeader>
                      <CardTitle className="break-words">{t('playerCardTitle', { name: pl.name })}</CardTitle>
                      <CardDescription className="font-mono text-xs break-all">{pl.uuid}</CardDescription>
                    </CardHeader>
                    <CardContent className="flex flex-col gap-4">
                      {pl.error && (
                        <Alert variant="destructive">
                          <AlertDescription className="break-words">{t('playerHubError', { error: pl.error })}</AlertDescription>
                        </Alert>
                      )}

                      {self && (
                        <div className="overflow-x-auto">
                          <Table>
                            <TableHeader>
                              <TableRow>
                                <TableHead>{t('colMappedName')}</TableHead>
                                <TableHead>{t('colBackendCode')}</TableHead>
                                <TableHead>{t('colUuid')}</TableHead>
                                <TableHead>{t('colRoleInfo')}</TableHead>
                                <TableHead className="text-right">{t('colActions')}</TableHead>
                              </TableRow>
                            </TableHeader>
                            <TableBody>
                              {/* MUA Union Hub wire format: on `self`, `mapped_uuid` is the mapped UUID and `uuid` is local;
                                  on `bind[]` items, Hub returns `uuid` as the active/mapped UUID and `mapped_uuid` as the peer site's unmapped UUID. */}
                              <TableRow>
                                <TableCell className="font-medium">{self.bind_mapped_name || pl.name}</TableCell>
                                <TableCell className="font-mono text-xs">{self.backend_scopes?.self || '—'}</TableCell>
                                <TableCell className="font-mono text-xs">
                                  <div>{t('uuidMapped')}: {self.mapped_uuid || '—'}</div>
                                  <div className="text-muted-foreground">{t('uuidUnmapped')}: {self.uuid || pl.uuid}</div>
                                </TableCell>
                                <TableCell>
                                  <Badge variant="secondary">{t('yourself')}</Badge>
                                </TableCell>
                                <TableCell className="text-right">
                                  {showSelfRemap && self.mapped_uuid && (
                                    <Button
                                      size="sm"
                                      variant="outline"
                                      className="pointer-coarse:h-10"
                                      disabled={isBusy}
                                      onClick={() => void handleRemapUuid(pl.uuid, self.mapped_uuid!)}
                                    >
                                      {t('btnRemapUuid')}
                                    </Button>
                                  )}
                                </TableCell>
                              </TableRow>
                              {binds.map((b, idx) => (
                                <TableRow key={`${b.internal_id ?? idx}`}>
                                  <TableCell className="font-medium">{b.bind_mapped_name || b.name || '—'}</TableCell>
                                  <TableCell className="font-mono text-xs">{b.backend_scopes?.self || '—'}</TableCell>
                                  <TableCell className="font-mono text-xs">
                                    <div className={b.bind_status === 0 ? 'font-medium' : undefined}>
                                      {b.bind_status === 0 ? t('uuidUsing') : t('uuidMapped')}: {b.uuid || '—'}
                                    </div>
                                    <div className="text-muted-foreground">{t('uuidUnmapped')}: {b.mapped_uuid || '—'}</div>
                                  </TableCell>
                                  <TableCell>
                                    {b.bind_status === 0 ? <Badge variant="default">{t('bindMaster')}</Badge> : null}
                                  </TableCell>
                                  <TableCell className="text-right">
                                    {b.mapped_uuid && (
                                      <Button
                                        size="sm"
                                        variant="outline"
                                        className="pointer-coarse:h-10"
                                        disabled={isBusy}
                                        onClick={() => void handleRemapUuid(pl.uuid, b.mapped_uuid!)}
                                      >
                                        {t('btnRemapUuid')}
                                      </Button>
                                    )}
                                  </TableCell>
                                </TableRow>
                              ))}
                            </TableBody>
                          </Table>
                        </div>
                      )}

                      {pl.dupName.length > 0 && (
                        <div className="flex flex-col gap-2">
                          <p className="text-xs font-medium text-muted-foreground">{t('dupNameTitle')}</p>
                          <div className="overflow-x-auto">
                            <Table>
                              <TableHeader>
                                <TableRow>
                                  <TableHead>{t('colMappedName')}</TableHead>
                                  <TableHead>{t('colBackendCode')}</TableHead>
                                  <TableHead>{t('colBackendRoot')}</TableHead>
                                </TableRow>
                              </TableHeader>
                              <TableBody>
                                {pl.dupName.map((d, idx) => (
                                  <TableRow key={`${d.internal_id ?? idx}`}>
                                    <TableCell>{d.bind_mapped_name || d.name || '—'}</TableCell>
                                    <TableCell className="font-mono text-xs">{d.backend_scopes?.self || '—'}</TableCell>
                                    <TableCell className="font-mono text-xs break-all">{d.backend || '—'}</TableCell>
                                  </TableRow>
                                ))}
                              </TableBody>
                            </Table>
                          </div>
                        </div>
                      )}

                      {data.configured && (
                        <div className="flex flex-col gap-3 border-t border-border pt-3">
                          {isBound ? (
                            <div>
                              <Button
                                size="sm"
                                variant="destructive"
                                className="pointer-coarse:h-10"
                                disabled={isBusy}
                                onClick={() => void handleUnbind(pl.uuid)}
                              >
                                {t('btnUnbind')}
                              </Button>
                            </div>
                          ) : (
                            <>
                              <p className="text-xs text-muted-foreground">{t('bindTokenHint')}</p>
                              <form
                                onSubmit={(e) => {
                                  e.preventDefault()
                                  void handleBindTo(pl.uuid)
                                }}
                                className="flex flex-wrap items-center gap-2"
                              >
                                <Input
                                  className="w-full max-w-sm font-mono text-xs"
                                  aria-label={t('bindTokenPlaceholder')}
                                  value={tokenInputs[pl.uuid] ?? ''}
                                  onChange={(e) => setTokenInputs((prev) => ({ ...prev, [pl.uuid]: e.target.value }))}
                                  placeholder={t('bindTokenPlaceholder')}
                                />
                                <Button
                                  type="submit"
                                  size="sm"
                                  className="pointer-coarse:h-10"
                                  disabled={isBusy || !(tokenInputs[pl.uuid] ?? '').trim()}
                                >
                                  {t('btnBindTo')}
                                </Button>
                                <Button
                                  type="button"
                                  size="sm"
                                  variant="outline"
                                  className="pointer-coarse:h-10"
                                  disabled={isBusy}
                                  onClick={() => void handleFetchToken(pl.uuid)}
                                >
                                  {t('btnFetchToken')}
                                </Button>
                              </form>
                              {issuedTokens[pl.uuid] && (
                                <div className="flex flex-col gap-1">
                                  <span className="text-xs text-muted-foreground">{t('bindTokenIssuedLabel')}</span>
                                  <CopyField value={issuedTokens[pl.uuid]!} label={t('bindTokenIssuedLabel')} />
                                </div>
                              )}
                            </>
                          )}
                        </div>
                      )}
                    </CardContent>
                  </Card>
                )
              })}
            </div>
          )}

          <Card>
            <CardHeader>
              <CardTitle>{t('serversTitle')}</CardTitle>
            </CardHeader>
            <CardContent>
              <ServersTable servers={data.servers} />
            </CardContent>
          </Card>
        </>
      )}
    </div>
  )
}

function UnionAdminConsolePage() {
  const t = useT()
  const api = usePluginApi()
  const statusQuery = api.useQuery<AdminStatusResponse>('/admin/status')
  const [busyAction, setBusyAction] = useState<string | null>(null)
  const [diag, setDiag] = useState<DiagnoseResult | null>(null)

  const data = statusQuery.data

  const runAction = async (key: string, fn: () => Promise<void>) => {
    setBusyAction(key)
    try {
      await fn()
      void statusQuery.refetch()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err))
    } finally {
      setBusyAction(null)
    }
  }

  const handleUpdateList = () =>
    void runAction('list', async () => {
      const res = await api.post<{ version: number; count: number }>('/admin/actions/updatelist', {})
      toast.success(t('toastListUpdated', { version: res.version, count: res.count }))
    })

  const handleUpdateKey = async () => {
    const ok = await confirm({
      title: t('confirmUpdateKeyTitle'),
      description: t('confirmUpdateKeyDesc'),
      confirmLabel: t('btnUpdatePrivateKey'),
      destructive: true,
    })
    if (!ok) return
    await runAction('key', async () => {
      const res = await api.post<{ privateKeyVersion: number }>('/admin/actions/updateprivatekey', {})
      toast.success(t('toastKeyUpdated', { version: res.privateKeyVersion }))
    })
  }

  const handleFullSync = () =>
    void runAction('sync', async () => {
      const res = await api.post<{ count: number }>('/admin/actions/sync', {})
      toast.success(t('toastFullSyncDone', { count: res.count }))
    })

  const handleDiagnose = () =>
    void runAction('diagnose', async () => {
      const res = await api.post<DiagnoseResult>('/admin/actions/diagnose', {})
      setDiag(res)
    })

  const handleRotateOAuth2 = async () => {
    const ok = await confirm({
      title: t('confirmRotateOAuth2Title'),
      description: t('confirmRotateOAuth2Desc'),
      confirmLabel: t('btnRotateOAuth2'),
      destructive: true,
    })
    if (!ok) return
    await runAction('oauth2', async () => {
      await api.post('/admin/actions/regenerate-oauth2-key', {})
      toast.success(t('toastOAuth2Rotated'))
    })
  }

  const handleRetryOutbox = () =>
    void runAction('outbox', async () => {
      const res = await api.post<{ succeeded: number; failed: number }>('/admin/actions/retry-outbox', {})
      toast.success(t('toastOutboxRetried', { succeeded: res.succeeded, failed: res.failed }))
    })

  const handleDismissOutbox = async (id: number) => {
    const ok = await confirm({
      title: t('confirmDismissOutboxTitle'),
      confirmLabel: t('btnDismiss'),
      destructive: true,
    })
    if (!ok) return
    await runAction(`dismiss-${id}`, async () => {
      await api.delete(`/admin/outbox/${id}`)
      toast.success(t('toastOutboxDismissed'))
    })
  }

  const listHubKnown = data?.hub.serverListVersion !== null && data?.hub.serverListVersion !== undefined
  const listOutdated = listHubKnown && data.hub.serverListVersion !== data.local.serverListVersion
  const keyHubKnown = data?.hub.privateKeyVersion !== null && data?.hub.privateKeyVersion !== undefined
  const keyOutdated = keyHubKnown && data.hub.privateKeyVersion !== data.local.privateKeyVersion
  const anyBusy = busyAction !== null

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title={t('consoleTitle')}
        description={t('consoleSubtitle')}
        actions={
          <Button variant="outline" size="sm" className="pointer-coarse:h-10" onClick={() => void statusQuery.refetch()} disabled={statusQuery.isFetching}>
            {t('refresh')}
          </Button>
        }
      />

      {statusQuery.isLoading ? (
        <div className="flex items-center justify-center py-12">
          <Spinner />
        </div>
      ) : statusQuery.isError ? (
        <Alert variant="destructive">
          <AlertTitle>{t('consoleTitle')}</AlertTitle>
          <AlertDescription className="flex flex-wrap items-center justify-between gap-3">
            <span>{statusQuery.error instanceof Error ? statusQuery.error.message : String(statusQuery.error)}</span>
            <Button size="sm" variant="outline" className="pointer-coarse:h-10" onClick={() => void statusQuery.refetch()}>
              {t('retry')}
            </Button>
          </AlertDescription>
        </Alert>
      ) : !data ? null : (
        <>
          {data.hub.error && (
            <Alert variant="destructive">
              <AlertDescription className="break-words">{t('hubReachError', { error: data.hub.error })}</AlertDescription>
            </Alert>
          )}

          <div className="grid gap-4 lg:grid-cols-2">
            <Card>
              <CardHeader>
                <CardTitle>{t('syncTitle')}</CardTitle>
                <CardDescription className="font-mono text-xs break-all">{data.unionApiRoot}</CardDescription>
              </CardHeader>
              <CardContent className="flex flex-col gap-4">
                <div className="grid gap-2 text-sm">
                  <div className="flex flex-wrap items-center justify-between gap-2 rounded-md border border-border px-3 py-2">
                    <span>{t('serverListLabel')}</span>
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="font-mono text-xs text-muted-foreground">
                        {t('localVersionLabel')} v{data.local.serverListVersion}
                        {listHubKnown ? ` / ${t('hubVersionLabel')} v${data.hub.serverListVersion}` : ''}
                      </span>
                      <Badge variant={!listHubKnown ? 'secondary' : listOutdated ? 'warning' : 'success'}>
                        {!listHubKnown ? t('securityLevelUnknown') : listOutdated ? t('outdated') : t('upToDate')}
                      </Badge>
                    </div>
                  </div>
                  <div className="flex flex-wrap items-center justify-between gap-2 rounded-md border border-border px-3 py-2">
                    <span>{t('privateKeyLabel')}</span>
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="font-mono text-xs text-muted-foreground">
                        {t('localVersionLabel')} v{data.local.privateKeyVersion}
                        {keyHubKnown ? ` / ${t('hubVersionLabel')} v${data.hub.privateKeyVersion}` : ''}
                      </span>
                      <Badge variant={!keyHubKnown ? 'secondary' : keyOutdated ? 'warning' : 'success'}>
                        {!keyHubKnown ? t('securityLevelUnknown') : keyOutdated ? t('outdated') : t('upToDate')}
                      </Badge>
                    </div>
                  </div>
                </div>

                <div className="flex flex-wrap gap-2">
                  <Button
                    size="sm"
                    className="pointer-coarse:h-10"
                    loading={busyAction === 'list'}
                    disabled={anyBusy || !data.memberKeyConfigured}
                    onClick={handleUpdateList}
                  >
                    {t('btnUpdateList')}
                  </Button>
                  <Button
                    size="sm"
                    variant="outline"
                    className="pointer-coarse:h-10"
                    loading={busyAction === 'key'}
                    disabled={anyBusy || !data.memberKeyConfigured || !data.checks.hasYggdrasilKeyWrite}
                    onClick={() => void handleUpdateKey()}
                  >
                    {t('btnUpdatePrivateKey')}
                  </Button>
                  <Button
                    size="sm"
                    variant="outline"
                    className="pointer-coarse:h-10"
                    loading={busyAction === 'sync'}
                    disabled={anyBusy || !data.memberKeyConfigured}
                    onClick={handleFullSync}
                  >
                    {t('btnFullSync')}
                  </Button>
                  <Button
                    size="sm"
                    variant="secondary"
                    className="pointer-coarse:h-10"
                    loading={busyAction === 'diagnose'}
                    disabled={anyBusy || !data.memberKeyConfigured}
                    onClick={handleDiagnose}
                  >
                    {t('btnDiagnose')}
                  </Button>
                </div>

                {diag && (
                  <div className="rounded-md border border-border bg-muted/40 p-3 text-xs">
                    <p className="mb-2 font-medium">{t('diagnoseTitle')}</p>
                    <div className="grid gap-1.5">
                      <div className="flex items-center justify-between gap-2">
                        <span>{t('diagnoseSkinToUnion')}</span>
                        <Badge variant={diag.status === 'ok' ? 'success' : 'destructive'}>
                          {diag.status === 'ok' ? t('diagnosePass') : t('diagnoseFail')}
                        </Badge>
                      </div>
                      {diag.status === 'ok' && (
                        <>
                          <div className="flex items-center justify-between gap-2">
                            <span>{t('diagnoseUnionToSkin')}</span>
                            <Badge variant={diag.data.healthy ? 'success' : 'destructive'}>
                              {diag.data.healthy ? t('diagnosePass') : t('diagnoseFail')}
                            </Badge>
                          </div>
                          {typeof diag.data.delay === 'number' && (
                            <div className="flex items-center justify-between gap-2">
                              <span>{t('diagnoseDelay')}</span>
                              <span className="font-mono">{(diag.data.delay * 1000).toFixed(2)} ms</span>
                            </div>
                          )}
                        </>
                      )}
                      {diag.data.exception && (
                        <pre className="mt-1 overflow-x-auto whitespace-pre-wrap break-words rounded bg-background p-2 font-mono">{diag.data.exception}</pre>
                      )}
                      {diag.data.body && (
                        <pre className="mt-1 overflow-x-auto whitespace-pre-wrap break-words rounded bg-background p-2 font-mono">{diag.data.body}</pre>
                      )}
                    </div>
                  </div>
                )}
              </CardContent>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle>{t('readinessTitle')}</CardTitle>
                <CardDescription className="font-mono text-xs break-all">{data.wireVersion}</CardDescription>
              </CardHeader>
              <CardContent className="flex flex-col gap-2 text-sm">
                {[
                  { label: t('checkMemberKey'), ok: data.memberKeyConfigured, okText: t('statusOk'), failText: t('statusMissing'), optional: false },
                  { label: t('checkKeepUuid'), ok: data.checks.keepUuidOnRename, okText: t('statusEnabled'), failText: t('statusRecommended'), optional: false },
                  { label: t('checkUuidAlgo'), ok: true, okText: data.checks.uuidAlgorithm.toUpperCase(), failText: '', optional: false },
                  { label: t('checkEmailVerify'), ok: data.checks.requireEmailVerify, okText: t('statusEnabled'), failText: t('statusRecommended'), optional: false },
                  { label: t('checkPermKeyWrite'), ok: data.checks.hasYggdrasilKeyWrite, okText: t('statusOk'), failText: t('statusDisabled'), optional: false },
                  { label: t('checkPermUuidWrite'), ok: data.checks.hasPlayersUuidWrite, okText: t('statusOk'), failText: t('statusDisabled'), optional: false },
                  { label: t('checkRemoteKeySync'), ok: data.checks.allowRemoteKeySync, okText: t('statusEnabled'), failText: t('statusDisabled'), optional: true },
                  { label: t('checkRemoteUuidRemap'), ok: data.checks.allowRemoteUuidRemap, okText: t('statusEnabled'), failText: t('statusDisabled'), optional: true },
                  { label: t('checkRemoteBackendKey'), ok: data.checks.allowRemoteBackendKey, okText: t('statusEnabled'), failText: t('statusDisabled'), optional: true },
                ].map((row) => (
                  <div key={row.label} className="flex flex-wrap items-center justify-between gap-2 py-0.5">
                    <StatusDot status={row.ok ? 'ok' : row.optional ? 'off' : 'warn'} className="min-w-0 break-words text-xs">
                      {row.label}
                    </StatusDot>
                    <Badge className="shrink-0" variant={row.ok ? 'success' : 'secondary'}>
                      {row.ok ? row.okText : row.failText}
                    </Badge>
                  </div>
                ))}
              </CardContent>
            </Card>
          </div>

          <div className="grid gap-4 lg:grid-cols-2">
            <Card>
              <CardHeader>
                <CardTitle>{t('oauth2Title')}</CardTitle>
              </CardHeader>
              <CardContent className="flex flex-col gap-3">
                {data.local.yggdrasilPubKeyFp && (
                  <div className="flex flex-col gap-1">
                    <span className="text-xs text-muted-foreground">{t('yggPubFp')}</span>
                    <CopyField value={data.local.yggdrasilPubKeyFp} label={t('yggPubFp')} />
                  </div>
                )}
                {data.oauth2.publicKeySha256Fp && (
                  <div className="flex flex-col gap-1">
                    <span className="text-xs text-muted-foreground">{t('oauth2KeyFp')}</span>
                    <CopyField value={data.oauth2.publicKeySha256Fp} label={t('oauth2KeyFp')} />
                  </div>
                )}
                <div>
                  <Button
                    size="sm"
                    variant="outline"
                    className="pointer-coarse:h-10"
                    loading={busyAction === 'oauth2'}
                    disabled={anyBusy}
                    onClick={() => void handleRotateOAuth2()}
                  >
                    {t('btnRotateOAuth2')}
                  </Button>
                </div>
              </CardContent>
            </Card>

            {data.hub.adminNotice && (
              <Card>
                <CardHeader>
                  <CardTitle>{t('adminNoticeTitle')}</CardTitle>
                </CardHeader>
                <CardContent className="text-sm">
                  <SafeMarkdown text={data.hub.adminNotice} />
                </CardContent>
              </Card>
            )}
          </div>

          <Card>
            <CardHeader className="flex flex-row flex-wrap items-center justify-between gap-2">
              <CardTitle>{t('outboxTitle')}</CardTitle>
              {data.outbox.length > 0 && (
                <Button
                  size="sm"
                  variant="outline"
                  className="pointer-coarse:h-10"
                  loading={busyAction === 'outbox'}
                  disabled={anyBusy || !data.memberKeyConfigured}
                  onClick={handleRetryOutbox}
                >
                  {t('btnRetryOutbox')}
                </Button>
              )}
            </CardHeader>
            <CardContent>
              {data.outbox.length === 0 ? (
                <p className="text-sm text-muted-foreground">{t('outboxEmpty')}</p>
              ) : (
                <div className="overflow-x-auto">
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead>{t('colAction')}</TableHead>
                        <TableHead>{t('colPlayerName')}</TableHead>
                        <TableHead>{t('colUuid')}</TableHead>
                        <TableHead>{t('colAttempts')}</TableHead>
                        <TableHead>{t('colLastError')}</TableHead>
                        <TableHead className="text-right">{t('colActions')}</TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {data.outbox.map((item) => (
                        <TableRow key={item.id}>
                          <TableCell><Badge variant="outline">{item.action}</Badge></TableCell>
                          <TableCell>{item.name || '—'}</TableCell>
                          <TableCell className="font-mono text-xs">{item.uuid}</TableCell>
                          <TableCell className="tabular-nums">{item.attempts}</TableCell>
                          <TableCell className="max-w-64 truncate text-xs text-muted-foreground" title={item.lastError || undefined}>
                            {item.lastError}
                          </TableCell>
                          <TableCell className="text-right">
                            <Button
                              size="sm"
                              variant="ghost"
                              className="pointer-coarse:h-10"
                              disabled={anyBusy}
                              onClick={() => void handleDismissOutbox(item.id)}
                            >
                              {t('btnDismiss')}
                            </Button>
                          </TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                </div>
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>{t('serversTitle')}</CardTitle>
            </CardHeader>
            <CardContent>
              <ServersTable servers={data.servers} />
            </CardContent>
          </Card>
        </>
      )}
    </div>
  )
}

function UnionBlacklistPage() {
  const t = useT()
  const api = usePluginApi()
  const [email, setEmail] = useState('')
  const [reason, setReason] = useState('')
  const [searchInput, setSearchInput] = useState('')
  const [queryText, setQueryText] = useState('')
  const [page, setPage] = useState(1)
  const [busyId, setBusyId] = useState<number | null>(null)
  const [submitting, setSubmitting] = useState(false)

  const listQuery = api.useQuery<BlacklistPageResponse>('/admin/blacklist', {
    query: { q: queryText || undefined, page },
  })

  const handleCreate = async (e: FormEvent) => {
    e.preventDefault()
    if (!email.trim()) return
    setSubmitting(true)
    try {
      await api.post('/admin/blacklist', { email: email.trim(), reason: reason.trim() })
      toast.success(t('toastBlCreated'))
      setEmail('')
      setReason('')
      void listQuery.refetch()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err))
    } finally {
      setSubmitting(false)
    }
  }

  const handleSearch = (e: FormEvent) => {
    e.preventDefault()
    setPage(1)
    setQueryText(searchInput.trim())
  }

  const handleInvalidate = async (id: number) => {
    const ok = await confirm({
      title: t('confirmInvalidateTitle'),
      confirmLabel: t('btnInvalidate'),
    })
    if (!ok) return
    setBusyId(id)
    try {
      await api.put(`/admin/blacklist/${id}/invalidate`, {})
      toast.success(t('toastBlInvalidated'))
      void listQuery.refetch()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err))
    } finally {
      setBusyId(null)
    }
  }

  const handleDelete = async (id: number) => {
    const ok = await confirm({
      title: t('confirmDeleteTitle'),
      confirmLabel: t('btnDelete'),
      destructive: true,
    })
    if (!ok) return
    setBusyId(id)
    try {
      await api.delete(`/admin/blacklist/${id}`)
      toast.success(t('toastBlDeleted'))
      void listQuery.refetch()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err))
    } finally {
      setBusyId(null)
    }
  }

  const items = listQuery.data?.data ?? []
  const currentPage = listQuery.data?.current_page ?? page
  const lastPage = listQuery.data?.last_page ?? 1

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title={t('blacklistTitle')} description={t('blacklistSubtitle')} />

      <Card>
        <CardHeader>
          <CardTitle>{t('blAddTitle')}</CardTitle>
        </CardHeader>
        <CardContent>
          <form onSubmit={(e) => void handleCreate(e)} className="flex flex-col gap-4 md:flex-row md:items-end">
            <div className="flex flex-1 flex-col gap-1.5">
              <Label htmlFor="bl-email">{t('blEmailLabel')}</Label>
              <Input
                id="bl-email"
                type="email"
                required
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                placeholder={t('blEmailPlaceholder')}
              />
            </div>
            <div className="flex flex-1 flex-col gap-1.5">
              <Label htmlFor="bl-reason">{t('blReasonLabel')}</Label>
              <Input
                id="bl-reason"
                value={reason}
                onChange={(e) => setReason(e.target.value)}
                placeholder={t('blReasonPlaceholder')}
              />
            </div>
            <Button type="submit" className="shrink-0" loading={submitting}>
              {t('blSubmit')}
            </Button>
          </form>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <form onSubmit={handleSearch} className="flex flex-wrap items-center gap-2">
            <Input
              className="w-full max-w-sm"
              aria-label={t('blSearchPlaceholder')}
              value={searchInput}
              onChange={(e) => setSearchInput(e.target.value)}
              placeholder={t('blSearchPlaceholder')}
            />
            <Button type="submit" size="sm" variant="outline" className="pointer-coarse:h-10">
              {t('blSearch')}
            </Button>
          </form>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          {listQuery.isError && (
            <Alert variant="destructive">
              <AlertTitle>{t('blacklistTitle')}</AlertTitle>
              <AlertDescription className="flex flex-wrap items-center justify-between gap-3">
                <span>{listQuery.error instanceof Error ? listQuery.error.message : String(listQuery.error)}</span>
                <Button size="sm" variant="outline" className="pointer-coarse:h-10" onClick={() => void listQuery.refetch()}>
                  {t('retry')}
                </Button>
              </AlertDescription>
            </Alert>
          )}
          {listQuery.isLoading ? (
            <div className="flex items-center justify-center py-8">
              <Spinner />
            </div>
          ) : items.length === 0 ? (
            <p className="text-sm text-muted-foreground">{t('blEmpty')}</p>
          ) : (
            <div className="overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>{t('colId')}</TableHead>
                    <TableHead>{t('colEmail')}</TableHead>
                    <TableHead>{t('colSource')}</TableHead>
                    <TableHead>{t('colReason')}</TableHead>
                    <TableHead>{t('colCreatedAt')}</TableHead>
                    <TableHead>{t('colValidUntil')}</TableHead>
                    <TableHead className="text-right">{t('colActions')}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {items.map((item) => (
                    <TableRow key={item.id}>
                      <TableCell className="font-mono text-xs">#{item.id}</TableCell>
                      <TableCell className="font-mono text-xs">{item.email}</TableCell>
                      <TableCell><Badge variant="outline">{item.source || '—'}</Badge></TableCell>
                      <TableCell className="max-w-64 truncate text-xs" title={item.reason || undefined}>
                        {item.reason || '—'}
                      </TableCell>
                      <TableCell className="text-xs text-muted-foreground">
                        {item.created_at ? <RelativeTime date={item.created_at} /> : '—'}
                      </TableCell>
                      <TableCell className="text-xs text-muted-foreground">{item.valid_until || '—'}</TableCell>
                      <TableCell className="text-right">
                        <div className="inline-flex items-center gap-1.5">
                          <Button
                            size="sm"
                            variant="outline"
                            className="pointer-coarse:h-10"
                            disabled={busyId !== null}
                            onClick={() => void handleInvalidate(item.id)}
                          >
                            {t('btnInvalidate')}
                          </Button>
                          <Button
                            size="sm"
                            variant="destructive"
                            className="pointer-coarse:h-10"
                            disabled={busyId !== null}
                            onClick={() => void handleDelete(item.id)}
                          >
                            {t('btnDelete')}
                          </Button>
                        </div>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          )}

          {lastPage > 1 && (
            <div className="flex items-center justify-end gap-2">
              <Button size="sm" variant="outline" className="pointer-coarse:h-10" disabled={currentPage <= 1} onClick={() => setPage((p) => Math.max(1, p - 1))}>
                {t('prevPage')}
              </Button>
              <span className="text-xs text-muted-foreground">
                {t('pageInfo', { page: currentPage, last: lastPage })}
              </span>
              <Button size="sm" variant="outline" className="pointer-coarse:h-10" disabled={currentPage >= lastPage} onClick={() => setPage((p) => p + 1)}>
                {t('nextPage')}
              </Button>
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  )
}

export default defineWebPlugin({
  pages: {
    'user:/union': UnionUserPage,
    'explore:/union': UnionUserPage,
    '/union': UnionUserPage,
    'admin:/console': UnionAdminConsolePage,
    '/console': UnionAdminConsolePage,
    'admin:/blacklist': UnionBlacklistPage,
    '/blacklist': UnionBlacklistPage,
  },
})
