# 香港电台（RTHK）电视直播来源

提取日期：2026-10-02。

来源：[港台电视官网](https://www.rthk.hk/tv) 及其加载的 [电视播放器脚本](https://www.rthk.hk/js/page/main_tv.js?v=1.2.17)。网页列出 31、32、33、34、35 台的播放入口；脚本中还保留了 36 台的分支。

## 可导入的候选列表

[rthk.m3u](rthk.m3u) 收录 33、34、35 台这三路官网当前直接使用的 HLS 地址。可以在 IPTV Manager 中以通用直播来源添加这些地址，也可以把发布后的 M3U 文件地址添加为订阅，不经过 YouTube 解析器。

**地址提取成功，尚未验证可播。** 本次经用户指定的 SOCKS5 代理读取官网和脚本成功，但下列六个 CDN 地址均返回 `HTTP 403 Forbidden`，响应为 Akamai 的 `Access Denied` 网页，没有取得有效 HLS 清单。33 台补上浏览器 User-Agent、官网 Referer 和 Origin 后仍为 403；未下载媒体分片，也未进行 VLC 实播。

仅凭 403 无法确定是出口地区、CDN 访问策略还是其他限制。官网明确提示部分节目只限香港播放，但不能据此认定本次所有 403 都是地区限制。把地址写成 M3U、添加请求头或通过本项目中继，并不能保证解决上游拒绝访问。

| 频道 | 官网播放方式及本次收录情况 | 原始 HLS 地址 |
|---|---|---|
| 港台電視 31 | 官网配置 FairPlay / Widevine / PlayReady DRM。普通 M3U 与本项目的通用中继无法提供官网所需的 DRM 播放能力，未收录。 | [M3U8](https://rthktv31-vos-live.akamaized.net/Content/HLS/Live/channel(rthk_ch31)/index.m3u8) |
| 港台電視 32 | 脚本保留此 HLS 地址，但当前播放分支使用带认证令牌的专用 Web SDK，没有把该地址交给 HLS 播放器。不能确认该地址仍供普通播放器使用，未收录。 | [M3U8](https://rthktv32-vos-live.akamaized.net/Content/HLS/Live/channel(rthk_ch32)/index.m3u8) |
| 港台電視 33 | 官网直接传入 HLS 播放器，收录为候选来源。 | [M3U8](https://rthktv33-vos-live.akamaized.net/Content/HLS/Live/channel(rthk_ch33)/index.m3u8) |
| 港台電視 34 | 官网直接传入 HLS 播放器，收录为候选来源。 | [M3U8](https://rthktv34-vos-live.akamaized.net/Content/HLS/Live/channel(rthk_ch34)/index.m3u8) |
| 港台電視 35 | 官网直接传入 HLS 播放器，收录为候选来源。 | [M3U8](https://rthktv35-vos-live.akamaized.net/Content/HLS/Live/channel(rthk_ch35)/index.m3u8) |
| 36 台脚本条目 | 脚本包含 HLS 播放分支，但此次首页没有对应播放入口，未收录，也不据此判断频道在播。 | [M3U8](https://rthktv36-vos-live.akamaized.net/Content/HLS/Live/channel(rthk_ch36)/index.m3u8) |

候选列表未加入应用内置来源目录。此次仅整理公开来源，不调用 DRM 许可证接口，也不保存或发布认证令牌。原始网页、响应、抓取脚本与测试结果保留在被 Git 和 Docker 忽略的 `local-test/`；开发代理地址未写入列表或应用配置。

在实际使用网络上，应依次确认主清单、子清单和媒体分片能够读取，再用 VLC 检查画面、声音与连续播放。客户端直连使用客户端的网络出口；选择中继后，使用服务器为该频道配置的出口。
