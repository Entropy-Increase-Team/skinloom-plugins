<p align="center"><img src="assets/icon.svg" width="72" alt="SkinLoom"></p>

<h1 align="center">云织官方插件与主题集 · SkinLoom Plugins</h1>

<p align="center">云织 SkinLoom 官方维护的 Tier 1 轻量沙箱插件、Tier 2 原生 Go 系统插件与 Level ⑤ 主题扩展源码库</p>

<p align="center">
  <a href="https://github.com/Entropy-Increase-Team/skinloom-plugins/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/Entropy-Increase-Team/skinloom-plugins/ci.yml?branch=main&style=flat-square&logo=githubactions&logoColor=white&label=CI" alt="CI"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/Entropy-Increase-Team/skinloom-plugins?style=flat-square" alt="License"></a>
  <a href="https://github.com/Entropy-Increase-Team/skinloom/releases"><img src="https://img.shields.io/badge/SkinLoom-%3E%3D1.1.3-2F855A?style=flat-square" alt="SkinLoom >=1.1.3"></a>
  <a href="https://github.com/Entropy-Increase-Team/skinloom-plugins/stargazers"><img src="https://img.shields.io/github/stars/Entropy-Increase-Team/skinloom-plugins?style=flat-square&logo=github" alt="GitHub Stars"></a>
</p>

<p align="center">
  <a href="https://www.npmjs.com/package/@skinloom/plugin-sdk"><img src="https://img.shields.io/npm/v/@skinloom/plugin-sdk?style=flat-square&logo=npm&label=%40skinloom%2Fplugin-sdk&color=CB3837" alt="@skinloom/plugin-sdk"></a>
  <img src="https://img.shields.io/github/go-mod/go-version/Entropy-Increase-Team/skinloom-plugins?filename=mua-union%2Fgo.mod&style=flat-square&logo=go&logoColor=white" alt="Go Version">
  <img src="https://img.shields.io/badge/TypeScript-5.9-3178C6?style=flat-square&logo=typescript&logoColor=white" alt="TypeScript">
  <img src="https://img.shields.io/badge/React-19-149ECA?style=flat-square&logo=react&logoColor=white" alt="React 19">
</p>

<p align="center">
  <a href="https://docs.skinloom.cn/docs/plugins/overview">插件开发文档</a> ·
  <a href="https://github.com/Entropy-Increase-Team/skinloom-registry">官方插件市场</a> ·
  <a href="https://github.com/Entropy-Increase-Team/skinloom">SkinLoom 主仓库</a> ·
  <a href="README.en.md">English</a>
</p>

---

本仓库集中维护 SkinLoom 官方团队出品的全部插件与主题扩展，所有正式版本均通过 [skinloom-registry](https://github.com/Entropy-Increase-Team/skinloom-registry) 以固定 Commit SHA 受控编译、官方 Ed25519 私钥签名并分发至官方插件与主题市场。

## 官方扩展清单

| 扩展 ID | 类型 | 运行时 | 简介 |
| --- | --- | --- | --- |
| [`invitation-codes`](./invitation-codes) | `plugin` | `js` (Tier 1) | 邀请码注册拦截、批量生成、CSV 导出、玩家积分兑换邀请码与邀请人奖励，暴露 `importCodes` 服务 |
| [`mojang-verification`](./mojang-verification) | `plugin` | `js` (Tier 1) | Microsoft / Xbox Live OAuth 正版认证、同名角色冲突回收退分通知、正版徽章与改名同步，暴露 `importVerifications` 服务 |
| [`texture-description`](./texture-description) | `plugin` | `js` (Tier 1) | 皮肤与披风描述说明、上传时填写与详情页在线编辑，暴露 `importDescriptions` 服务 |
| [`theme-atelier`](./theme-atelier) | `theme` | Level ⑤ | 官方五层主题示范包：暖陶配色 Token、分栏登录/标签用户中心、主题设置、自由 CSS 与 `site.footer` / `texture.card` 组件覆盖 |
| [`bs-importer`](./bs-importer) | `plugin` | `native` (Tier 2 Go) | 按需安装、迁完可卸载的 Blessing Skin 扩展数据迁移助手（直连 MySQL/PG/SQLite 或 Export Bridge，支持旧站扩展表并自动发现实现通用 `bsMigrateDescribe`/`bsMigrateImport` 协议的已安装插件 + SSE 实时进度流） |
| [`mua-union`](./mua-union) | `plugin` | `native` (Tier 2 Go) | 中国高校 Minecraft 联盟（MUA）联合皮肤站认证插件：成员站 `skinDomains` 同步、角色跨站绑定与改名重试队列、联合黑名单及 Union OAuth2 单点登录，实现通用 BS 迁移协议 |

## 站长如何安装

无需手动编译本仓库：在 SkinLoom 管理后台进入 **「插件与主题 → 插件市场」** 或 **「外观 → 浏览主题市场」** 搜索上述 ID 即可一键验签安装，或通过命令行安装：

```bash
skinloom plugin market-install invitation-codes
skinloom plugin market-install mojang-verification
skinloom plugin market-install texture-description
skinloom plugin market-install theme-atelier
skinloom plugin market-install bs-importer --trust-high-risk
skinloom plugin market-install mua-union --trust-high-risk
```

## 本地开发与验证

本仓库与兄弟目录 `../skinloom` 共享 `@skinloom/cli`、`@skinloom/plugin-sdk`、`@skinloom/ui` 与 Go SDK（零重复依赖）：

```bash
# 1. 确保主仓库工具链已构建
cd ../skinloom/web && bun install && bun run --filter @skinloom/plugin-sdk build && bun run --filter @skinloom/cli build

# 2. 在本仓库一键运行全部插件的清单检查、类型检查、单元测试与构建
cd ../../skinloom-plugins
npm run lint
npm run typecheck
npm test
npm run test:go
npm run build
```

## 许可

[MIT](LICENSE)。
