package provider

func hkstvDefinition() definition {
	return definition{catalog: Catalog{
		ID:           "hkstv",
		Name:         "香港卫视",
		Website:      "https://hkstv.tv/live",
		Description:  "香港卫视官网直播，默认服务器中继，也可选择客户端直连。代理继承全局设置，添加后可逐频道修改。",
		SourceType:   "builtin",
		DefaultMode:  "relay",
		LinkLabel:    "原始 M3U8 ↗",
		PlaybackHelp: "支持直连或中继，原始画质由播放器选择。",
		Channels: []Channel{
			{ID: "mutfysrq", Name: "香港卫视", URL: "https://webcast.hkstv.tv/livestream/mutfysrq/playlist.m3u8", Selected: true, Note: "官网 HTTPS HLS 直播"},
		},
	},
		create: func(Options) resolver { return fixedSource{headers: map[string]string{"User-Agent": "Mozilla/5.0"}} },
	}
}
