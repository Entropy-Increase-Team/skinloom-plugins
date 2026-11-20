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
} from '@skinloom/ui'
import { defineWebPlugin, toast, usePluginApi, useT } from '@skinloom/plugin-sdk/web'
import { useEffect, useRef, useState } from 'react'

interface TableProbe {
  table: string
  targetPlugin: string
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

const DEFAULT_TABLES = ['invitation_codes', 'mojang_verifications', 'textures_description']
const DRIVER_ITEMS = [
  { value: 'mysql', label: 'MySQL / MariaDB' },
  { value: 'postgres', label: 'PostgreSQL' },
  { value: 'sqlite', label: 'SQLite' },
]

function tableLabel(t: (key: string, params?: Record<string, string | number>) => string, tbl: string): string {
  if (tbl === 'invitation_codes') return t('tableInvitationCodes')
  if (tbl === 'mojang_verifications') return t('tableMojangVerifications')
  if (tbl === 'textures_description') return t('tableTextureDescription')
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

function BSImporterPage() {
  const t = useT()
  const api = usePluginApi()

  const [mode, setMode] = useState<'db' | 'bridge'>('db')
  const [driver, setDriver] = useState('mysql')
  const [dsn, setDsn] = useState('')
  const [tablePrefix, setTablePrefix] = useState('')
  const [bridgeUrl, setBridgeUrl] = useState('')
  const [bridgeToken, setBridgeToken] = useState('')
  const [selectedTables, setSelectedTables] = useState<string[]>(DEFAULT_TABLES)
  const [probe, setProbe] = useState<TestSourceResult | null>(null)
  const [logs, setLogs] = useState<string[]>([])
  const [lastReport, setLastReport] = useState<ImportReport | null>(null)
  const esRef = useRef<EventSource | null>(null)

  useEffect(() => {
    return () => {
      esRef.current?.close()
      esRef.current = null
    }
  }, [])

  const jobsQuery = api.useQuery<{ items: JobSummary[] }>('/admin/jobs')

  const testMutation = api.useMutation<TestSourceResult, Record<string, unknown>>('POST /admin/test', {
    onSuccess(data) {
      setProbe(data)
      const present = data.tables.filter((x) => x.exists && x.rows > 0).map((x) => x.table)
      if (present.length > 0) {
        setSelectedTables(present)
      }
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
      toast.error(err.message)
    },
  })

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
          <Tabs value={mode} onValueChange={(v) => setMode(v === 'bridge' ? 'bridge' : 'db')}>
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
                    if (v) setDriver(String(v))
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
                  type="password"
                  autoComplete="off"
                  placeholder="user:pass@tcp(127.0.0.1:3306)/blessing_skin"
                  value={dsn}
                  onChange={(e) => setDsn(e.target.value)}
                />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="bs-prefix">{t('tablePrefix')}</Label>
                <Input
                  id="bs-prefix"
                  placeholder="bs_"
                  value={tablePrefix}
                  onChange={(e) => setTablePrefix(e.target.value)}
                />
              </div>
            </div>
          ) : (
            <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label htmlFor="bs-bridge-url">{t('bridgeUrl')}</Label>
                <Input
                  id="bs-bridge-url"
                  placeholder="https://old-bs.example.com/skinloom-bridge"
                  value={bridgeUrl}
                  onChange={(e) => setBridgeUrl(e.target.value)}
                />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="bs-bridge-token">{t('bridgeToken')}</Label>
                <Input
                  id="bs-bridge-token"
                  type="password"
                  autoComplete="off"
                  value={bridgeToken}
                  onChange={(e) => setBridgeToken(e.target.value)}
                />
              </div>
            </div>
          )}

          <Button
            variant="outline"
            className="pointer-coarse:h-10"
            loading={testMutation.isPending}
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
          <div className="flex flex-col gap-3">
            {DEFAULT_TABLES.map((tbl) => {
              const info = probe?.tables.find((x) => x.table === tbl)
              return (
                <label
                  key={tbl}
                  className="flex min-h-10 cursor-pointer flex-wrap items-center justify-between gap-2 rounded-lg border border-border px-3 py-2.5 text-sm"
                >
                  <span className="flex min-w-0 items-center gap-2.5">
                    <Checkbox
                      checked={selectedTables.includes(tbl)}
                      onCheckedChange={() => toggleTable(tbl)}
                    />
                    <span className="min-w-0 break-words text-sm">{tableLabel(t, tbl)}</span>
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
              loading={runMutation.isPending}
              disabled={selectedTables.length === 0}
              onClick={() => {
                setLogs([])
                setLastReport(null)
                runMutation.mutate(buildPayload(true))
              }}
            >
              {t('dryRunBtn')}
            </Button>
            <Button
              className="pointer-coarse:h-10"
              loading={runMutation.isPending}
              disabled={selectedTables.length === 0}
              onClick={() => {
                setLogs([])
                setLastReport(null)
                runMutation.mutate(buildPayload(false))
              }}
            >
              {t('importBtn')}
            </Button>
          </div>
        </CardContent>
      </Card>

      {lastReport && lastReport.totalErrors > 0 && (
        <Alert variant="destructive">
          <AlertTitle>{t('errorsTitle', { count: lastReport.totalErrors })}</AlertTitle>
          <AlertDescription className="mt-2 space-y-2">
            {lastReport.tables
              .filter((tbl) => tbl.errors && tbl.errors.length > 0)
              .map((tbl) => {
                const errs = tbl.errors ?? []
                return (
                  <div key={tbl.table} className="space-y-1">
                    <div className="text-xs font-medium">{tableLabel(t, tbl.table)}</div>
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
            <pre className="max-h-64 overflow-y-auto rounded-lg bg-muted p-3 font-mono text-xs text-foreground">
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
                        <Badge variant={j.errorCount > 0 ? 'destructive' : 'default'}>
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
