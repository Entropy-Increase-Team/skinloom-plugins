import { Badge, TexturePreview, type TextureType } from '@skinloom/ui'
import { defineWebPlugin, useSettings, useSite, useT } from '@skinloom/plugin-sdk/web'

interface AtelierFooterProps {
  siteName?: string
}

interface AtelierTextureCardProps {
  href?: string
  name?: string
  texture?: {
    id: number
    name: string
    type: TextureType
    hash: string
    public?: boolean
    likes?: number
    uploader?: { id?: number; nickname?: string } | null
  }
}

function AtelierFooter(props: AtelierFooterProps) {
  const site = useSite()
  const t = useT()
  const settings = useSettings<{ footerTagline?: string }>()
  const name = props.siteName || site.name || 'SkinLoom'
  const tagline = typeof settings.footerTagline === 'string' && settings.footerTagline.trim()
    ? settings.footerTagline.trim()
    : t('poweredBy')

  return (
    <footer className="sl-footer mt-auto border-t border-border bg-card/60">
      <div className="mx-auto flex max-w-7xl flex-col items-center justify-between gap-2 px-4 py-6 text-xs text-muted-foreground sm:flex-row sm:px-6">
        <div className="flex items-center gap-2 font-medium text-foreground">
          <span className="inline-block size-2 rounded-full bg-primary" aria-hidden="true" />
          <span>{name}</span>
        </div>
        <p className="text-center sm:text-right">{tagline}</p>
      </div>
    </footer>
  )
}

function AtelierTextureCard(props: AtelierTextureCardProps) {
  const t = useT()
  const tex = props.texture
  if (!tex) return null

  const title = props.name || tex.name
  const rawHref = props.href || `/skinlib/show/${tex.id}`
  const href = /^\/(?![/\\])/.test(rawHref) ? rawHref : `/skinlib/show/${tex.id}`

  return (
    <a
      href={href}
      className="sl-texture-card group flex flex-col overflow-hidden rounded-xl border border-border bg-card text-card-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
    >
      <div className="relative flex aspect-[4/3] items-center justify-center bg-muted/60 p-3">
        <TexturePreview texture={{ id: tex.id }} type={tex.type} height={140} alt={title} className="max-h-full w-auto" />
        <div className="absolute top-2 left-2 flex items-center gap-1">
          <Badge variant="secondary" className="text-[11px] uppercase">
            {tex.type}
          </Badge>
          {tex.public === false && (
            <Badge variant="outline" className="bg-card/85 text-[11px]">
              {t('private')}
            </Badge>
          )}
        </div>
      </div>
      <div className="flex flex-col gap-1 border-t border-border/70 px-3 py-2.5">
        <div className="truncate text-sm font-medium text-foreground group-hover:text-primary">
          {title}
        </div>
        <div className="flex items-center justify-between gap-2 text-xs text-muted-foreground">
          <span className="truncate">{tex.uploader?.nickname || `#${tex.id}`}</span>
          {typeof tex.likes === 'number' && (
            <span className="shrink-0 tabular-nums">{t('likes', { count: tex.likes })}</span>
          )}
        </div>
      </div>
    </a>
  )
}

export default defineWebPlugin({
  overrides: {
    'site.footer': AtelierFooter,
    'texture.card': AtelierTextureCard,
  },
})
