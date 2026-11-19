import { definePlugin, reject, type PluginContext } from '@skinloom/plugin-sdk/server'

export interface VerificationDoc extends Record<string, unknown> {
  userId: number
  uuid: string
  mcName: string
  verified: boolean
  verifiedAt: string
  updatedAt: string
}

export interface MojangSettings {
  clientId: string
  clientSecret: string
  conflictPolicy: 'reclaim' | 'reject'
  notifyByEmail: boolean
  rewardScore: number
  badgeText: string
}

export interface ImportVerificationInput {
  userId: number
  uuid: string
  verified?: boolean
  mcName?: string
  createdAt?: string
}

interface PendingOAuthState {
  userId: number
  createdAt: string
}

const UUID_HEX_RE = /^[0-9a-f]{32}$/

function normalizeUUID(raw: unknown): string {
  if (typeof raw !== 'string') return ''
  return raw.trim().toLowerCase().replaceAll('-', '')
}

async function readSettings(ctx: PluginContext): Promise<MojangSettings> {
  const all = await ctx.settings.all<Partial<MojangSettings>>()
  const reward = Number(all.rewardScore ?? 500)
  return {
    clientId: typeof all.clientId === 'string' ? all.clientId.trim() : '',
    clientSecret: typeof all.clientSecret === 'string' ? all.clientSecret.trim() : '',
    conflictPolicy: all.conflictPolicy === 'reject' ? 'reject' : 'reclaim',
    notifyByEmail: all.notifyByEmail !== false,
    rewardScore: Number.isFinite(reward) ? Math.max(0, Math.min(1_000_000, Math.trunc(reward))) : 500,
    badgeText: typeof all.badgeText === 'string' && all.badgeText.trim() ? all.badgeText.trim().slice(0, 32) : '正版认证',
  }
}

async function buildRedirectURI(ctx: PluginContext): Promise<string> {
  const siteUrl = (await ctx.site.url()).trim().replace(/\/+$/, '')
  if (!siteUrl) {
    throw new Error('site.url must be configured before using Microsoft verification')
  }
  return `${siteUrl}/api/plugins/mojang-verification/callback`
}

async function resolveMinecraftProfile(
  ctx: PluginContext,
  cfg: MojangSettings,
  code: string,
  redirectUri: string,
): Promise<{ uuid: string; name: string }> {
  // Step 1: Exchange OAuth code for Microsoft Live access token
  const formBody = new URLSearchParams({
    client_id: cfg.clientId,
    client_secret: cfg.clientSecret,
    code,
    grant_type: 'authorization_code',
    redirect_uri: redirectUri,
  }).toString()

  const msResp = await ctx.fetch('https://login.live.com/oauth20_token.srf', {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: formBody,
  })
  if (!msResp.ok) {
    throw new Error(`Microsoft token exchange failed (HTTP ${msResp.status})`)
  }
  const msData = await msResp.json<{ access_token?: string }>()
  if (!msData.access_token) {
    throw new Error('Microsoft token response missing access_token')
  }

  // Step 2: Authenticate with Xbox Live
  const xblResp = await ctx.fetch('https://user.auth.xboxlive.com/user/authenticate', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify({
      Properties: {
        AuthMethod: 'RPS',
        SiteName: 'user.auth.xboxlive.com',
        RpsTicket: `d=${msData.access_token}`,
      },
      RelyingParty: 'http://auth.xboxlive.com',
      TokenType: 'JWT',
    }),
  })
  if (!xblResp.ok) {
    throw new Error(`Xbox Live authentication failed (HTTP ${xblResp.status})`)
  }
  const xblData = await xblResp.json<{
    Token?: string
    DisplayClaims?: { xui?: Array<{ uhs?: string }> }
  }>()
  const xblToken = xblData.Token
  const uhs = xblData.DisplayClaims?.xui?.[0]?.uhs
  if (!xblToken || !uhs) {
    throw new Error('Xbox Live response missing token or user hash')
  }

  // Step 3: Obtain XSTS token for Minecraft Services
  const xstsResp = await ctx.fetch('https://xsts.auth.xboxlive.com/xsts/authorize', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify({
      Properties: {
        SandboxId: 'RETAIL',
        UserTokens: [xblToken],
      },
      RelyingParty: 'rp://api.minecraftservices.com/',
      TokenType: 'JWT',
    }),
  })
  if (!xstsResp.ok) {
    throw new Error(`XSTS authorization failed (HTTP ${xstsResp.status})`)
  }
  const xstsData = await xstsResp.json<{ Token?: string }>()
  if (!xstsData.Token) {
    throw new Error('XSTS response missing token')
  }

  // Step 4: Authenticate with Minecraft Services
  const mcAuthResp = await ctx.fetch('https://api.minecraftservices.com/authentication/login_with_xbox', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify({
      identityToken: `XBL3.0 x=${uhs};${xstsData.Token}`,
    }),
  })
  if (!mcAuthResp.ok) {
    throw new Error(`Minecraft Services login failed (HTTP ${mcAuthResp.status})`)
  }
  const mcAuthData = await mcAuthResp.json<{ access_token?: string }>()
  if (!mcAuthData.access_token) {
    throw new Error('Minecraft Services login response missing access_token')
  }

  // Step 5: Fetch official Minecraft Java profile
  const profResp = await ctx.fetch('https://api.minecraftservices.com/minecraft/profile', {
    method: 'GET',
    headers: { Authorization: `Bearer ${mcAuthData.access_token}`, Accept: 'application/json' },
  })
  if (profResp.status === 404) {
    throw new Error('This Microsoft account does not own Minecraft Java Edition.')
  }
  if (!profResp.ok) {
    throw new Error(`Failed to fetch Minecraft profile (HTTP ${profResp.status})`)
  }
  const prof = await profResp.json<{ id?: string; name?: string }>()
  const uuid = normalizeUUID(prof.id)
  const name = typeof prof.name === 'string' ? prof.name.trim() : ''
  if (!UUID_HEX_RE.test(uuid) || !name) {
    throw new Error('Minecraft profile response is incomplete')
  }
  return { uuid, name }
}

export default definePlugin({
  async setup(ctx) {
    const verifications = ctx.db.collection<VerificationDoc>('verifications')

    // 1. User badge filter: attach verified badge to verified accounts.
    ctx.hooks.filter('user.badges', async (badges, filterCtx) => {
      const rec = await verifications.findOne({ userId: filterCtx.userId })
      if (!rec || !rec.verified) {
        return badges
      }
      const cfg = await readSettings(ctx)
      return [
        ...badges,
        {
          text: cfg.badgeText,
          color: 'success',
          icon: 'badge-check',
        },
      ]
    })

    // 2. Cross-plugin service exposed for bs-importer (idempotent import).
    ctx.services.expose<
      [ImportVerificationInput[]],
      { imported: number; skipped: number; errors: string[] }
    >('importVerifications', async (items) => {
      if (!Array.isArray(items)) {
        return { imported: 0, skipped: 0, errors: ['items must be an array'] }
      }
      let imported = 0
      let skipped = 0
      const errors: string[] = []
      for (const item of items) {
        const userId = Math.trunc(Number(item?.userId ?? 0))
        const uuid = normalizeUUID(item?.uuid)
        if (!Number.isFinite(userId) || userId <= 0 || !UUID_HEX_RE.test(uuid)) {
          errors.push(`invalid verification record (userId=${String(item?.userId)}, uuid=${String(item?.uuid)})`)
          continue
        }
        const [byUUID, byUser] = await Promise.all([
          verifications.findOne({ uuid }),
          verifications.findOne({ userId }),
        ])
        if (byUUID || byUser) {
          skipped++
          continue
        }
        const ts =
          item.createdAt && !Number.isNaN(Date.parse(item.createdAt))
            ? new Date(item.createdAt).toISOString()
            : new Date().toISOString()
        await verifications.insertOne({
          userId,
          uuid,
          mcName: typeof item.mcName === 'string' ? item.mcName.trim().slice(0, 32) : '',
          verified: item.verified !== false,
          verifiedAt: ts,
          updatedAt: ts,
        })
        imported++
      }
      return { imported, skipped, errors }
    })

    // 3. Authenticated user routes
    ctx.http.get('/me/status', { auth: 'user' }, async (req) => {
      if (!req.user) {
        return { status: 401, body: { detail: 'Authentication required' } }
      }
      const [cfg, rec] = await Promise.all([
        readSettings(ctx),
        verifications.findOne({ userId: req.user.id }),
      ])
      return {
        status: 200,
        body: {
          configured: Boolean(cfg.clientId && cfg.clientSecret),
          verified: Boolean(rec?.verified),
          uuid: rec?.uuid ?? null,
          mcName: rec?.mcName ?? null,
          verifiedAt: rec?.verifiedAt ?? null,
          rewardScore: cfg.rewardScore,
          badgeText: cfg.badgeText,
        },
      }
    })

    ctx.http.post('/me/start', { auth: 'user' }, async (req) => {
      if (!req.user) {
        return { status: 401, body: { detail: 'Authentication required' } }
      }
      const existing = await verifications.findOne({ userId: req.user.id })
      if (existing?.verified) {
        return reject('This account has already completed Minecraft verification.')
      }
      const cfg = await readSettings(ctx)
      if (!cfg.clientId || !cfg.clientSecret) {
        return reject('Microsoft OAuth clientId and clientSecret are not configured.')
      }
      const redirectUri = await buildRedirectURI(ctx)
      const state = await ctx.crypto.randomString(32)
      await ctx.kv.set<PendingOAuthState>(
        `oauth:${state}`,
        { userId: req.user.id, createdAt: new Date().toISOString() },
        600,
      )
      const qs = new URLSearchParams({
        client_id: cfg.clientId,
        response_type: 'code',
        redirect_uri: redirectUri,
        scope: 'XboxLive.signin offline_access',
        state,
        prompt: 'select_account',
      })
      return {
        status: 200,
        body: {
          authUrl: `https://login.live.com/oauth20_authorize.srf?${qs.toString()}`,
          state,
        },
      }
    })

    ctx.http.get('/callback', { auth: 'user' }, async (req) => {
      if (!req.user) {
        return { status: 401, body: { detail: 'Please sign in before completing verification.' } }
      }
      if (req.query.error) {
        return reject(`Microsoft verification canceled or denied (${req.query.error}).`)
      }
      const code = (req.query.code ?? '').trim()
      const state = (req.query.state ?? '').trim()
      if (!code || !state) {
        return reject('Missing OAuth code or state parameter.')
      }

      const pending = await ctx.kv.get<PendingOAuthState>(`oauth:${state}`)
      await ctx.kv.delete(`oauth:${state}`)
      if (!pending || pending.userId !== req.user.id) {
        return reject('Invalid or expired verification state. Please try again.')
      }

      const existingByUser = await verifications.findOne({ userId: req.user.id })
      if (existingByUser?.verified) {
        return reject('This account has already completed Minecraft verification.')
      }

      const cfg = await readSettings(ctx)
      if (!cfg.clientId || !cfg.clientSecret) {
        return reject('Microsoft OAuth credentials are not configured.')
      }

      const redirectUri = await buildRedirectURI(ctx)
      const profile = await resolveMinecraftProfile(ctx, cfg, code, redirectUri)

      // Ensure UUID is not already linked to another SkinLoom account.
      const existingByUUID = await verifications.findOne({ uuid: profile.uuid })
      if (existingByUUID && existingByUUID.userId !== req.user.id) {
        return reject('This Minecraft account is already verified on another SkinLoom account.')
      }

      // Resolve player name conflict if another user holds the official Minecraft username.
      const existingPlayer = await ctx.players.findByName(profile.name)
      if (existingPlayer && existingPlayer.userId !== req.user.id) {
        if (cfg.conflictPolicy === 'reject') {
          return reject(`Player name "${profile.name}" is already in use by another account.`)
        }
        await ctx.players.delete(existingPlayer.userId, existingPlayer.id, true)
        const noticeBody = `Your player **${profile.name}** was reclaimed because the verified official Minecraft owner linked their account. Any player creation score has been refunded.`
        await ctx.notify.send(existingPlayer.userId, 'Player Name Reclaimed by Verified Owner', noticeBody)
        if (cfg.notifyByEmail) {
          const prevOwner = await ctx.users.get(existingPlayer.userId)
          if (prevOwner?.email) {
            await ctx.mail.send({
              to: prevOwner.email,
              subject: `Player "${profile.name}" reclaimed by verified Minecraft owner`,
              text: noticeBody.replaceAll('**', ''),
            })
          }
        }
      }

      // Ensure the verified user owns a player with their official Minecraft username.
      const myPlayers = await ctx.players.list(req.user.id)
      const alreadyHasName = myPlayers.some((p) => p.name.toLowerCase() === profile.name.toLowerCase())
      if (!alreadyHasName) {
        await ctx.players.create(req.user.id, profile.name)
      }

      const nowISO = new Date().toISOString()
      const isFirstTime = !existingByUser
      if (existingByUser) {
        await verifications.updateOne(
          { id: existingByUser.id },
          {
            uuid: profile.uuid,
            mcName: profile.name,
            verified: true,
            updatedAt: nowISO,
          },
        )
      } else {
        await verifications.insertOne({
          userId: req.user.id,
          uuid: profile.uuid,
          mcName: profile.name,
          verified: true,
          verifiedAt: nowISO,
          updatedAt: nowISO,
        })
      }

      if (isFirstTime && cfg.rewardScore > 0) {
        await ctx.score.apply(
          req.user.id,
          cfg.rewardScore,
          'mojang_verification_reward',
          'mojang_uuid',
          profile.uuid,
        )
      }

      await ctx.audit.record('mojang_verification.verified', 'user', String(req.user.id), {
        uuid: profile.uuid,
        mcName: profile.name,
        firstTime: isFirstTime,
      })

      return {
        status: 302,
        headers: { Location: '/user/profile?mojang=verified' },
        body: { ok: true, uuid: profile.uuid, mcName: profile.name },
      }
    })

    ctx.http.post('/me/refresh', { auth: 'user' }, async (req) => {
      if (!req.user) {
        return { status: 401, body: { detail: 'Authentication required' } }
      }
      const rec = await verifications.findOne({ userId: req.user.id })
      if (!rec || !rec.verified) {
        return reject('Your account has not completed Minecraft verification yet.')
      }

      const resp = await ctx.fetch(
        `https://sessionserver.mojang.com/session/minecraft/profile/${encodeURIComponent(rec.uuid)}`,
        { method: 'GET', headers: { Accept: 'application/json' } },
      )
      if (!resp.ok) {
        return reject(`Failed to query Mojang sessionserver (HTTP ${resp.status}).`)
      }
      const prof = await resp.json<{ name?: string }>()
      const latestName = typeof prof.name === 'string' ? prof.name.trim() : ''
      if (!latestName) {
        return reject('Mojang profile did not include a valid username.')
      }

      if (rec.mcName && rec.mcName.toLowerCase() !== latestName.toLowerCase()) {
        const myPlayers = await ctx.players.list(req.user.id)
        const oldPlayer = myPlayers.find((p) => p.name.toLowerCase() === rec.mcName.toLowerCase())
        if (oldPlayer) {
          const conflict = await ctx.players.findByName(latestName)
          if (!conflict || conflict.id === oldPlayer.id) {
            await ctx.players.rename(req.user.id, oldPlayer.id, latestName)
          }
        }
      }

      const nowISO = new Date().toISOString()
      await verifications.updateOne({ id: rec.id }, { mcName: latestName, updatedAt: nowISO })
      return {
        status: 200,
        body: { ok: true, uuid: rec.uuid, mcName: latestName, updatedAt: nowISO },
      }
    })
  },
})
