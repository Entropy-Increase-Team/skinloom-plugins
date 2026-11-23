import {
  Alert,
  AlertDescription,
  AlertTitle,
  Badge,
  Button,
  Card,
  CardContent,
  CardHeader,
  CardTitle,
  Checkbox,
  Input,
  Label,
  PageHeader,
  RelativeTime,
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  Tabs,
  TabsList,
  TabsTrigger,
  useConfirm,
} from '@skinloom/ui'
import { defineWebPlugin, toast, usePluginApi, useSite, useT } from '@skinloom/plugin-sdk/web'
import { useEffect, useRef, useState } from 'react'

interface ProviderSummary {
  id: string
  targetPlugin: string
  dynamic: boolean
  label?: Record<string, string>
  description?: Record<string, string>
  optionKeys?: string[]
  optionPrefixes?: string[]
  tables?: string[]
}

interface TableProbe {
  table: string
  targetPlugin: string
  dynamic?: boolean
  label?: Record<string, string>
  description?: Record<string, string>
  optionKeys?: string[]
  optionPrefixes?: string[]
  tables?: string[]
  exists: boolean
  rows: number
}

interface TestSourceResult {
  ok: boolean
  mode: string
  bsVersion?: string
  tables: TableProbe[]
}

interface TableImportReport {
  table: string
  targetPlugin: string
  read: number
  imported: number
  skipped: number
  errors?: string[]
}

interface ImportReport {
  dryRun: boolean
  totalRead: number
  totalImported: number
  totalSkipped: number
  totalErrors: number
  tables: TableImportReport[]
}

interface JobSummary {
  id: number
  mode: string
  dryRun: boolean
  tables: string[]
  status: string
  importedCount: number
  skippedCount: number
  errorCount: number
  createdAt: string
}

const LEGACY_TABLES = ['invitation_codes', 'mojang_verifications', 'textures_description']
const DRIVER_ITEMS = [
  { value: 'mysql', label: 'MySQL / MariaDB' },
  { value: 'postgres', label: 'PostgreSQL' },
  { value: 'sqlite', label: 'SQLite' },
]

function pickLocalized(map: Record<string, string> | undefined, locale: string, fallback: string): string {
  if (!map) return fallback
  if (map[locale]) return map[locale]!
  const norm = locale.toLowerCase()
  if (norm.startsWith('zh') && map['zh-CN']) return map['zh-CN']!
  if (map.en) return map.en
  const first = Object.values(map).find((v) => v && v.trim())
  return first ?? fallback
}

function tableLabel(
  t: (key: string, params?: Record<string, string | number>) => string,
  tbl: string,
  locale: string,
  providers: ProviderSummary[],
): string {
  if (tbl === 'invitation_codes') return t('tableInvitationCodes')
  if (tbl === 'mojang_verifications') return t('tableMojangVerifications')
  if (tbl === 'textures_description') return t('tableTextureDescription')
  const match = providers.find((p) => p.id === tbl)
  if (match) return pickLocalized(match.label, locale, match.targetPlugin)
  return tbl
}

function modeLabel(t: (key: string, params?: Record<string, string | number>) => string, mode: string): string {
  if (mode === 'db') return t('modeDbShort')
  if (mode === 'bridge') return t('modeBridgeShort')
  return mode
}

function statusLabel(t: (key: string, params?: Record<string, string | number>) => string, status: string): string {
  if (status === 'completed') return t('statusCompleted')
  if (status === 'completed_with_errors') return t('statusCompletedWithErrors')
  if (status === 'running') return t('statusRunning')
  if (status === 'failed') return t('statusFailed')
  return status
}

function formatProviderScope(prov: ProviderSummary): string {
  const parts: string[] = []
  if (prov.optionKeys && prov.optionKeys.length > 0) {
    parts.push(`options: ${prov.optionKeys.slice(0, 5).join(', ')}${prov.optionKeys.length > 5 ? ` (+${prov.optionKeys.length - 5})` : ''}`)
  }
  if (prov.optionPrefixes && prov.optionPrefixes.length > 0) {
    parts.push(`prefixes: ${prov.optionPrefixes.join(', ')}`)
  }
  if (prov.tables && prov.tables.length > 0) {
    parts.push(`tables: ${prov.tables.join(', ')}`)
  }
  return parts.join(' | ')
}

function BSImporterPage() {
  const t = useT()
  const site = useSite()
  const api = usePluginApi()
  const confirm = useConfirm()
  const locale = site.locale || 'zh-CN'

  const [mode, setMode] = useState<'db' | 'bridge'>('db')
  const [driver, setDriver] = useState('mysql')
  const [dsn, setDsn] = useState('')
  const [tablePrefix, setTablePrefix] = useState('')
  const [bridgeUrl, setBridgeUrl] = useState('')
  const [bridgeToken, setBridgeToken] = useState('')
  const [selectedTables, setSelectedTables] = useState<string[]>(LEGACY_TABLES)
  const [probe, setProbe] = useState<TestSourceResult | null>(null)
  const [logs, setLogs] = useState<string[]>([])
  const [lastReport, setLastReport] = useState<ImportReport | null>(null)
  const [activeRunMode, setActiveRunMode] = useState<'dry' | 'live' | null>(null)
  const esRef = useRef<EventSource | null>(null)
  const initSyncedRef = useRef(false)

  useEffect(() => {
    return () => {
      esRef.current?.close()
      esRef.current = null
    }
  }, [])

  const providersQuery = api.useQuery<{ items: ProviderSummary[] }>('/admin/providers')
  const baseProviders: ProviderSummary[] =
    providersQuery.data?.items ??
    LEGACY_TABLES.map((id) => ({ id, targetPlugin: id, dynamic: false }))
  const providers: ProviderSummary[] = [...baseProviders]
  if (probe?.tables) {
    for (const tp of probe.tables) {
      if (!providers.some((p) => p.id === tp.table)) {
        providers.push({
          id: tp.table,
          targetPlugin: tp.targetPlugin,
          dynamic: Boolean(tp.dynamic),
          label: tp.label,
          description: tp.description,
          optionKeys: tp.optionKeys,
          optionPrefixes: tp.optionPrefixes,
          tables: tp.tables,
        })
      }
    }
  }

  useEffect(() => {
    if (!initSyncedRef.current && providersQuery.data?.items?.length) {
      initSyncedRef.current = true
      setSelectedTables(providersQuery.data.items.map((p) => p.id))
    }
  }, [providersQuery.data])

  const jobsQuery = api.useQuery<{ items: JobSummary[] }>('/admin/jobs')

  const testMutation = api.useMutation<TestSourceResult, Record<string, unknown>>('POST /admin/test', {
    onSuccess(data) {
      setProbe(data)
      const present = data.tables.filter((x) => x.exists && x.rows > 0).map((x) => x.table)
      setSelectedTables(present)
      void providersQuery.refetch()
      toast.success(t('verifiedToast'))
    },
    onError(err) {
      toast.error(err.message)
    },
  })

  const runMutation = api.useMutation<
    { jobId: number; status: string; report?: ImportReport },
    Record<string, unknown>
  >('POST /admin/run', {
    onSuccess(data) {
      setActiveRunMode(null)
      setLastReport(data.report ?? null)
      const label = statusLabel(t, data.status)
      if (data.status === 'completed_with_errors' || data.status === 'failed') {
        toast.error(t('jobDoneToast', { jobId: data.jobId, status: label }))
      } else {
        toast.success(t('jobDoneToast', { jobId: data.jobId, status: label }))
      }
      void jobsQuery.refetch()
      if (typeof EventSource !== 'undefined') {
        esRef.current?.close()
        const es = new EventSource(`/api/plugins/${encodeURIComponent(api.pluginId)}/admin/jobs/${data.jobId}/stream`)
        esRef.current = es
        const append = (ev: MessageEvent) => {
          try {
            const parsed = JSON.parse(String(ev.data)) as { msg?: string }
            if (parsed.msg) {
              setLogs((prev) => [...prev, parsed.msg!])
            }
          } catch {
            setLogs((prev) => [...prev, String(ev.data)])
          }
        }
        es.addEventListener('progress', append)
        es.addEventListener('done', (ev) => {
          append(ev as MessageEvent)
          es.close()
        })
        es.onerror = () => es.close()
      }
    },
    onError(err) {
      setActiveRunMode(null)
      toast.error(err.message)
    },
  })

  const hasCredentials =
    mode === 'db'
      ? dsn.trim().length > 0
      : bridgeUrl.trim().length > 0 && (bridgeToken.trim().length > 0 || bridgeUrl.trim().startsWith('{'))
  const busy = testMutation.isPending || runMutation.isPending

  const buildPayload = (dryRun: boolean) => ({
    mode,
    driver: mode === 'db' ? driver : undefined,
    dsn: mode === 'db' ? dsn : undefined,
    tablePrefix: mode === 'db' ? tablePrefix : undefined,
    bridgeUrl: mode === 'bridge' ? bridgeUrl : undefined,
    bridgeToken: mode === 'bridge' ? bridgeToken : undefined,
    tables: selectedTables,
    dryRun,
  })

  const startRun = async (dryRun: boolean) => {
    if (!dryRun) {
      const ok = await confirm({
        title: t('confirmImportTitle'),
        description: t('confirmImportDesc', { count: selectedTables.length }),
        confirmLabel: t('importBtn'),
        destructive: true,
      })
      if (!ok) return
    }
    esRef.current?.close()
    esRef.current = null
    setLogs([])
    setLastReport(null)
    setActiveRunMode(dryRun ? 'dry' : 'live')
    runMutation.mutate(buildPayload(dryRun))
  }

  const toggleTable = (tbl: string) => {
    setSelectedTables((prev) => (prev.includes(tbl) ? prev.filter((x) => x !== tbl) : [...prev, tbl]))
  }

  const jobs = jobsQuery.data?.items ?? []

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title={t('title')} description={t('subtitle')} />

      <Alert>
        <AlertTitle>{t('uninstallNoticeTitle')}</AlertTitle>
        <AlertDescription>{t('uninstallNotice')}</AlertDescription>
      </Alert>

      <Card>
        <CardHeader>
          <CardTitle>{t('sourceCardTitle')}</CardTitle>
        </CardHeader>
        <CardContent className="space-y-4">
          <Tabs
            value={mode}
            onValueChange={(v) => {
              setMode(v === 'bridge' ? 'bridge' : 'db')
              setProbe(null)
            }}
          >
            <TabsList className="grid h-auto w-full grid-cols-1 sm:inline-flex sm:w-auto">
              <TabsTrigger value="db" className="whitespace-normal text-left sm:text-center pointer-coarse:min-h-10">
                {t('modeDb')}
              </TabsTrigger>
              <TabsTrigger value="bridge" className="whitespace-normal text-left sm:text-center pointer-coarse:min-h-10">
                {t('modeBridge')}
              </TabsTrigger>
            </TabsList>
          </Tabs>

          {mode === 'db' ? (
            <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
              <div className="space-y-1.5">
                <Label htmlFor="bs-driver">{t('driver')}</Label>
                <Select
                  items={DRIVER_ITEMS}
                  value={driver}
                  onValueChange={(v) => {
                    if (v) {
                      setDriver(String(v))
                      setProbe(null)
                    }
                  }}
                >
                  <SelectTrigger id="bs-driver" className="pointer-coarse:h-10">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {DRIVER_ITEMS.map((item) => (
                      <SelectItem key={item.value} value={item.value}>
                        {item.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1.5 sm:col-span-2">
                <Label htmlFor="bs-dsn">{t('dsn')}</Label>
                <Input
                  id="bs-dsn"
                  type={driver === 'sqlite' ? 'text' : 'password'}
                  autoComplete="off"
                  placeholder={driver === 'sqlite' ? '/path/to/blessing-skin.sqlite' : 'user:pass@tcp(127.0.0.1:3306)/blessing_skin'}
                  value={dsn}
                  onChange={(e) => {
                    setDsn(e.target.value)
                    setProbe(null)
                  }}
                />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="bs-prefix">{t('tablePrefix')}</Label>
                <Input
                  id="bs-prefix"
                  placeholder="bs_"
                  value={tablePrefix}
                  onChange={(e) => {
                    setTablePrefix(e.target.value)
                    setProbe(null)
                  }}
                />
              </div>
            </div>
          ) : (
            <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label htmlFor="bs-bridge-url">{t('bridgeUrl')}</Label>
                <Input
                  id="bs-bridge-url"
                  placeholder="https://old-bs.example.com"
                  value={bridgeUrl}
                  onChange={(e) => {
                    const val = e.target.value
                    setProbe(null)
                    const trimmed = val.trim()
                    if (trimmed.startsWith('{') && trimmed.endsWith('}')) {
                      try {
                        const parsed = JSON.parse(trimmed) as { url?: unknown; token?: unknown }
                        if (typeof parsed.url === 'string' && parsed.url.trim()) {
                          setBridgeUrl(parsed.url.trim())
                          if (typeof parsed.token === 'string' && parsed.token.trim()) {
                            setBridgeToken(parsed.token.trim())
                          }
                          return
                        }
                      } catch {
                        // Fall through and let user keep editing.
                      }
                    }
                    setBridgeUrl(val)
                  }}
                />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="bs-bridge-token">{t('bridgeToken')}</Label>
                <Input
                  id="bs-bridge-token"
                  type="password"
                  autoComplete="off"
                  value={bridgeToken}
                  onChange={(e) => {
                    setBridgeToken(e.target.value)
                    setProbe(null)
                  }}
                />
              </div>
            </div>
          )}

          <Button
            variant="outline"
            className="pointer-coarse:h-10"
            loading={testMutation.isPending}
            disabled={busy || !hasCredentials}
            onClick={() => testMutation.mutate(buildPayload(true))}
          >
            {testMutation.isPending ? t('testing') : t('testBtn')}
          </Button>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t('tablesCardTitle')}</CardTitle>
        </CardHeader>
        <CardContent className="space-y-4">
          {providersQuery.isError && (
            <Alert variant="destructive">
              <AlertDescription>{providersQuery.error.message}</AlertDescription>
            </Alert>
          )}
          <div className="flex flex-col gap-3">
            {providers.map((prov) => {
              const tbl = prov.id
              const info = probe?.tables.find((x) => x.table === tbl)
              const desc = prov.dynamic ? pickLocalized(prov.description, locale, '') : ''
              const scopeText = prov.dynamic ? formatProviderScope(prov) : ''
              return (
                <label
                  key={tbl}
                  className="flex min-h-10 cursor-pointer flex-wrap items-center justify-between gap-2 rounded-lg border border-border px-3 py-2.5 text-sm"
                >
                  <span className="flex min-w-0 items-start gap-2.5">
                    <Checkbox
                      className="mt-0.5"
                      checked={selectedTables.includes(tbl)}
                      onCheckedChange={() => toggleTable(tbl)}
                    />
                    <span className="flex min-w-0 flex-col gap-0.5">
                      <span className="flex flex-wrap items-center gap-1.5">
                        <span className="min-w-0 break-words text-sm">
                          {tableLabel(t, tbl, locale, providers)}
                        </span>
                        {prov.dynamic && (
                          <Badge variant="outline" className="text-[11px]">
                            {t('dynamicProviderBadge')}
                          </Badge>
                        )}
                      </span>
                      {desc && <span className="break-words text-xs text-muted-foreground">{desc}</span>}
                      {scopeText && (
                        <span className="break-all font-mono text-[11px] text-muted-foreground">
                          {t('providerScopeLabel', { scope: scopeText })}
                        </span>
                      )}
                    </span>
                  </span>
                  {info && (
                    <Badge variant={info.exists && info.rows > 0 ? 'default' : 'secondary'} className="shrink-0">
                      {info.exists ? t('rowsDetected', { count: info.rows }) : t('notPresent')}
                    </Badge>
                  )}
                </label>
              )
            })}
          </div>

          <div className="flex flex-wrap items-center gap-2">
            <Button
              variant="outline"
              className="pointer-coarse:h-10"
              loading={activeRunMode === 'dry'}
              disabled={busy || !hasCredentials || selectedTables.length === 0}
              onClick={() => void startRun(true)}
            >
              {t('dryRunBtn')}
            </Button>
            <Button
              className="pointer-coarse:h-10"
              loading={activeRunMode === 'live'}
              disabled={busy || !hasCredentials || selectedTables.length === 0}
              onClick={() => void startRun(false)}
            >
              {t('importBtn')}
            </Button>
          </div>
        </CardContent>
      </Card>

      {lastReport && (
        <Alert variant={lastReport.totalErrors > 0 ? 'destructive' : 'default'}>
          <AlertTitle>
            {lastReport.dryRun
              ? t('reportSummaryDryRunTitle', {
                  read: lastReport.totalRead,
                  skipped: lastReport.totalSkipped,
                  errors: lastReport.totalErrors,
                })
              : t('reportSummaryTitle', {
                  read: lastReport.totalRead,
                  imported: lastReport.totalImported,
                  skipped: lastReport.totalSkipped,
                  errors: lastReport.totalErrors,
                })}
          </AlertTitle>
          <AlertDescription className="mt-2 space-y-2">
            {lastReport.tables.map((tbl) => {
              const errs = tbl.errors ?? []
              return (
                <div key={tbl.table} className="space-y-1">
                  <div className="flex flex-wrap items-center justify-between gap-2 text-xs">
                    <span className="font-medium">{tableLabel(t, tbl.table, locale, providers)}</span>
                    <span className="font-mono text-muted-foreground tabular-nums">
                      {t('reportRowMeta', {
                        read: tbl.read,
                        imported: tbl.imported,
                        skipped: tbl.skipped,
                        errors: errs.length,
                      })}
                    </span>
                  </div>
                  {errs.length > 0 && (
                    <>
                      <ul className="list-disc space-y-0.5 pl-4 font-mono text-xs">
                        {errs.slice(0, 20).map((errText, idx) => (
                          <li key={idx} className="break-all">
                            {errText}
                          </li>
                        ))}
                      </ul>
                      {errs.length > 20 && (
                        <p className="text-xs text-muted-foreground">
                          {t('errorsMore', { count: errs.length - 20 })}
                        </p>
                      )}
                    </>
                  )}
                </div>
              )
            })}
          </AlertDescription>
        </Alert>
      )}

      {logs.length > 0 && (
        <Card>
          <CardHeader>
            <CardTitle className="text-base">{t('liveLogTitle')}</CardTitle>
          </CardHeader>
          <CardContent>
            <pre className="max-h-64 overflow-x-auto overflow-y-auto whitespace-pre-wrap break-words rounded-lg bg-muted p-3 font-mono text-xs text-foreground">
              {logs.join('\n')}
            </pre>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader>
          <CardTitle className="text-base">{t('historyTitle')}</CardTitle>
        </CardHeader>
        <CardContent>
          {jobsQuery.isError ? (
            <Alert variant="destructive">
              <AlertDescription>{jobsQuery.error.message}</AlertDescription>
            </Alert>
          ) : jobs.length === 0 ? (
            <p className="py-6 text-center text-sm text-muted-foreground">{t('emptyHistory')}</p>
          ) : (
            <div className="overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>{t('colId')}</TableHead>
                    <TableHead>{t('colMode')}</TableHead>
                    <TableHead>{t('colStatus')}</TableHead>
                    <TableHead>{t('colSummary')}</TableHead>
                    <TableHead>{t('colCreated')}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {jobs.map((j) => (
                    <TableRow key={j.id}>
                      <TableCell className="font-mono text-xs">#{j.id}</TableCell>
                      <TableCell className="text-xs">
                        {modeLabel(t, j.mode)}
                        {j.dryRun ? ` (${t('dryRunTag')})` : ''}
                      </TableCell>
                      <TableCell>
                        <Badge
                          variant={
                            j.status === 'failed' || j.errorCount > 0
                              ? 'destructive'
                              : j.status === 'completed'
                                ? 'success'
                                : 'secondary'
                          }
                        >
                          {statusLabel(t, j.status)}
                        </Badge>
                      </TableCell>
                      <TableCell className="font-mono text-xs tabular-nums">
                        {j.importedCount} / {j.skippedCount} / {j.errorCount}
                      </TableCell>
                      <TableCell className="text-xs text-muted-foreground">
                        <RelativeTime date={j.createdAt} />
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  )
}

export default defineWebPlugin({
  pages: {
    'admin:/bs-importer': BSImporterPage,
    '/bs-importer': BSImporterPage,
  },
})
