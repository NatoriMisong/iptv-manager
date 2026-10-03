# 香港卫视直播来源

官网直播页：<https://hkstv.tv/live>。

HTTPS HLS 主清单：<https://webcast.hkstv.tv/livestream/mutfysrq/playlist.m3u8>。

## 在项目中使用

打开“添加频道 → 网站直播源 → 香港卫视”，勾选添加。默认服务器中继，可改为客户端直连；代理继承全局设置，添加后可逐频道修改。重复添加跳过已有地址，不覆盖原播放设置。

作为 `builtin` 网站来源保存（`provider_id=hkstv`、`provider_channel_id=mutfysrq`），目录和请求规则由 `internal/provider/hkstv.go` 维护，媒体传输复用共用层。不调用 yt-dlp 或 TVB 接口，不额外引入解析进程、Cookie 会话、定时请求或转码。

频道保存上面的主清单地址。上游通过主清单自动提供带 `hls_ctx` 参数的子清单和分片，这类临时子清单地址不应保存为频道来源；会话到期后重新打开频道，从主清单取得新会话。

单独使用 VLC 时，也可打开 [原始 M3U 列表](hkstv.m3u)。该文件直接访问上游，不经过 IPTV Manager，也不使用项目的播放令牌或代理配置。

## 官网获取流程

1. 官网直播脚本读取 `GET https://hkstv.tv/services/live/default`，返回频道“香港卫视”、`sourceid=mutfysrq`。
2. 请求 `GET https://hkstv.tv/lives/api/player/mutfysrq?mode=live&protocal=hls`。`protocal` 为官网使用的参数拼写。
3. 接口返回 HTTP 的 `webcast.hkstv.tv/livestream/mutfysrq/playlist.m3u8`；播放服务器会跳转到 HTTPS。本项目直接保存 HTTPS 地址。

当前主清单 URL 不带认证或签名参数，但不能保证上游以后不改变地址。来源地址随项目维护；如果官网更换来源，应更新该模块中的目录，或另建一个通用直播频道使用自定义 URL；已添加频道的来源字段只读。

## 已知限制

- 上游对 Go 默认的 `Go-http-client/1.1` User-Agent 返回 502，使用普通浏览器 User-Agent 返回正常清单。通用媒体中继因此统一设置浏览器 User-Agent，已有来源请求头可覆盖该默认值。
- 清单和分片请求不需要 Cookie、Referer 或其他认证头；媒体清单未声明加密密钥。
- 部分网络对 HTTP 直连会重定向到拦截页，应优先使用 HTTPS 地址；目标服务器的可用性仍取决于实际出口。
