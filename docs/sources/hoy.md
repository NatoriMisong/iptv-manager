# HOY 76、77、78

| 网站来源频道 | provider_channel_id | 官方接口内部 ID | 保存的官网页 |
|---|---|---|---|
| HOY 76 | 76 | 1 | <https://hoy.tv/live?channel_no=76> |
| HOY 77 | 77 | 2 | <https://hoy.tv/live?channel_no=77> |
| HOY 78 | 78 | 3 | <https://hoy.tv/live?channel_no=78> |

管理页“添加频道 → 网站直播源 → HOY”默认勾选三路并采用中继，代理继承全局配置。数据库使用 `source_type=builtin`、`provider_id=hoy`，保存上述频道标识与官网页，不保存临时 CDN URL。重复导入保留原设置，配置备份可恢复身份和固定播放入口。添加不访问官网；来源解析、签名和会话均由 `internal/provider/hoy.go` 处理，HLS 中继继续共用媒体层。

## 官网获取流程

1. `GET https://api2.hoy.tv/api/v3/a/channel?orientation=landscape` 返回目录；`videos.id` 是频道号，外层 `id` 是播放接口所用 ID。项目固定提供上述三路。
2. `POST https://api2.hoy.tv/api/v3/a/liveCheckout/{内部ID}`，`Content-Type: application/json`，无请求体，无需登录或预先 Cookie。
3. 响应的 `data.video.link` 是 `https://ch{频道号}-live-stream.hoy.tv/ch{频道号}/index-fhd.m3u8`；`data.signedUrl` 是完整签名地址；`data.signed` 提供 `CloudFront-Policy`、`CloudFront-Key-Pair-Id`、`CloudFront-Signature`。
4. 官网播放器给后续媒体请求附加 `Policy`、`Key-Pair-Id`、`Signature`。项目核对返回的内外频道 ID、HLS 地址、Policy 目录及有效期，再用 `data.video.link` 和 `data.signed` 生成地址；所有资源请求都替换为当前会话签名，保留其他查询参数。不会仅给根清单签名，也不盲用响应内其他 URL 或认证头。

官网使用 `channel_no` 参数；`channel_noa` 返回空频道数据。接口同时下发 `.hoy.tv` 的 CloudFront Cookie；项目按官网播放器的查询参数方式认证，不存储或转发这些跨频道同名 Cookie。

参考：[官方目录](https://api2.hoy.tv/api/v3/a/channel?orientation=landscape)、[播放器脚本](https://hoy.tv/_next/static/chunks/959.1baf8d4672e6af17.js)。脚本文件名可能随官网发布变化。

## 会话和网络边界

- 每个已保存频道及其代理配置独立维护签名；同频道并发首次播放合并为一次解析，最多同时初始化两路、保留 64 个会话。没有后台定时刷新。
- 签名只保存在内存；有效期取 Policy 到期前 30 秒与一小时上限的最早值。失败缓存 15 秒。清单 401/403 最多重新解析并重试一次，持续失败有冷却，避免地区限制导致接口请求循环。
- 过期的音视频清单由共用媒体层按原音轨、画质定位新清单；旧分片和密钥不替换为其他资源。网页刷新清除会话及资源缓存，取消旧请求；代理变更后也不会复用旧会话。
- HTTPS 媒体资源及其重定向仅允许当前频道的 CDN 主机和 `/ch{频道号}/` 目录；拒绝其他频道、其他站点和目录穿越，不向其发送签名。接口不跟随重定向。直连保留实际目标 IP 检查，显式出站代理沿用项目的信任边界，TLS 证书校验保持启用。
- HTTP 连接、请求取消和脱敏网络错误分类从 TVB 提取到 `internal/provider/http.go` 共用；TVB 自身的 Cookie 和 TLS 握手限次重试逻辑保留。
- 中继不转码、不解密、不重新切片。直连仅返回带签名主清单的 HTTP 307；后续签名传递由播放器负责，未验证 VLC 直连可用，默认推荐中继。

## 已知限制

- 官方播放接口返回 200 和有效签名，不代表直播 CDN 允许当前出口。三个频道的清单都可能返回地区限制 403，正文为 `The Amazon CloudFront distribution is configured to block access from your country.` 需要使用来源允许的地区出口；刷新签名不能解除地区限制。
- 尚未验证真实媒体播放，画质、音轨、VLC 解码和长期稳定性需要在实际环境中验收。模拟源中的音轨和 AES-128 密钥用于检查中继能力，不表示真实 HOY 清单一定包含这些资源。
- 签名过期和拒绝访问后的恢复由模拟来源测试覆盖；真实签名到期后能否连续播放，取决于播放器行为，必要时重新打开频道。
- 发布配置及网站来源目录不包含开发代理、临时签名或账户 Cookie。
