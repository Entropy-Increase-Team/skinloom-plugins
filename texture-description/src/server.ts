import { definePlugin, reject, type PluginContext } from '@skinloom/plugin-sdk/server'

export interface TextureDescriptionDoc extends Record<string, unknown> {
  textureId: number
  description: string
  updatedBy: number
  updatedAt: string
}

export interface ImportDescriptionInput {
  textureId: number
  description: string
}

function extractDescription(form?: Record<string, unknown>): string {
  if (!form || typeof form !== 'object') return ''
  const raw = form['texture-description:description'] ?? form['description']
  return typeof raw === 'string' ? raw.trim() : ''
}

async function readMaxLength(ctx: PluginContext): Promise<number> {
  const n = Number((await ctx.settings.get<number>('maxLength')) ?? 2000)
  return Number.isFinite(n) ? Math.max(50, Math.min(10000, Math.trunc(n))) : 2000
}

export default definePlugin({
  async setup(ctx) {
    const descriptions = ctx.db.collection<TextureDescriptionDoc>('descriptions')

    // 1. Validate length on upload and temporarily stash pending description keyed by (userId, hash).
    ctx.hooks.guard('texture.upload', async (payload) => {
      const desc = extractDescription(payload.form)
      if (!desc) return
      const maxLen = await readMaxLength(ctx)
      if (desc.length > maxLen) {
        return reject(`Texture description exceeds ${maxLen} characters.`)
      }
      await ctx.kv.set(`pending:${payload.userId}:${payload.hash}`, desc, 300)
      return
    })

    // 2. Attach stashed description once texture is persisted.
    ctx.events.on('texture.uploaded', async (payload) => {
      if (!payload.uploaderId || !payload.hash) return
      const key = `pending:${payload.uploaderId}:${payload.hash}`
      const desc = await ctx.kv.get<string>(key)
      if (!desc) return
      await ctx.kv.delete(key)

      const now = new Date().toISOString()
      const existing = await descriptions.findOne({ textureId: payload.textureId })
      if (existing) {
        await descriptions.updateOne(
          { id: existing.id },
          { description: desc, updatedBy: payload.uploaderId, updatedAt: now },
        )
      } else {
        await descriptions.insertOne({
          textureId: payload.textureId,
          description: desc,
          updatedBy: payload.uploaderId,
          updatedAt: now,
        })
      }
    })

    // 3. Clean up description when texture is deleted.
    ctx.events.on('texture.deleted', async (payload) => {
      const existing = await descriptions.findOne({ textureId: payload.textureId })
      if (existing) {
        await descriptions.deleteOne({ id: existing.id })
      }
    })

    // 4. Cross-plugin service exposed for bs-importer (idempotent import).
    ctx.services.expose<
      [ImportDescriptionInput[]],
      { imported: number; skipped: number; errors: string[] }
    >('importDescriptions', async (items) => {
      if (!Array.isArray(items)) {
        return { imported: 0, skipped: 0, errors: ['items must be an array'] }
      }
      const maxLen = await readMaxLength(ctx)
      let imported = 0
      let skipped = 0
      const errors: string[] = []
      const now = new Date().toISOString()
      for (const item of items) {
        const tid = Math.trunc(Number(item?.textureId ?? 0))
        const desc = typeof item?.description === 'string' ? item.description.trim() : ''
        if (!Number.isFinite(tid) || tid <= 0 || !desc) {
          errors.push(`invalid texture description entry (tid=${String(item?.textureId)})`)
          continue
        }
        const existing = await descriptions.findOne({ textureId: tid })
        if (existing) {
          skipped++
          continue
        }
        await descriptions.insertOne({
          textureId: tid,
          description: desc.slice(0, maxLen),
          updatedBy: 0,
          updatedAt: now,
        })
        imported++
      }
      return { imported, skipped, errors }
    })

    // 5. Public read & owner/admin edit routes
    ctx.http.get('/textures/:tid', { auth: 'public' }, async (req) => {
      const tid = Math.trunc(Number(req.params.tid ?? 0))
      if (!Number.isFinite(tid) || tid <= 0) {
        return { status: 400, body: { detail: 'Invalid texture ID' } }
      }
      const tex = await ctx.textures.get(tid)
      if (!tex) {
        return { status: 404, body: { detail: 'Texture not found' } }
      }
      const isAdmin = req.user?.role === 'admin' || req.user?.role === 'superadmin'
      const isOwner = Boolean(req.user && tex.uploaderId !== null && req.user.id === tex.uploaderId)
      if (!tex.public && !isOwner && !isAdmin) {
        return { status: 403, body: { detail: 'Private texture' } }
      }
      const doc = await descriptions.findOne({ textureId: tid })
      return {
        status: 200,
        body: {
          textureId: tid,
          description: doc?.description ?? '',
          updatedAt: doc?.updatedAt ?? null,
          canEdit: isOwner || isAdmin,
        },
      }
    })

    ctx.http.put('/textures/:tid', { auth: 'user' }, async (req) => {
      if (!req.user) {
        return { status: 401, body: { detail: 'Authentication required' } }
      }
      const tid = Math.trunc(Number(req.params.tid ?? 0))
      if (!Number.isFinite(tid) || tid <= 0) {
        return { status: 400, body: { detail: 'Invalid texture ID' } }
      }
      const tex = await ctx.textures.get(tid)
      if (!tex) {
        return { status: 404, body: { detail: 'Texture not found' } }
      }
      const isAdmin = req.user.role === 'admin' || req.user.role === 'superadmin'
      const isOwner = tex.uploaderId !== null && req.user.id === tex.uploaderId
      if (!isOwner && !isAdmin) {
        return { status: 403, body: { detail: 'Only the uploader or an administrator may edit this description.' } }
      }

      const body = (req.body ?? {}) as Record<string, unknown>
      const text = typeof body.description === 'string' ? body.description.trim() : ''
      const maxLen = await readMaxLength(ctx)
      if (text.length > maxLen) {
        return reject(`Description exceeds ${maxLen} characters.`)
      }

      const existing = await descriptions.findOne({ textureId: tid })
      if (!text) {
        if (existing) {
          await descriptions.deleteOne({ id: existing.id })
        }
        return { status: 200, body: { textureId: tid, description: '', updatedAt: null } }
      }

      const now = new Date().toISOString()
      if (existing) {
        await descriptions.updateOne(
          { id: existing.id },
          { description: text, updatedBy: req.user.id, updatedAt: now },
        )
      } else {
        await descriptions.insertOne({
          textureId: tid,
          description: text,
          updatedBy: req.user.id,
          updatedAt: now,
        })
      }
      return { status: 200, body: { textureId: tid, description: text, updatedAt: now } }
    })
  },
})
