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
} from '@skinloom/ui'
import { defineWebPlugin, navigate, toast, usePluginApi, useT } from '@skinloom/plugin-sdk/web'

interface VerificationStatus {
  configured: boolean
  verified: boolean
  uuid: string | null
  mcName: string | null
  verifiedAt: string | null
  rewardScore: number
  badgeText: string
}

function MojangVerificationCard() {
  const t = useT()
  const api = usePluginApi()
  const statusQuery = api.useQuery<VerificationStatus>('/me/status')

  const startMutation = api.useMutation<{ authUrl: string }, Record<string, never>>('POST /me/start', {
    onSuccess(data) {
      if (data.authUrl) {
        navigate(data.authUrl)
      }
    },
    onError(err) {
      toast.error(err.message)
    },
  })

  const refreshMutation = api.useMutation<{ mcName: string }, Record<string, never>>('POST /me/refresh', {
    onSuccess(data) {
      toast.success(t('refreshedToast', { name: data.mcName }))
      void statusQuery.refetch()
    },
    onError(err) {
      toast.error(err.message)
    },
  })

  const data = statusQuery.data

  return (
    <Card>
      <CardHeader className="flex flex-row items-start justify-between gap-3">
        <div className="min-w-0 space-y-1">
          <CardTitle>{t('title')}</CardTitle>
          <CardDescription>{t('subtitle')}</CardDescription>
        </div>
        {data && (
          <Badge variant={data.verified ? 'default' : 'secondary'} className="shrink-0">
            {data.verified ? data.badgeText || t('verified') : t('unverified')}
          </Badge>
        )}
      </CardHeader>
      <CardContent className="space-y-4">
        {statusQuery.isError ? (
          <Alert variant="destructive">
            <AlertDescription>{statusQuery.error.message}</AlertDescription>
          </Alert>
        ) : data?.verified ? (
          <>
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <div className="min-w-0 space-y-1">
                <div className="text-xs text-muted-foreground">{t('mcName')}</div>
                <div className="truncate font-medium text-foreground">{data.mcName || '—'}</div>
              </div>
              <div className="min-w-0 space-y-1">
                <div className="text-xs text-muted-foreground">{t('mcUuid')}</div>
                {data.uuid ? <CopyField value={data.uuid} /> : <span className="text-sm">—</span>}
              </div>
            </div>
            <Button
              variant="outline"
              size="sm"
              className="pointer-coarse:h-10"
              loading={refreshMutation.isPending}
              onClick={() => refreshMutation.mutate({})}
            >
              {t('refreshBtn')}
            </Button>
          </>
        ) : (
          <>
            {data && data.rewardScore > 0 && (
              <p className="text-sm text-muted-foreground">
                {t('rewardHint', { score: data.rewardScore })}
              </p>
            )}
            {data && !data.configured ? (
              <Alert>
                <AlertDescription>{t('notConfigured')}</AlertDescription>
              </Alert>
            ) : (
              <Button
                className="pointer-coarse:h-10"
                loading={startMutation.isPending}
                disabled={!data}
                onClick={() => startMutation.mutate({})}
              >
                {t('verifyBtn')}
              </Button>
            )}
          </>
        )}
      </CardContent>
    </Card>
  )
}

export default defineWebPlugin({
  slots: {
    'user.dashboard.widgets': {
      component: MojangVerificationCard,
      order: 20,
    },
    'user.profile.sections': {
      component: MojangVerificationCard,
      order: 20,
    },
  },
})
