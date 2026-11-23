# MUA Union Federation (`mua-union`)

SkinLoom 官方 Tier 2 原生 Go 插件，完整兼容中国高校 Minecraft 联盟（[MUA](https://skin.mualliance.ltd)）联合皮肤站认证协议（对应原 Blessing Skin `yggdrasil-connect` MUA 魔改版协议），并在安全模型上完全遵循 SkinLoom 原生规范。

## 功能概览

- **协议 100% 线级兼容**：
  - `GET /api/union/member` 站点能力发现（支持跨域 `CORS: *`）
  - RSA-SHA256 `UnionHostVerify`（`rawBody + timestamp + nonce`，`[-10s, +30s]` 时间窗与 60s SQL 防重放）
  - 成员站列表同步（自动向 Yggdrasil `skinDomains` 注入所有成员站域名）、全量角色同步、邮箱反查、双向连通性诊断
  - Union OAuth2 单点登录（`/api/union/member/oauth2` + `/api/union/member/oauth2/grant`，自动管理 4096 位 RSA 签名密钥对与分片 PKCS#1 v1.5 加密）
- **可靠的角色生命周期同步**：监听 `player.created`、`player.renamed`、`player.deleted` 事件自动同步至 Union Hub，失败写入隔离 SQL 重试队列，由定时任务指数退避重试。
- **用户跨站角色绑定中心**：展示站点安全等级（SL0/SL1/SL3）、重名冲突提示、跨站绑定令牌签发与绑定、解绑、UUID 统一请求，服务端强制校验当前登录用户对角色 UUID 的归属权（彻底消除 IDOR）。
- **管理员控制台与联合黑名单**：提供接入就绪检查（如 `player.keep_uuid_on_rename`、`yggdrasil.uuid_algorithm`、高危权限与远端回调开关）、版本比对、一键同步/诊断与联合黑名单增删查失效管理。
- **纵深安全强化**：
  - 所有高危远端回调（`updateprivatekey`、`remapuuid`、`updatebackendkey`）均受**宿主高危权限**（`yggdrasil.key.write` / `players.uuid.write`）与**插件设置默认关闭显式开关**双重保护，且强制写入审计日志。
  - 永久移除高危 `/api/yggdrasil/restore` 签发接口，`/api/union/member/updateplugin` 返回 `501` 拒绝任意 ZIP 覆盖，统一通过 SkinLoom 官方 Ed25519 验签插件市场更新。
  - 出站请求强制 HTTPS 并内置 DNS 重绑定与私网/回环/云元数据 IP SSRF 拦截。

## 推荐站点配置

1. 在 SkinLoom 管理后台「站点设置」中：
   - 开启 **改名保留 UUID**（`player.keep_uuid_on_rename`）
   - 建议开启 **注册邮箱验证**（满足 MUA SL3 安全评级）
   - Yggdrasil Connect (OIDC) 已由 SkinLoom Core 原生内置，无需额外部署 Janus。
2. 在插件设置中填写 `unionMemberKey`，并按需开启 `enableUnionOAuth2` 或高危远端回调开关。

## 从 Blessing Skin（MUA 版 `yggdrasil-connect`）平滑迁移

- **系统层（SkinLoom 核心导入向导）**：自动迁入角色 UUID（`uuid` 表）、Yggdrasil 签名私钥（`ygg_private_key`）、`yggdrasil.disable_authserver`、`oauth_clients`，并在源站启用了 `yggdrasil-connect` 时自动开启 `player.keep_uuid_on_rename`。
- **插件层（通用 `bsMigrateDescribe` / `bsMigrateImport` 协议）**：本插件主动声明 `optionalDependencies: { "bs-importer": "^1.0.0" }` 并实现通用迁移接口。安装官方 `bs-importer`（v1.1.0+）后将被自动发现（未安装本插件的站点不会看到 MUA 迁移项），无论是**直连旧数据库**还是通过 **`blessing-skin-export-bridge`**，勾选后即可一键幂等迁入 `union_api_root`、`union_member_key`、成员站列表及版本号、私钥版本号、功能开关以及完整的 4096 位 Union OAuth2 RSA 签名密钥对（不自动开启任何高危远端回调开关）。

