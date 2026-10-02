package provider

func tdmDefinition() definition {
	return definition{catalog: Catalog{
		ID:           "tdm",
		Name:         "澳视澳门",
		Website:      "https://www2.tdm.com.mo/zh-hant/live?Channel=1&type=tv",
		Description:  "来自澳广视（TDM）官网的电视直播地址。会议直播默认不选中，可按需添加。",
		SourceType:   "builtin",
		DefaultMode:  "inherit",
		LinkLabel:    "原始 M3U8 ↗",
		PlaybackHelp: "支持直连或中继，原始画质由播放器选择。",
		Channels: []Channel{
			{ID: "ctvp", Name: "澳視澳門 91台", URL: "https://live3.tdm.com.mo/ch1/ch1.live/playlist.m3u8", Selected: true, Note: ""},
			{ID: "ptvp", Name: "澳視葡文 92台", URL: "https://live3.tdm.com.mo/ch2/ch2.live/playlist.m3u8", Selected: true, Note: ""},
			{ID: "info", Name: "澳門資訊 94台", URL: "https://live3.tdm.com.mo/ch5/info_ch5.live/playlist.m3u8", Selected: true, Note: ""},
			{ID: "sports", Name: "澳門體育 93台", URL: "https://live3.tdm.com.mo/ch4/sport_ch4.live/playlist.m3u8", Selected: true, Note: ""},
			{ID: "hdctvp", Name: "澳門綜藝 95台", URL: "https://live3.tdm.com.mo/ch6/hd_ch6.live/playlist.m3u8", Selected: true, Note: ""},
			{ID: "satellite", Name: "澳門-Macau 96台", URL: "https://live3.tdm.com.mo/ch3/ch3.live/playlist.m3u8", Selected: true, Note: ""},
			{ID: "cgtn71", Name: "CCTV 綜合頻道 71台", URL: "https://live3.tdm.com.mo/cgtn/cgtn71/playlist.m3u8", Selected: true, Note: ""},
			{ID: "cgtn", Name: "CGTN 73台", URL: "https://live3.tdm.com.mo/cgtn/cgtn73/playlist.m3u8", Selected: true, Note: ""},
			{ID: "cgtn74", Name: "CGTN 紀錄頻道 74台", URL: "https://live3.tdm.com.mo/cgtn/cgtn74/playlist.m3u8", Selected: true, Note: ""},
			{ID: "legislativec", Name: "立法會直播", URL: "https://live3.tdm.com.mo/tv/ch21.live/playlist.m3u8", Selected: false, Note: "会议直播，节目安排以官网为准"},
			{ID: "legislativep", Name: "Directo das Reuniões da Assembleia de Macau", URL: "https://live3.tdm.com.mo/tv/ch22.live/playlist.m3u8", Selected: false, Note: "会议直播，节目安排以官网为准"},
		},
	},
		create: func(Options) resolver { return fixedSource{} },
	}
}
