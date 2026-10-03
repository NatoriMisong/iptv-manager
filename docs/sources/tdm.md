# 澳广视（TDM）电视直播来源

来源：[官方直播页面](https://www2.tdm.com.mo/zh-hant/live?Channel=1&type=tv) 和页面调用的 [官方频道接口](https://www2.tdm.com.mo/api/v1.0/live/categories)。以下地址均取自接口的 `onlineFeed` 字段，是官网公开提供的原始 M3U8，不经过 YouTube 解析器。

可以使用 [完整 M3U 列表](tdm.m3u)，或在管理页依次选择“添加频道 → 内置直播源 → 澳视澳门”。

| 官方频道名称 | 原始 M3U8 |
|---|---|
| 澳視澳門 91台 | [https://live3.tdm.com.mo/ch1/ch1.live/playlist.m3u8](https://live3.tdm.com.mo/ch1/ch1.live/playlist.m3u8) |
| 澳視葡文 92台 | [https://live3.tdm.com.mo/ch2/ch2.live/playlist.m3u8](https://live3.tdm.com.mo/ch2/ch2.live/playlist.m3u8) |
| 澳門資訊 94台 | [https://live3.tdm.com.mo/ch5/info_ch5.live/playlist.m3u8](https://live3.tdm.com.mo/ch5/info_ch5.live/playlist.m3u8) |
| 澳門體育 93台 | [https://live3.tdm.com.mo/ch4/sport_ch4.live/playlist.m3u8](https://live3.tdm.com.mo/ch4/sport_ch4.live/playlist.m3u8) |
| 澳門綜藝 95台 | [https://live3.tdm.com.mo/ch6/hd_ch6.live/playlist.m3u8](https://live3.tdm.com.mo/ch6/hd_ch6.live/playlist.m3u8) |
| 澳門-Macau 96台 | [https://live3.tdm.com.mo/ch3/ch3.live/playlist.m3u8](https://live3.tdm.com.mo/ch3/ch3.live/playlist.m3u8) |
| CCTV 綜合頻道 71台 | [https://live3.tdm.com.mo/cgtn/cgtn71/playlist.m3u8](https://live3.tdm.com.mo/cgtn/cgtn71/playlist.m3u8) |
| CGTN 73台 | [https://live3.tdm.com.mo/cgtn/cgtn73/playlist.m3u8](https://live3.tdm.com.mo/cgtn/cgtn73/playlist.m3u8) |
| CGTN 紀錄頻道 74台 | [https://live3.tdm.com.mo/cgtn/cgtn74/playlist.m3u8](https://live3.tdm.com.mo/cgtn/cgtn74/playlist.m3u8) |
| 立法會直播 | [https://live3.tdm.com.mo/tv/ch21.live/playlist.m3u8](https://live3.tdm.com.mo/tv/ch21.live/playlist.m3u8) |
| Directo das Reuniões da Assembleia de Macau | [https://live3.tdm.com.mo/tv/ch22.live/playlist.m3u8](https://live3.tdm.com.mo/tv/ch22.live/playlist.m3u8) |

目录收录接口中的 11 路电视，不含电台分类。两路立法会频道属于会议直播，是否有节目以官网安排为准，在内置来源选择器中默认不勾选。

## 在项目中使用

频道以 `builtin` 类型和 `provider_id=tdm` 保存，来源字段只读；目录在 `internal/provider/tdm.go` 中随程序版本维护。添加的频道继承全局代理，可以在频道编辑中单独设置。官网若更改地址，应更新模块目录，或另建通用频道使用自定义地址。

## 已知限制

- 地址取自官方接口，但直播 CDN（`live3.tdm.com.mo`）对部分网络出口会连接超时。遇到超时时不应判断为地址失效或需要 Cookies，先在实际服务器网络上确认主清单、子清单和分片能否读取。
- 部分开发网络的本地 DNS 结果与公网 DNS 不一致；项目只保存域名地址，不内置固定 IP、代理或 Cookies，也不关闭 TLS 校验。
