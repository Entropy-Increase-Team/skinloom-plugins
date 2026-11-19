import { readFileSync } from 'node:fs'
import path from 'node:path'
import { createTestHost } from '@skinloom/plugin-sdk/testing'
import { describe, expect, it } from 'vitest'
import plugin from './server.js'

const manifest = JSON.parse(readFileSync(path.resolve(import.meta.dirname, '../plugin.json'), 'utf8'))

const ALICE = { id: 10, email: 'alice@example.com', nickname: 'Alice', role: 'user' as const }
const SQUATTER = { id: 20, email: 'squatter@example.com', nickname: 'Squatter', role: 'user' as const }

describe('mojang-verification plugin', () => {
  it('completes Microsoft -> XBL -> XSTS -> Minecraft OAuth, reclaims conflicting name, awards badge & score, and syncs renames', async () => {
    const host = createTestHost({
      manifest,
      site: { url: 'https://skins.example.com' },
      settings: {
        clientId: 'ms-client-id',
        clientSecret: 'ms-client-secret',
        conflictPolicy: 'reclaim',
        notifyByEmail: true,
        rewardScore: 500,
        badgeText: '正版认证',
      },
      fixtures: {
        users: [
          { ...ALICE, score: 100 },
          { ...SQUATTER, score: 200 },
        ],
        players: [{ id: 99, userId: SQUATTER.id, name: 'Notch', uuid: '00000000000000000000000000000099' }],
      },
    })
    await host.setup(plugin)

    host.fetchMock
      .mockJson('https://login.live.com/oauth20_token.srf', { access_token: 'ms-access-token' })
      .mockJson('https://user.auth.xboxlive.com/user/authenticate', {
        Token: 'xbl-token',
        DisplayClaims: { xui: [{ uhs: 'user-hash-123' }] },
      })
      .mockJson('https://xsts.auth.xboxlive.com/xsts/authorize', { Token: 'xsts-token' })
      .mockJson('https://api.minecraftservices.com/authentication/login_with_xbox', {
        access_token: 'mc-access-token',
      })
      .mockJson('https://api.minecraftservices.com/minecraft/profile', {
        id: '069a79f444e94726a5befca90e38aaf5',
        name: 'Notch',
      })
      .mockJson('https://sessionserver.mojang.com/session/minecraft/profile/069a79f444e94726a5befca90e38aaf5', {
        id: '069a79f444e94726a5befca90e38aaf5',
        name: 'NotchRenamed',
      })

    // Step 1: Start OAuth flow
    const start = await host.request('POST', '/me/start', { user: ALICE, body: {} })
    expect(start.status).toBe(200)
    const { state, authUrl } = start.body as { state: string; authUrl: string }
    expect(authUrl).toContain('login.live.com/oauth20_authorize.srf')
    expect(state.length).toBeGreaterThan(10)

    // Step 2: Callback verifies account, reclaims "Notch" from Squatter, awards 500 score
    const cb = await host.request('GET', '/callback', {
      user: ALICE,
      query: { code: 'oauth-code-xyz', state },
    })
    expect(cb.status).toBe(302)
    expect(await host.ctx.score.balance(ALICE.id)).toBe(600)
    expect(
      host.notifications.some((n) => Array.isArray(n.target) && n.target.includes(SQUATTER.id)),
    ).toBe(true)
    expect(host.sentMails.some((m) => m.to === SQUATTER.email)).toBe(true)

    const alicePlayers = await host.ctx.players.list(ALICE.id)
    expect(alicePlayers.some((p) => p.name === 'Notch')).toBe(true)

    // Step 3: user.badges filter grants verified badge to Alice but not Squatter
    const aliceBadges = await host.filter('user.badges', [], { userId: ALICE.id, role: 'user' })
    expect(aliceBadges).toEqual([{ text: '正版认证', color: 'success', icon: 'badge-check' }])
    const squatterBadges = await host.filter('user.badges', [], { userId: SQUATTER.id, role: 'user' })
    expect(squatterBadges).toEqual([])

    // Step 4: Refresh syncs new Mojang username
    const ref = await host.request('POST', '/me/refresh', { user: ALICE, body: {} })
    expect(ref.status).toBe(200)
    expect((ref.body as { mcName: string }).mcName).toBe('NotchRenamed')
    const renamedPlayers = await host.ctx.players.list(ALICE.id)
    expect(renamedPlayers.some((p) => p.name === 'NotchRenamed')).toBe(true)
  })

  it('imports legacy BS mojang_verifications idempotently via importVerifications service', async () => {
    const host = createTestHost({ manifest })
    await host.setup(plugin)

    const r1 = await host.service('importVerifications', [
      { userId: 10, uuid: '069a79f4-44e9-4726-a5be-fca90e38aaf5', verified: true, mcName: 'Notch' },
      { userId: 11, uuid: 'not-a-uuid', verified: true },
    ])
    expect(r1).toMatchObject({ imported: 1, skipped: 0 })
    expect((r1 as { errors: string[] }).errors.length).toBe(1)

    const r2 = await host.service('importVerifications', [
      { userId: 10, uuid: '069a79f444e94726a5befca90e38aaf5', verified: true },
    ])
    expect(r2).toEqual({ imported: 0, skipped: 1, errors: [] })
  })
})
