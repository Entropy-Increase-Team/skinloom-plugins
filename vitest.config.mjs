import path from 'node:path'
import { fileURLToPath } from 'node:url'

const __dirname = path.dirname(fileURLToPath(import.meta.url))
const skinloomWeb = path.resolve(
  process.env.SKINLOOM_REPO_DIR ? path.join(process.env.SKINLOOM_REPO_DIR, 'web') : path.join(__dirname, '..', 'skinloom', 'web'),
)

export default {
  resolve: {
    alias: {
      '@skinloom/plugin-sdk/server': path.join(skinloomWeb, 'packages/plugin-sdk/src/server/index.ts'),
      '@skinloom/plugin-sdk/web': path.join(skinloomWeb, 'packages/plugin-sdk/src/web/index.ts'),
      '@skinloom/plugin-sdk/testing': path.join(skinloomWeb, 'packages/plugin-sdk/src/testing/index.ts'),
      '@skinloom/plugin-sdk/catalog': path.join(skinloomWeb, 'packages/plugin-sdk/src/gen/catalog.ts'),
      '@skinloom/plugin-sdk': path.join(skinloomWeb, 'packages/plugin-sdk/src/index.ts'),
      '@skinloom/ui': path.join(skinloomWeb, 'packages/ui/src/index.ts'),
    },
  },
  test: {
    environment: 'node',
  },
}
