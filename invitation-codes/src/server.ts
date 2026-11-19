import { definePlugin, reject, type PluginContext } from '@skinloom/plugin-sdk/server'

export type InviteCodeStatus = 'active' | 'used' | 'revoked'

export interface InviteCodeDoc extends Record<string, unknown> {
  code: string
  status: InviteCodeStatus
  creatorId: number
  usedBy: number
  usedCount: number
  maxUses: number
  createdAt: string
  usedAt: string | null
  expiresAt: string | null
  note: string
}

export interface InviteCodeSettings {
  requireCode: boolean
  codePrefix: string
  defaultMaxUses: number
  defaultExpireDays: number
  allowUserGenerate: boolean
  userGenerateCost: number
  userMaxActiveCodes: number
  registerBonusScore: number
  inviterRewardScore: number
}

export interface ImportCodeInput {
  code: string
  generatedAt?: string
  usedBy?: number
  usedAt?: string | null
  creatorId?: number
  maxUses?: number
  note?: string
}

const MAX_BULK_COUNT = 200
const CODE_ALPHABET = '23456789ABCDEFGHJKLMNPQRSTUVWXYZ'

function extractInviteCode(form?: Record<string, unknown>): string {
  if (!form || typeof form !== 'object') return ''
  const raw = form['invitation-codes:invite_code'] ?? form['invite_code']
  return typeof raw === 'string' ? raw.trim().toUpperCase() : ''
}

function clampInt(raw: unknown, fallback: number, min: number, max: number): number {
  const n = typeof raw === 'number' ? raw : Number(raw)
  if (!Number.isFinite(n)) return fallback
  return Math.max(min, Math.min(max, Math.trunc(n)))
}

function normalizePrefix(raw: unknown, fallback = 'SL-'): string {
  const s = typeof raw === 'string' ? raw.trim().toUpperCase() : fallback
  return s.replace(/[^A-Z0-9_-]/g, '').slice(0, 16)
}

async function readSettings(ctx: PluginContext): Promise<InviteCodeSettings> {
  const all = await ctx.settings.all<Partial<InviteCodeSettings>>()
  return {
    requireCode: all.requireCode !== false,
    codePrefix: normalizePrefix(all.codePrefix ?? 'SL-', 'SL-'),
    defaultMaxUses: clampInt(all.defaultMaxUses, 1, 1, 10000),
    defaultExpireDays: clampInt(all.defaultExpireDays, 0, 0, 3650),
    allowUserGenerate: Boolean(all.allowUserGenerate),
    userGenerateCost: clampInt(all.userGenerateCost, 200, 0, 1000000),
    userMaxActiveCodes: clampInt(all.userMaxActiveCodes, 5, 1, 100),
    registerBonusScore: clampInt(all.registerBonusScore, 0, 0, 1000000),
    inviterRewardScore: clampInt(all.inviterRewardScore, 0, 0, 1000000),
  }
}

async function generateUniqueCode(ctx: PluginContext, prefix: string): Promise<string> {
  const bytes = await ctx.crypto.randomBytes(10)
  let body = ''
  for (let i = 0; i < bytes.length; i++) {
    body += CODE_ALPHABET[bytes[i]! % CODE_ALPHABET.length]
  }
  return `${prefix}${body.slice(0, 5)}-${body.slice(5, 10)}`
}

function isExpired(doc: InviteCodeDoc, nowMs = Date.now()): boolean {
  if (!doc.expiresAt) return false
  const t = Date.parse(doc.expiresAt)
  return Number.isFinite(t) && t <= nowMs
}

function csvCell(value: string | number | null | undefined): string {
  let text = value === null || value === undefined ? '' : String(value)
  // Prevent spreadsheet CSV formula injection (including leading whitespace before =, +, -, @, tab, CR).
  if (/^\s*[=+\-@\t\r]/.test(text)) {
    text = `'${text}`
  }
  if (/[",\r\n]/.test(text)) {
    return `"${text.replaceAll('"', '""')}"`
  }
  return text
}

export default definePlugin({
  async setup(ctx) {
    const codes = ctx.db.collection<InviteCodeDoc>('codes')

    // 1. Pre-registration Guard: validate invitation code before account creation.
    ctx.hooks.guard('auth.register', async (payload) => {
      const cfg = await readSettings(ctx)
      const code = extractInviteCode(payload.form)
      if (!code) {
        if (cfg.requireCode) {
          return reject('An invitation code is required to register on this site.')
        }
        return
      }
      const doc = await codes.findOne({ code })
      if (!doc) {
        return reject('Invalid invitation code.')
      }
      if (doc.status === 'revoked') {
        return reject('This invitation code has been revoked.')
      }
      if (isExpired(doc)) {
        return reject('This invitation code has expired.')
      }
      if (doc.status === 'used' || doc.usedCount >= doc.maxUses) {
        return reject('This invitation code has already reached its usage limit.')
      }
      return
    })

    // 2. Post-registration Event: consume code and grant configured bonus/inviter score.
    ctx.events.on('user.registered', async (payload) => {
      const code = extractInviteCode(payload.form)
      if (!code) return

      const doc = await codes.findOne({ code })
      if (!doc || doc.status === 'revoked' || doc.usedCount >= doc.maxUses || isExpired(doc)) {
        return
      }
      const nextUses = doc.usedCount + 1
      const nextStatus: InviteCodeStatus = nextUses >= doc.maxUses ? 'used' : 'active'
      await codes.updateOne(
        { id: doc.id },
        {
          usedCount: nextUses,
          usedBy: payload.userId,
          usedAt: new Date().toISOString(),
          status: nextStatus,
        },
      )

      const cfg = await readSettings(ctx)
      if (cfg.registerBonusScore > 0) {
        await ctx.score.apply(
          payload.userId,
          cfg.registerBonusScore,
          'invitation_code_bonus',
          'invite_code',
          doc.code,
        )
      }
      if (cfg.inviterRewardScore > 0 && doc.creatorId > 0 && doc.creatorId !== payload.userId) {
        await ctx.score.apply(
          doc.creatorId,
          cfg.inviterRewardScore,
          'invitation_code_inviter_reward',
          'invite_code',
          doc.code,
        )
      }
    })

    // 3. Cross-plugin service exposed for bs-importer (idempotent import).
    ctx.services.expose<[ImportCodeInput[]], { imported: number; skipped: number; errors: string[] }>(
      'importCodes',
      async (items) => {
        if (!Array.isArray(items)) {
          return { imported: 0, skipped: 0, errors: ['items must be an array'] }
        }
        let imported = 0
        let skipped = 0
        const errors: string[] = []
        for (const item of items) {
          const normalized = typeof item?.code === 'string' ? item.code.trim().toUpperCase() : ''
          if (!normalized || normalized.length > 64) {
            errors.push(`invalid code "${String(item?.code ?? '')}"`)
            continue
          }
          const existing = await codes.findOne({ code: normalized })
          if (existing) {
            skipped++
            continue
          }
          const usedBy = clampInt(item.usedBy, 0, 0, Number.MAX_SAFE_INTEGER)
          const maxUses = clampInt(item.maxUses, 1, 1, 10000)
          const usedCount = usedBy > 0 ? 1 : 0
          const status: InviteCodeStatus = usedCount >= maxUses ? 'used' : 'active'
          await codes.insertOne({
            code: normalized,
            status,
            creatorId: clampInt(item.creatorId, 0, 0, Number.MAX_SAFE_INTEGER),
            usedBy,
            usedCount,
            maxUses,
            createdAt: item.generatedAt && !Number.isNaN(Date.parse(item.generatedAt))
              ? new Date(item.generatedAt).toISOString()
              : new Date().toISOString(),
            usedAt: item.usedAt && !Number.isNaN(Date.parse(item.usedAt))
              ? new Date(item.usedAt).toISOString()
              : null,
            expiresAt: null,
            note: typeof item.note === 'string' ? item.note.slice(0, 120) : 'Imported from Blessing Skin',
          })
          imported++
        }
        return { imported, skipped, errors }
      },
    )

    // 4. Admin API routes
    ctx.http.get('/admin/codes', { auth: 'admin' }, async (req) => {
      const status = (req.query.status ?? '').trim()
      const q = (req.query.q ?? '').trim().toUpperCase()
      const limit = clampInt(req.query.limit, 50, 1, 200)
      const cursor = req.query.cursor || undefined

      const filter: Record<string, unknown> = {}
      if (status === 'active' || status === 'used' || status === 'revoked') {
        filter.status = status
      }
      if (q) {
        filter.code = { $prefix: q }
      }

      const [page, total, active, used, revoked] = await Promise.all([
        codes.find(filter, { sort: { createdAt: -1 }, limit, cursor }),
        codes.count({}),
        codes.count({ status: 'active' }),
        codes.count({ status: 'used' }),
        codes.count({ status: 'revoked' }),
      ])

      return {
        status: 200,
        body: {
          items: page.items,
          nextCursor: page.nextCursor ?? '',
          stats: { total, active, used, revoked },
        },
      }
    })

    ctx.http.post('/admin/codes/generate', { auth: 'admin' }, async (req) => {
      const cfg = await readSettings(ctx)
      const body = (req.body ?? {}) as Record<string, unknown>
      const count = clampInt(body.count, 1, 1, MAX_BULK_COUNT)
      const prefix = normalizePrefix(body.prefix ?? cfg.codePrefix, cfg.codePrefix)
      const maxUses = clampInt(body.maxUses, cfg.defaultMaxUses, 1, 10000)
      const expireDays = clampInt(body.expireDays, cfg.defaultExpireDays, 0, 3650)
      const note = typeof body.note === 'string' ? body.note.trim().slice(0, 120) : ''

      const now = Date.now()
      const createdAt = new Date(now).toISOString()
      const expiresAt = expireDays > 0 ? new Date(now + expireDays * 86400_000).toISOString() : null
      const creatorId = req.user?.id ?? 0

      const created: Array<InviteCodeDoc & { id: string }> = []
      for (let i = 0; i < count; i++) {
        let code = ''
        for (let attempt = 0; attempt < 5; attempt++) {
          const candidate = await generateUniqueCode(ctx, prefix)
          const exists = await codes.findOne({ code: candidate })
          if (!exists) {
            code = candidate
            break
          }
        }
        if (!code) {
          return reject('Failed to allocate unique invitation code after retries.')
        }
        const doc = await codes.insertOne({
          code,
          status: 'active',
          creatorId,
          usedBy: 0,
          usedCount: 0,
          maxUses,
          createdAt,
          usedAt: null,
          expiresAt,
          note,
        })
        created.push(doc)
      }

      await ctx.audit.record('invitation_codes.generate', 'plugin', 'invitation-codes', {
        count: created.length,
        prefix,
        maxUses,
        expireDays,
      })

      return {
        status: 201,
        body: { items: created, count: created.length },
      }
    })

    ctx.http.post('/admin/codes/:code/revoke', { auth: 'admin' }, async (req) => {
      const code = (req.params.code ?? '').trim().toUpperCase()
      const doc = await codes.findOne({ code })
      if (!doc) {
        return { status: 404, body: { detail: 'Invitation code not found' } }
      }
      if (doc.status !== 'active') {
        return reject(`Cannot revoke code with status "${doc.status}".`)
      }
      await codes.updateOne({ id: doc.id }, { status: 'revoked' })
      await ctx.audit.record('invitation_codes.revoke', 'invite_code', code, null)
      return { status: 200, body: { ok: true, code, status: 'revoked' } }
    })

    ctx.http.delete('/admin/codes/:code', { auth: 'admin' }, async (req) => {
      const code = (req.params.code ?? '').trim().toUpperCase()
      const doc = await codes.findOne({ code })
      if (!doc) {
        return { status: 404, body: { detail: 'Invitation code not found' } }
      }
      await codes.deleteOne({ id: doc.id })
      await ctx.audit.record('invitation_codes.delete', 'invite_code', code, null)
      return { status: 200, body: { deleted: true, code } }
    })

    ctx.http.get('/admin/codes/export.csv', { auth: 'admin' }, async (req) => {
      const status = (req.query.status ?? '').trim()
      const filter: Record<string, unknown> = {}
      if (status === 'active' || status === 'used' || status === 'revoked') {
        filter.status = status
      }
      const rows: string[] = [
        'code,status,used_count,max_uses,creator_id,used_by,created_at,used_at,expires_at,note',
      ]
      let cursor: string | undefined
      for (let pageIdx = 0; pageIdx < 25; pageIdx++) {
        const res = await codes.find(filter, { sort: { createdAt: -1 }, limit: 200, cursor })
        for (const item of res.items) {
          rows.push(
            [
              csvCell(item.code),
              csvCell(item.status),
              csvCell(item.usedCount),
              csvCell(item.maxUses),
              csvCell(item.creatorId),
              csvCell(item.usedBy),
              csvCell(item.createdAt),
              csvCell(item.usedAt ?? ''),
              csvCell(item.expiresAt ?? ''),
              csvCell(item.note),
            ].join(','),
          )
        }
        if (!res.nextCursor) break
        cursor = res.nextCursor
      }
      return {
        status: 200,
        headers: {
          'Content-Type': 'text/csv; charset=utf-8',
          'Content-Disposition': 'attachment; filename="invitation-codes.csv"',
        },
        body: `${rows.join('\n')}\n`,
      }
    })

    // 5. Self-service User API routes
    ctx.http.get('/me/codes', { auth: 'user' }, async (req) => {
      if (!req.user) {
        return { status: 401, body: { detail: 'Authentication required' } }
      }
      const [cfg, page, activeCount] = await Promise.all([
        readSettings(ctx),
        codes.find({ creatorId: req.user.id }, { sort: { createdAt: -1 }, limit: 50 }),
        codes.count({ creatorId: req.user.id, status: 'active' }),
      ])
      return {
        status: 200,
        body: {
          items: page.items,
          activeCount,
          allowUserGenerate: cfg.allowUserGenerate,
          userGenerateCost: cfg.userGenerateCost,
          userMaxActiveCodes: cfg.userMaxActiveCodes,
        },
      }
    })

    ctx.http.post('/me/codes/generate', { auth: 'user' }, async (req) => {
      if (!req.user) {
        return { status: 401, body: { detail: 'Authentication required' } }
      }
      const cfg = await readSettings(ctx)
      if (!cfg.allowUserGenerate) {
        return reject('Self-service invitation code generation is disabled.')
      }
      const activeCount = await codes.count({ creatorId: req.user.id, status: 'active' })
      if (activeCount >= cfg.userMaxActiveCodes) {
        return reject(`You already hold ${activeCount}/${cfg.userMaxActiveCodes} active invitation codes.`)
      }
      let code = ''
      for (let attempt = 0; attempt < 5; attempt++) {
        const candidate = await generateUniqueCode(ctx, cfg.codePrefix)
        if (!(await codes.findOne({ code: candidate }))) {
          code = candidate
          break
        }
      }
      if (!code) {
        return reject('Failed to allocate unique invitation code; please try again.')
      }

      if (cfg.userGenerateCost > 0) {
        const balance = await ctx.score.balance(req.user.id)
        if (balance < cfg.userGenerateCost) {
          return reject(`Insufficient score (need ${cfg.userGenerateCost}, have ${balance}).`)
        }
        await ctx.score.apply(
          req.user.id,
          -cfg.userGenerateCost,
          'invitation_code_generate',
          'user',
          String(req.user.id),
        )
      }

      const now = Date.now()
      const createdAt = new Date(now).toISOString()
      const expiresAt =
        cfg.defaultExpireDays > 0 ? new Date(now + cfg.defaultExpireDays * 86400_000).toISOString() : null
      const doc = await codes.insertOne({
        code,
        status: 'active',
        creatorId: req.user.id,
        usedBy: 0,
        usedCount: 0,
        maxUses: 1,
        createdAt,
        usedAt: null,
        expiresAt,
        note: `Generated by user #${req.user.id}`,
      })
      return { status: 201, body: { item: doc } }
    })
  },
})
