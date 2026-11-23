<p align="center"><img src="assets/icon.svg" width="72" alt="SkinLoom"></p>

<h1 align="center">SkinLoom Official Plugins &amp; Themes</h1>

<p align="center">Canonical monorepo for official Tier 1 sandbox plugins, Tier 2 native Go plugins, and Level 5 themes</p>

<p align="center">
  <a href="https://docs.skinloom.cn/docs/plugins/overview">Docs (Chinese)</a> ·
  <a href="https://github.com/Entropy-Increase-Team/skinloom-registry">Official Registry</a> ·
  <a href="https://github.com/Entropy-Increase-Team/skinloom">SkinLoom Core</a> ·
  <a href="README.md">简体中文</a>
</p>

---

All official extensions maintained by the SkinLoom team live here and are published as signed `.slp` packages through [Entropy-Increase-Team/skinloom-registry](https://github.com/Entropy-Increase-Team/skinloom-registry).

## Packages

| ID | Kind | Runtime | Description |
| --- | --- | --- | --- |
| [`invitation-codes`](./invitation-codes) | `plugin` | `js` (Tier 1) | Registration invitation codes, bulk admin generation, CSV export, user score redemption, and `importCodes` service |
| [`mojang-verification`](./mojang-verification) | `plugin` | `js` (Tier 1) | Microsoft / Xbox Live OAuth Minecraft Java ownership verification, name reclamation, verified badge, and `importVerifications` service |
| [`texture-description`](./texture-description) | `plugin` | `js` (Tier 1) | Texture descriptions editable on upload and detail pages, plus `importDescriptions` service |
| [`theme-atelier`](./theme-atelier) | `theme` | Level 5 | Reference L1–L5 theme with warm terracotta tokens, layout variants, settings, CSS, and `site.footer` / `texture.card` overrides |
| [`bs-importer`](./bs-importer) | `plugin` | `native` (Tier 2 Go) | On-demand Blessing Skin extension importer (direct MySQL/PG/SQLite + Export Bridge for legacy extension tables and dynamically discovered plugins implementing the generic `bsMigrateDescribe`/`bsMigrateImport` protocol, with SSE live progress); safe to uninstall after migration |
| [`mua-union`](./mua-union) | `plugin` | `native` (Tier 2 Go) | Minecraft University Alliance (MUA) Union federation protocol: member `skinDomains` sync, cross-site player binding & retry outbox, shared blacklist, Union OAuth2 SSO, and generic BS migration provider |

## Local Development

```bash
npm run lint
npm run typecheck
npm test
npm run test:go
npm run build
```

## License

[MIT](LICENSE).
