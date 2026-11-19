import {
  Alert,
  AlertDescription,
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
  StatCard,
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
import { useState, type FormEvent } from 'react'

interface InviteCodeItem {
  id: string
  code: string
  status: 'active' | 'used' | 'revoked'
  creatorId: number
  usedBy: number
  usedCount: number
  maxUses: number
  createdAt: string
  usedAt: string | null
  expiresAt: string | null
  note: string
}

interface AdminCodesResponse {
  items: InviteCodeItem[]
  nextCursor: string
  stats: {
    total: number
    active: number
    used: number
    revoked: number
  }
}

interface UserCodesResponse {
  items: InviteCodeItem[]
  activeCount: number
  allowUserGenerate: boolean
  userGenerateCost: number
  userMaxActiveCodes: number
}

function statusBadgeVariant(status: InviteCodeItem['status']): 'default' | 'secondary' | 'destructive' {
  if (status === 'active') return 'default'
  if (status === 'revoked') return 'destructive'
  return 'secondary'
}

function AdminCodesPage() {
  const t = useT()
  const api = usePluginApi()
  const [statusFilter, setStatusFilter] = useState<string>('all')
  const [count, setCount] = useState('10')
  const [prefix, setPrefix] = useState('SL-')
  const [maxUses, setMaxUses] = useState('1')
  const [expireDays, setExpireDays] = useState('0')
  const [note, setNote] = useState('')
  const [busyCode, setBusyCode] = useState<string | null>(null)

  const query = api.useQuery<AdminCodesResponse>('/admin/codes', {
    query: { status: statusFilter === 'all' ? undefined : statusFilter, limit: 100 },
  })

  const generateMutation = api.useMutation<{ count: number }, Record<string, unknown>>(
    'POST /admin/codes/generate',
    {
      onSuccess(data) {
        toast.success(t('generatedToast', { count: data.count }))
        setNote('')
        void query.refetch()
      },
      onError(err) {
        toast.error(err.message)
      },
    },
  )

  const handleGenerate = (e: FormEvent) => {
    e.preventDefault()
    generateMutation.mutate({
      count: Number(count) || 1,
      prefix,
      maxUses: Number(maxUses) || 1,
      expireDays: Number(expireDays) || 0,
      note,
    })
  }

  const handleRevoke = async (code: string) => {
    setBusyCode(code)
    try {
      await api.post(`/admin/codes/${encodeURIComponent(code)}/revoke`)
      toast.success(t('revokedToast'))
      await query.refetch()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err))
    } finally {
      setBusyCode(null)
    }
  }

  const handleDelete = async (code: string) => {
    setBusyCode(code)
    try {
      await api.delete(`/admin/codes/${encodeURIComponent(code)}`)
      toast.success(t('deletedToast'))
      await query.refetch()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err))
    } finally {
      setBusyCode(null)
    }
  }

  const stats = query.data?.stats ?? { total: 0, active: 0, used: 0, revoked: 0 }
  const items = query.data?.items ?? []
  const exportHref =
    statusFilter === 'all'
      ? `/api/plugins/${encodeURIComponent(api.pluginId)}/admin/codes/export.csv`
      : `/api/plugins/${encodeURIComponent(api.pluginId)}/admin/codes/export.csv?status=${encodeURIComponent(statusFilter)}`

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title={t('title')}
        description={t('subtitle')}
        actions={
          <a
            href={exportHref}
            download="invitation-codes.csv"
            className="inline-flex h-9 items-center justify-center rounded-md border border-border bg-card px-3 text-sm font-medium text-foreground hover:bg-accent"
          >
            {t('exportCsv')}
          </a>
        }
      />

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <StatCard label={t('statTotal')} value={stats.total} />
        <StatCard label={t('statActive')} value={stats.active} />
        <StatCard label={t('statUsed')} value={stats.used} />
        <StatCard label={t('statRevoked')} value={stats.revoked} />
      </div>

      <Card>
        <CardHeader>
          <CardTitle>{t('generateTitle')}</CardTitle>
        </CardHeader>
        <CardContent>
          <form onSubmit={handleGenerate} className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-5">
            <div className="space-y-1.5">
              <Label htmlFor="ic-count">{t('count')}</Label>
              <Input id="ic-count" type="number" min={1} max={200} value={count} onChange={(e) => setCount(e.target.value)} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="ic-prefix">{t('prefix')}</Label>
              <Input id="ic-prefix" value={prefix} onChange={(e) => setPrefix(e.target.value)} maxLength={16} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="ic-uses">{t('maxUses')}</Label>
              <Input id="ic-uses" type="number" min={1} max={10000} value={maxUses} onChange={(e) => setMaxUses(e.target.value)} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="ic-expire">{t('expireDays')}</Label>
              <Input id="ic-expire" type="number" min={0} max={3650} value={expireDays} onChange={(e) => setExpireDays(e.target.value)} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="ic-note">{t('note')}</Label>
              <Input id="ic-note" value={note} onChange={(e) => setNote(e.target.value)} maxLength={120} />
            </div>
            <div className="sm:col-span-2 lg:col-span-5">
              <Button type="submit" loading={generateMutation.isPending}>
                {t('generateBtn')}
              </Button>
            </div>
          </form>
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
          <Tabs value={statusFilter} onValueChange={(v) => setStatusFilter(String(v))}>
            <TabsList>
              <TabsTrigger value="all">{t('filterAll')}</TabsTrigger>
              <TabsTrigger value="active">{t('filterActive')}</TabsTrigger>
              <TabsTrigger value="used">{t('filterUsed')}</TabsTrigger>
              <TabsTrigger value="revoked">{t('filterRevoked')}</TabsTrigger>
            </TabsList>
          </Tabs>
        </CardHeader>
        <CardContent>
          {items.length === 0 ? (
            <p className="py-8 text-center text-sm text-muted-foreground">{t('empty')}</p>
          ) : (
            <div className="overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>{t('colCode')}</TableHead>
                    <TableHead>{t('colStatus')}</TableHead>
                    <TableHead>{t('colUses')}</TableHead>
                    <TableHead>{t('colExpires')}</TableHead>
                    <TableHead>{t('colNote')}</TableHead>
                    <TableHead className="text-right">{t('colActions')}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {items.map((item) => (
                    <TableRow key={item.id}>
                      <TableCell className="font-mono text-xs">{item.code}</TableCell>
                      <TableCell>
                        <Badge variant={statusBadgeVariant(item.status)}>{item.status}</Badge>
                      </TableCell>
                      <TableCell className="tabular-nums">
                        {item.usedCount} / {item.maxUses}
                      </TableCell>
                      <TableCell className="text-xs text-muted-foreground">
                        {item.expiresAt ? <RelativeTime date={item.expiresAt} /> : t('never')}
                      </TableCell>
                      <TableCell className="max-w-48 truncate text-xs text-muted-foreground">
                        {item.note || '—'}
                      </TableCell>
                      <TableCell className="text-right">
                        <div className="inline-flex items-center gap-1.5">
                          {item.status === 'active' && (
                            <Button
                              size="sm"
                              variant="outline"
                              disabled={busyCode === item.code}
                              onClick={() => void handleRevoke(item.code)}
                            >
                              {t('revoke')}
                            </Button>
                          )}
                          <Button
                            size="sm"
                            variant="destructive"
                            disabled={busyCode === item.code}
                            onClick={() => void handleDelete(item.code)}
                          >
                            {t('delete')}
                          </Button>
                        </div>
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

function UserInvitesPage() {
  const t = useT()
  const api = usePluginApi()
  const query = api.useQuery<UserCodesResponse>('/me/codes')
  const createMutation = api.useMutation<{ item: InviteCodeItem }, Record<string, never>>(
    'POST /me/codes/generate',
    {
      onSuccess(data) {
        toast.success(t('userGeneratedToast', { code: data.item.code }))
        void query.refetch()
      },
      onError(err) {
        toast.error(err.message)
      },
    },
  )

  const data = query.data
  const items = data?.items ?? []

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title={t('userTitle')} description={t('userSubtitle')} />

      {data && !data.allowUserGenerate ? (
        <Alert>
          <AlertDescription>{t('userDisabled')}</AlertDescription>
        </Alert>
      ) : (
        <Card>
          <CardHeader>
            <CardTitle>{t('userTitle')}</CardTitle>
            {data && (
              <CardDescription>
                {t('userCostHint', {
                  cost: data.userGenerateCost,
                  active: data.activeCount,
                  max: data.userMaxActiveCodes,
                })}
              </CardDescription>
            )}
          </CardHeader>
          <CardContent>
            <Button
              loading={createMutation.isPending}
              disabled={!data || data.activeCount >= data.userMaxActiveCodes}
              onClick={() => createMutation.mutate({})}
            >
              {t('userGenerateBtn', { cost: data?.userGenerateCost ?? 0 })}
            </Button>
          </CardContent>
        </Card>
      )}

      <Card>
        <CardContent className="pt-6">
          {items.length === 0 ? (
            <p className="py-6 text-center text-sm text-muted-foreground">{t('empty')}</p>
          ) : (
            <div className="flex flex-col gap-3">
              {items.map((item) => (
                <div
                  key={item.id}
                  className="flex flex-col gap-2 rounded-lg border border-border p-3 sm:flex-row sm:items-center sm:justify-between"
                >
                  <div className="min-w-0 flex-1">
                    <CopyField value={item.code} />
                  </div>
                  <div className="flex items-center gap-2 text-xs text-muted-foreground">
                    <Badge variant={statusBadgeVariant(item.status)}>{item.status}</Badge>
                    <span className="tabular-nums">
                      {item.usedCount}/{item.maxUses}
                    </span>
                  </div>
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  )
}

export default defineWebPlugin({
  pages: {
    'admin:/codes': AdminCodesPage,
    '/codes': AdminCodesPage,
    'user:/invites': UserInvitesPage,
    '/invites': UserInvitesPage,
  },
})
