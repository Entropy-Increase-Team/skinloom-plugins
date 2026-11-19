import { readFileSync } from 'node:fs'
import path from 'node:path'
import { createTestHost } from '@skinloom/plugin-sdk/testing'
import { describe, expect, it } from 'vitest'
import plugin from './server.js'

const manifest = JSON.parse(readFileSync(path.resolve(import.meta.dirname, '../plugin.json'), 'utf8'))

const OWNER = { id: 7, email: 'owner@example.com', nickname: 'Owner', role: 'user' as const }
const OTHER = { id: 8, email: 'other@example.com', nickname: 'Other', role: 'user' as const }

describe('texture-description plugin', () => {
  it('captures upload descriptions, enforces private access, supports editing, and imports from BS', async () => {
    const host = createTestHost({
      manifest,
      settings: { maxLength: 500 },
      fixtures: {
        textures: [
          { id: 100, name: 'Sunset Steve', type: 'steve', hash: 'abc123', public: true, uploaderId: OWNER.id },
          { id: 101, name: 'Private Cape', type: 'cape', hash: 'def456', public: false, uploaderId: OWNER.id },
        ],
      },
    })
    await host.setup(plugin)

    // Upload guard stashes description; event persists it
    const g = await host.guard('texture.upload', {
      userId: OWNER.id,
      name: 'Sunset Steve',
      type: 'steve',
      public: true,
      width: 64,
      height: 64,
      sizeBytes: 1024,
      hash: 'abc123',
      form: { description: 'Hand-crafted pixel art skin (CC-BY 4.0)' },
    })
    expect(g).toEqual({ ok: true })
    await host.emit('texture.uploaded', {
      textureId: 100,
      uploaderId: OWNER.id,
      hash: 'abc123',
      type: 'steve',
      public: true,
    })

    const pubRead = await host.request('GET', '/textures/100', { user: OTHER })
    expect(pubRead.status).toBe(200)
    expect(pubRead.body).toMatchObject({
      textureId: 100,
      description: 'Hand-crafted pixel art skin (CC-BY 4.0)',
      canEdit: false,
    })

    // Non-owner cannot edit
    const forbiddenEdit = await host.request('PUT', '/textures/100', {
      user: OTHER,
      body: { description: 'Hacked' },
    })
    expect(forbiddenEdit.status).toBe(403)

    // Private texture hidden from non-owner
    const privRead = await host.request('GET', '/textures/101', { user: OTHER })
    expect(privRead.status).toBe(403)

    // Owner can edit
    const okEdit = await host.request('PUT', '/textures/100', {
      user: OWNER,
      body: { description: 'Updated v2 description' },
    })
    expect(okEdit.status).toBe(200)
    expect((okEdit.body as { description: string }).description).toBe('Updated v2 description')

    // Import service idempotency
    const imp1 = await host.service('importDescriptions', [
      { textureId: 100, description: 'Should skip existing' },
      { textureId: 101, description: 'Imported cape description' },
    ])
    expect(imp1).toEqual({ imported: 1, skipped: 1, errors: [] })
  })
})
