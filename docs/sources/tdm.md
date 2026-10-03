# 澳广视（TDM）电视直播来源

来源：[官方直播页面](https://www.tdm.com.mo/zh-hant/live?Channel=1&type=tv) 和页面调用的 [官方频道接口](https://www.tdm.com.mo/api/v1.0/live/categories)。以下地址均取自接口的 `onlineFeed` 字段，是官网公开提供的原始 M3U8，不经过 YouTube 解析器。

## 直播域名按出口切换

官网播放器在设置地址前会再调用 `https://www.tdm.com.mo/api/v1/common/get-domain`。该接口按请求 IP 返回 `sourceLiveDomain`、`liveDomain` 和一张 `domains` 替换表，播放器先把地址里的 `sourceLiveDomain` 换成 `liveDomain`，再按表的顺序逐项替换一次。澳门以外的出口通常得到：

```text
live3.tdm.com.mo  →  locallive.tdm.com.mo  →  globallive.tdm.com.mo
```

`live3` 和 `locallive` 对境外出口分别表现为连接超时和 403，`globallive` 是华为云 CDN，可正常获取主清单、子清单和分片。项目在每次播放时通过该来源配置的代理调用同一接口并做同样的替换，结果按出口缓存一小时，接口失败时暂时沿用目录地址并在 30 秒后重试；手动“刷新来源”会立即清除缓存。

可以使用 [完整 M3U 列表](tdm.m3u)，或在管理页依次选择“添加频道 → 网站直播源 → 澳视澳门”。

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

目录只收录接口中的 11 路电视，不含电台分类。网站来源选择器默认勾选 91 至 96 台；三路 CGTN 和两路立法会会议直播默认不勾选，后者是否有节目以官网安排为准。

## 在项目中使用

频道以 `builtin` 类型和 `provider_id=tdm` 保存，来源字段只读，保存的是接口原始地址；播放时才改写域名。目录在 `internal/provider/tdm.go` 中随程序版本维护。代理在“网站直播源”页面为整个来源选择，域名接口和媒体请求使用同一出口。官网若更改地址，应更新模块目录，或另建通用频道使用自定义地址。

## 已知限制

- 目录和 [原始 M3U 列表](tdm.m3u) 中的 `live3.tdm.com.mo` 只对澳门本地出口可用；直接在播放器里使用该列表时，境外网络需把域名换成 `globallive.tdm.com.mo`。项目内的频道无需手动替换。
- 域名替换表由官网按请求 IP 决定，直连模式下播放器拿到的是服务器出口对应的地址；客户端所在地区能否访问该 CDN 以官网为准。
- 部分网络的本地 DNS 会把 `live3.tdm.com.mo` 解析到错误地址；项目只保存域名，不固定 IP、代理或 Cookies，也不关闭 TLS 校验。
