import { Button, Card, CardContent, CardHeader, CardTitle, Textarea } from '@skinloom/ui'
import { defineWebPlugin, toast, usePluginApi, useT, type PluginSlotProps } from '@skinloom/plugin-sdk/web'
import { useState } from 'react'

interface TextureDescResponse {
  textureId: number
  description: string
  updatedAt: string | null
  canEdit: boolean
}

function TextureDescriptionPanel(props: PluginSlotProps['skinlib.detail.below']) {
  const t = useT()
  const api = usePluginApi()
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState('')

  const query = api.useQuery<TextureDescResponse>(`/textures/${props.textureId}`)
  const saveMutation = api.useMutation<TextureDescResponse, { description: string }>(
    `PUT /textures/${props.textureId}`,
    {
      onSuccess() {
        toast.success(t('savedToast'))
        setEditing(false)
        void query.refetch()
      },
      onError(err) {
        toast.error(err.message)
      },
    },
  )

  const data = query.data
  if (!data) return null
  if (!data.description && !data.canEdit) return null

  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between gap-2">
        <CardTitle className="text-base">{t('title')}</CardTitle>
        {data.canEdit && !editing && (
          <Button
            size="sm"
            variant="outline"
            onClick={() => {
              setDraft(data.description)
              setEditing(true)
            }}
          >
            {t('edit')}
          </Button>
        )}
      </CardHeader>
      <CardContent>
        {editing ? (
          <div className="space-y-3">
            <Textarea
              rows={4}
              value={draft}
              maxLength={2000}
              placeholder={t('placeholder')}
              onChange={(e) => setDraft(e.target.value)}
            />
            <div className="flex items-center gap-2">
              <Button
                size="sm"
                loading={saveMutation.isPending}
                onClick={() => saveMutation.mutate({ description: draft })}
              >
                {t('save')}
              </Button>
              <Button size="sm" variant="ghost" onClick={() => setEditing(false)}>
                {t('cancel')}
              </Button>
            </div>
          </div>
        ) : data.description ? (
          <p className="whitespace-pre-wrap text-sm leading-relaxed text-foreground">
            {data.description}
          </p>
        ) : (
          <p className="text-sm text-muted-foreground">{t('empty')}</p>
        )}
      </CardContent>
    </Card>
  )
}

export default defineWebPlugin({
  slots: {
    'skinlib.detail.below': TextureDescriptionPanel,
  },
})
