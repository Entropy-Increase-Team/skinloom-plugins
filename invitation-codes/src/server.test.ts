import { readFileSync } from 'node:fs'
import path from 'node:path'
import { createTestHost } from '@skinloom/plugin-sdk/testing'
import { describe, expect, it } from 'vitest'
import plugin from './server.js'

const manifest = JSON.parse(readFileSync(path.resolve(import.meta.dirname, '../plugin.json'), 'utf8'))

const ADMIN = { id: 1, email: 'admin@example.com', nickname: 'Admin', role: 'admin' as const }
const USER = { id: 2, email: 'player@example.com', nickname: 'Player', role: 'user' as const }

describe('invitation-codes plugin', () => {
  it('enforces invitation code on registration, consumes it, and awards inviter/invitee bonus', async () => {
    const host = createTestHost({
      manifest,
      settings: {
        requireCode: true,
        codePrefix: 'VIP-',
        registerBonusScore: 150,
        inviterRewardScore: 75,
      },
      fixtures: {
        users: [
          { id: 1, email: 'admin@example.com', nickname: 'Admin', role: 'admin', score: 1000 },
          { id: 2, email: 'newbie@example.com', nickname: 'Newbie', role: 'user', score: 500 },
        ],
      },
    })
    await host.setup(plugin)

    // Missing code rejected
    const missing = await host.guard('auth.register', {
      email: 'newbie@example.com',
      nickname: 'Newbie',
      form: {},
    })
    expect(missing).toMatchObject({ ok: false, rejected: true })

    // Admin generates 2 codes
    const gen = await host.request('POST', '/admin/codes/generate', {
      user: ADMIN,
      body: { count: 2, prefix: 'VIP-', maxUses: 1, note: '=SUM(A1:A2)' },
    })
    expect(gen.status).toBe(201)
    const generatedCode = (gen.body as { items: Array<{ code: string }> }).items[0]!.code
    expect(generatedCode.startsWith('VIP-')).toBe(true)

    // Valid code passes guard (both namespaced & bare key)
    const ok = await host.guard('auth.register', {
      email: 'newbie@example.com',
      nickname: 'Newbie',
      form: { 'invitation-codes:invite_code': generatedCode.toLowerCase() },
    })
    expect(ok).toEqual({ ok: true })

    // Consuming on user.registered marks code used & applies scores
    await host.emit('user.registered', {
      userId: 2,
      method: 'password',
      form: { invite_code: generatedCode },
    })
    expect(await host.ctx.score.balance(2)).toBe(650)
    expect(await host.ctx.score.balance(1)).toBe(1075)

    // Reusing exhausted code is rejected
    const reused = await host.guard('auth.register', {
      email: 'other@example.com',
      nickname: 'Other',
      form: { invite_code: generatedCode },
    })
    expect(reused).toMatchObject({ ok: false, rejected: true })

    // CSV export neutralizes formula injection (=SUM -> '=SUM)
    const csv = await host.request('GET', '/admin/codes/export.csv', { user: ADMIN })
    expect(csv.status).toBe(200)
    expect(String(csv.body)).toContain("'=SUM(A1:A2)")
  })

  it('supports user score redemption, revoke, delete, and idempotent importCodes service', async () => {
    const host = createTestHost({
      manifest,
      settings: {
        allowUserGenerate: true,
        userGenerateCost: 200,
        userMaxActiveCodes: 1,
      },
      fixtures: {
        users: [{ id: 2, email: 'player@example.com', nickname: 'Player', role: 'user', score: 300 }],
      },
    })
    await host.setup(plugin)

    const u1 = await host.request('POST', '/me/codes/generate', { user: USER, body: {} })
    expect(u1.status).toBe(201)
    expect(await host.ctx.score.balance(2)).toBe(100)

    // Second attempt blocked by userMaxActiveCodes = 1
    const u2 = await host.request('POST', '/me/codes/generate', { user: USER, body: {} })
    expect(u2.status).toBe(422)

    // Import from BS idempotently
    const firstImport = await host.service('importCodes', [
      { code: 'BS-OLD-001', generatedAt: '2025-01-01T00:00:00Z', usedBy: 0 },
      { code: 'BS-OLD-002', generatedAt: '2025-01-02T00:00:00Z', usedBy: 2, usedAt: '2025-01-03T00:00:00Z' },
    ])
    expect(firstImport).toEqual({ imported: 2, skipped: 0, errors: [] })

    const secondImport = await host.service('importCodes', [
      { code: 'bs-old-001', generatedAt: '2025-01-01T00:00:00Z', usedBy: 0 },
    ])
    expect(secondImport).toEqual({ imported: 0, skipped: 1, errors: [] })

    // Revoke BS-OLD-001 -> guard rejects it
    const rev = await host.request('POST', '/admin/codes/BS-OLD-001/revoke', { user: ADMIN })
    expect(rev.status).toBe(200)
    const afterRevoke = await host.guard('auth.register', {
      email: 'x@example.com',
      nickname: 'X',
      form: { invite_code: 'BS-OLD-001' },
    })
    expect(afterRevoke).toMatchObject({ ok: false, rejected: true })
  })
})
