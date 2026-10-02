# TVB 新闻直播 C 频道来源

提取日期：2026-10-02。

来源：[TVB 新闻直播页](https://news.tvb.com/tc/live/C)。页面使用 Nuxt，加载播放器后调用公开接口取得临时播放地址，HTML 本身不直接包含 M3U8。

## 官网播放流程

1. `GET https://news.tvb.com/app/public/live/channels`：取得频道目录。本次目录中 `C` 为「無綫新聞台」，`F` 为「直播｜財經頻道」；此次只验证用户指定的 C 频道。
2. `POST https://news.tvb.com/app/public/live/stream/C`：取得 `data.stream_url`、`expire_time`、`refresh_interval` 等字段。本次无需账户 Cookies 即可取得播放信息。
3. 访问返回的签名 M3U8，保留 CDN 下发的 `hdntl` Cookie，并在后续音视频子清单及分片请求中按 Cookie 的域名、路径和有效期正常发送、更新。
4. 读取清单引用的音轨、视频分片和标准 HLS AES-128 密钥。此次没有调用 DRM 许可证接口或进行转码。

本次接口识别代理出口为 `JP`，返回 `geo_blocked=false`、`stream_type=video`，并选择 `ott_I-NEWS_h264` 来源。其他出口可能得到不同结果，不能将这次结果视为所有地区均可播放。

返回地址的主机为 `prd-vcache.edge-global.akamai.tvb.com`，路径为 `/__cl/slocalr2526/__c/ott_I-NEWS_h264/__op/bks/__f/index.m3u8`，同时带有 `hdnea`、`p`、`mode` 查询参数。**路径本身不等于完整可用播放地址，应使用接口返回的完整 URL，不能删去签名。** 接口返回的建议刷新周期为 3600 秒，不应作为永久固定订阅地址保存。

## 本次验证结果

全部网络请求使用用户指定的 SOCKS5 代理；正式请求使用官网 Referer、Origin 和普通浏览器 User-Agent。

| 检查项 | 结果 |
|---|---|
| 官网及公开播放接口 | HTTP 200，取得 C 频道签名来源 |
| HLS 主清单 | HTTP 200，包含 1920×1080 / 50 fps 和 1280×720 / 50 fps 两档视频、两条音轨 |
| 不保存 CDN Cookie 请求子清单 | 视频和音轨均返回 HTTP 403 |
| 保留 CDN Cookie 请求子清单 | 720p 视频及默认音轨均 HTTP 200，取得有效 HLS |
| 清单加密方式 | `METHOD=AES-128`，默认 `KEYFORMAT=identity` |
| 音视频密钥请求 | HTTP 200，响应长度均为 16 字节；内容直接丢弃，未发布 |
| 初次 Range 分片探测 | HTTP 403，尚不能据此确定唯一原因 |
| 刷新子清单及 Cookie 后，正常完整 GET 分片 | 默认音轨 HTTP 200 / 78,400 字节；720p 视频 HTTP 200 / 2,256,576 字节；响应直接丢弃 |

上述结果确认主清单、子清单及少量实际媒体数据可以取得；未启动 VLC，未验证解码、音视频同步、连续播放或签名到期后恢复，也没有请求 1080p 视频分片。

## 在 IPTV Manager 中使用的限制

当前通用来源处理固定 URL，没有维护上游 Cookie 会话，也不会定时调用上述 POST 接口更新 TVB 签名。因此直接把这次临时地址保存为普通中继频道，仍可能在子清单或分片阶段收到 403，不能当作已适配的内置来源。

稳定接入需要增加 TVB 的按需解析与过期刷新，并让同一播放会话的主清单、子清单及媒体请求共享和更新符合域名范围的上游 Cookie。直连还取决于播放器的 Cookie 支持和客户端网络出口。

完整临时播放地址、用于 VLC 测试的 M3U、Cookie 和探测响应仅保留在被 Git 与 Docker 忽略的 `local-test/tmp/tvb/`，不上传 GitHub。VLC 测试清单附带 Referer / User-Agent 提示，这些扩展也不等于当前项目通用 M3U 导入支持自定义请求头。此次仅提取并记录来源，未修改应用播放逻辑或内置频道目录。
