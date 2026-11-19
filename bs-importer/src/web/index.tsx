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
      toast.success('Connection verified')
    },
    onError(err) {
      toast.error(err.message)
    },
  })

  const runMutation = api.useMutation<{ jobId: number; status: string }, Record<string, unknown>>('POST /admin/run', {
    onSuccess(data) {
      toast.success(`Job #${data.jobId}: ${data.status}`)
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
            <TabsList>
              <TabsTrigger value="db">{t('modeDb')}</TabsTrigger>
              <TabsTrigger value="bridge">{t('modeBridge')}</TabsTrigger>
            </TabsList>
          </Tabs>

          {mode === 'db' ? (
            <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
              <div className="space-y-1.5">
                <Label>{t('driver')}</Label>
                <Select value={driver} onValueChange={(v) => setDriver(String(v))}>
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="mysql">MySQL / MariaDB</SelectItem>
                    <SelectItem value="postgres">PostgreSQL</SelectItem>
                    <SelectItem value="sqlite">SQLite</SelectItem>
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
                  className="flex items-center justify-between gap-3 rounded-lg border border-border px-3 py-2.5 text-sm"
                >
                  <span className="flex items-center gap-2.5">
                    <Checkbox
                      checked={selectedTables.includes(tbl)}
                      onCheckedChange={() => toggleTable(tbl)}
                    />
                    <span className="font-mono text-xs">{tbl}</span>
                  </span>
                  {info && (
                    <Badge variant={info.exists && info.rows > 0 ? 'default' : 'secondary'}>
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
              loading={runMutation.isPending}
              disabled={selectedTables.length === 0}
              onClick={() => {
                setLogs([])
                runMutation.mutate(buildPayload(true))
              }}
            >
              {t('dryRunBtn')}
            </Button>
            <Button
              loading={runMutation.isPending}
              disabled={selectedTables.length === 0}
              onClick={() => {
                setLogs([])
                runMutation.mutate(buildPayload(false))
              }}
            >
              {t('importBtn')}
            </Button>
          </div>
        </CardContent>
      </Card>

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
          {jobs.length === 0 ? (
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
                        {j.mode} {j.dryRun ? '(dry-run)' : ''}
                      </TableCell>
                      <TableCell>
                        <Badge variant={j.errorCount > 0 ? 'destructive' : 'default'}>{j.status}</Badge>
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
