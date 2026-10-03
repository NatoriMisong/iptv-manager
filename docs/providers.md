# 网站直播来源开发说明

从 0.3.0 起，网站来源统一以 `source_type=builtin` 保存，使用 `provider_id` 和 `provider_channel_id` 标识供应方与频道。频道数据库 ID 仍是独立的随机 ID，决定固定播放入口；显示名称、分组、播放方式和代理不参与来源身份匹配。从 0.4.0 起，网站来源频道不再有逐频道代理，`settings.provider_proxies[provider_id]` 为整个来源选择一个命名代理或直连，`core.EffectiveProxy` 对网站来源频道按此解析。

## 文件分工

| 文件 | 职责 |
|---|---|
| `internal/provider/tdm.go` | 澳广视目录、官网域名接口和按出口改写直播域名 |
| `internal/provider/hkstv.go` | 香港卫视目录、固定 HLS 来源和浏览器 User-Agent |
| `internal/provider/tvb.go` | TVB 目录、官网接口、签名地址、Cookie 会话、有效期和请求规则 |
| `internal/provider/provider.go` | 目录、解析结果、会话接口和静态来源共用实现 |
| `internal/provider/registry.go` | 统一注册、身份验证、旧来源匹配及分发 |
| `internal/media/provider.go` | 通用会话中继、按原音视频轨道恢复过期清单 |
| `internal/store/providers.go` | 网站来源目录导入与去重身份 |
| `internal/store/proxies.go` | 命名代理的保存、删除保护和来源代理设置 |

HLS 改写、播放器鉴权、资源引用、缓存、并发限制和流量统计统一留在 `internal/media`；SQLite 与管理 API 不实现某个电视台的 Cookie 或解析规则。前端读取目录中的名称、说明、链接标签和播放提示，不判断某个供应方名称。

## 添加一个来源

1. 在 `internal/provider` 新增供应方 Go 文件，定义返回 `definition` 的函数。目录需要稳定的供应方 ID、频道 ID、名称、来源 URL、默认选中状态、默认播放方式及用户提示。
2. 固定公开 HLS 地址可以复用 `fixedSource`，按需提供请求头。它只返回目录里的 URL，不访问官网、不创建 Cookie 会话、不启动解析进程。
3. 需要动态接口或 Cookie 的来源，实现 `resolver` 的 `Resolve` 和 `Invalidate`，通过 `definition.create` 为当前服务创建实例。临时签名与认证信息只放内存，持久化 URL 使用官方来源页或稳定入口。
4. 在 `registry.go` 的 `definitions` 中注册一次。目录接口、添加页面、身份验证和播放分发会自动使用它，不需要新增前端来源类型或数据库枚举。
5. 添加供应方测试及必要的中继回归，更新来源文档。不要修改已经发布的来源 ID；频道名称和实际地址可以更新，播放时由目录提供最新地址。

网站来源添加 API 接收 `provider_id` 和 `channel_ids`，服务器从目录取名称与 URL。对已存在频道的编辑仅调整用户设置，来源身份不可切换；需要自定义 URL 时另建通用频道。

## 动态会话约定

`Playback.Session` 可为空；非空时，媒体层通过 `Session` 接口获取和转发资源，不直接操作 Cookie。供应方需要保证并发安全、请求超时、按频道与代理配置隔离，以及取消和撤销的有效性。

- `Resolve` 按请求复用或建立会话，合并同频道的并发初始化。`config` 是共用媒体层生成的配置指纹；代理或来源配置变化后不能沿用旧会话。
- `Active` 表示该会话仍是当前可用会话；`Expired` 判断时间和认证状态。`CanRefresh` 表示旧引用仍有资格定位后继会话，手动清除后必须返回 false。
- `Reject` 标记认证失效。共用媒体层只对可刷新的清单在 401/403 后尝试更新一次；供应方负责限制持续失败的请求频率。
- `ValidateURL` 和 `Fetch` 负责供应方的域名、重定向、Cookie 作用域及请求头规则。保留 TLS 验证，直连检查实际目标地址；代理由共用媒体层按来源设置解析后传入，供应方不读取代理配置。
- `Invalidate(channelID)` 撤销对应频道的地址、Cookie 和正在执行的旧请求。共用层同时清理动态会话的媒体引用和频道缓存，迟到的响应不能恢复已撤销认证。

不增加后台定时拉流或 Cookie 刷新；首次播放及过期后的请求按需获取。现有 Session 适用于 HLS 主/子清单和可按语义匹配的音视频轨道，其他播放协议需要单独设计。

## 升级与测试

数据库和备份只有当前版本 5，没有自动升级。`UpgradeLegacy` 用于批量添加时识别与网站来源目录相同的通用地址，避免重复添加。版本 5 备份导入时验证注册身份、代理引用和来源代理设置，失败不做部分更新。

使用模拟 HTTP 来源检查目录有效性、非法身份、直连和中继、请求头、代理、刷新撤销及重复添加；动态来源再检查 Cookie 隔离、到期恢复、并发和失败限次。迁移测试检查固定频道 ID、播放设置、订阅、密码、流量和回滚。真实网络验证只证明当次请求可用，不能替代 VLC 解码和长期播放测试。

本地工具、Cookie、签名 URL、数据库、代理设置和探测结果放在忽略目录 `local-test/`，不得提交或进入 Docker 构建上下文。
