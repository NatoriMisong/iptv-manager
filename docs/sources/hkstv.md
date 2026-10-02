# 香港卫视直播来源

验证日期：2026-10-02；接入版本：0.2.5。

官网直播页：<https://hkstv.tv/live>。

HTTPS HLS 主清单：<https://webcast.hkstv.tv/livestream/mutfysrq/playlist.m3u8>。

## 在项目中使用

打开“添加频道 → 内置直播源 → 香港卫视”，勾选添加。默认服务器中继，可改为客户端直连；代理继承全局设置，添加后可逐频道修改。重复添加跳过已有地址，不覆盖原播放设置。

作为 `builtin` 内置来源保存（`provider_id=hkstv`、`provider_channel_id=mutfysrq`），目录和请求规则由 `internal/provider/hkstv.go` 维护，媒体传输复用共用层。不调用 yt-dlp 或 TVB 接口，不额外引入解析进程、Cookie 会话、定时请求或转码。频道保存上面的主清单地址；上游通过主清单自动提供带 `hls_ctx` 参数的子清单和分片，不应把这类临时子清单地址保存为频道来源。

单独使用 VLC 时，也可打开 [原始 M3U 列表](hkstv.m3u)。该文件直接访问上游，不经过 IPTV Manager，也不使用项目的播放令牌或代理配置。

## 官网获取流程

1. 官网直播脚本读取 `GET https://hkstv.tv/services/live/default`，本次返回频道“香港卫视”、`sourceid=mutfysrq`。
2. 请求 `GET https://hkstv.tv/lives/api/player/mutfysrq?mode=live&protocal=hls`。`protocal` 为官网使用的参数拼写。
3. 接口返回 HTTP 的 `webcast.hkstv.tv/livestream/mutfysrq/playlist.m3u8`；播放服务器会跳转到 HTTPS。本项目直接保存验证过的 HTTPS 地址。

当前主清单 URL 不带认证或签名参数，但不能保证上游以后不改变地址。内置地址随项目维护；如果官网更换来源，应更新该模块中的目录，或另建一个通用直播频道使用自定义 URL；已添加的内置频道来源字段只读。

## 网络验证边界

- HTTPS 直连：主清单、媒体子清单均返回 HTTP 200；视频分片返回 200 并收到约 2.7 MB 数据，但本机在 30 秒内未完整下载，最终超时。
- 使用用户指定的 SOCKS5 代理：主清单、媒体子清单和一个完整分片均返回 HTTP 200。该分片为 `video/MP2T`，响应长度 4,459,924 字节，完整接收。
- 此次清单和分片请求没有 Cookie、Referer 或其他自定义认证头；媒体清单未声明加密密钥。
- 项目接入时发现，上游对 `Go-http-client/1.1` 返回 502，改用 `Mozilla/5.0` 返回正常清单；用 curl 和 Go 分别对照确认。通用中继现在为清单、分片等请求设置普通浏览器 User-Agent，仍允许已有来源请求头覆盖该默认值。
- 本机 HTTP 直连曾被网络重定向到拦截页，因此优先使用上面的 HTTPS 地址；目标服务器的可用性仍取决于实际出口。

项目内的真实验证另使用临时 SQLite 实例，经登录、内置目录和批量导入 API 添加该频道，重复添加跳过。直连入口返回 307 至原始主清单；通过配置的代理，从本服务取得主清单、子清单和一个完整分片，均返回 200，该分片为 3,994,812 字节。子清单和分片地址均改写为本服务地址，临时参数保留在服务端资源引用中；未启动 YouTube 或 TVB 解析。

以上验证覆盖一次短请求链路，不代表已在 VLC 中验证解码、音视频同步、长期稳定性或 `hls_ctx` 到期后的连续播放。必要时重新打开频道，从主清单取得新会话。

原始接口响应、临时会话参数和探测脚本仅保留在 Git 与 Docker 忽略的 `local-test/` 中；开发代理地址不写入项目配置或发布清单。
