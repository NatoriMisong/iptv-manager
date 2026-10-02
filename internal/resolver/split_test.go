package resolver

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The reported live source has two audio-only formats with no acodec metadata
// and six video-only formats. Audio association comes from the original master.
func splitLiveData(master string) []byte {
	formats := []map[string]any{
		{"format_id": "233", "url": "https://manifest.googlevideo.com/audio-low.m3u8", "manifest_url": master, "protocol": "m3u8_native", "vcodec": "none", "height": 0},
		{"format_id": "234", "url": "https://manifest.googlevideo.com/audio-high.m3u8", "manifest_url": master, "protocol": "m3u8_native", "vcodec": "none", "height": 0},
	}
	for i, height := range []int{144, 240, 360, 480, 720, 1080} {
		formats = append(formats, map[string]any{"format_id": []string{"269", "229", "230", "231", "232", "270"}[i], "url": fmt.Sprintf("https://manifest.googlevideo.com/video-%d.m3u8?expire=1900000600", height), "manifest_url": master, "protocol": "m3u8_native", "vcodec": "avc1.4D401F", "acodec": "none", "height": height})
	}
	data, _ := json.Marshal(map[string]any{"is_live": true, "title": "split live", "formats": formats})
	return data
}

func TestReportedSplitLiveFormatsRespectQualityAndKeepMaster(t *testing.T) {
	master := "https://manifest.googlevideo.com/master.m3u8?expire=1900001200"
	for _, quality := range []int{360, 480, 720, 1080} {
		result, err := parseResult(splitLiveData(master), quality, time.Unix(1900000000, 0))
		if err != nil {
			t.Fatal(err)
		}
		if result.URL != master || result.Height != quality || !strings.Contains(result.VideoURL, fmt.Sprintf("video-%d.m3u8", quality)) {
			t.Fatalf("wrong split selection: %+v", result)
		}
		if result.ExpiresAt.Unix() != 1900000600 {
			t.Fatal("video expiry not included")
		}
	}
	if _, err := parseResult(splitLiveData(master), 100, time.Unix(1900000000, 0)); !errors.Is(err, ErrNoHLS) {
		t.Fatalf("quality limit bypassed: %v", err)
	}
}

func TestSplitSourceRejectsMissingForeignAndExpiredMaster(t *testing.T) {
	for _, master := range []string{"", "http://localhost/master.m3u8", "https://manifest.googlevideo.com/master.m3u8?expire=1800000000"} {
		if _, err := parseResult(splitLiveData(master), 720, time.Unix(1900000000, 0)); err == nil {
			t.Fatalf("accepted unusable master %q", master)
		}
	}
}

func TestMuxedSourceKeepsExistingSelectionWhenSplitAlsoAvailable(t *testing.T) {
	var info map[string]any
	_ = json.Unmarshal(splitLiveData("https://manifest.googlevideo.com/master.m3u8"), &info)
	info["formats"] = append(info["formats"].([]any), map[string]any{"url": "https://manifest.googlevideo.com/muxed.m3u8", "protocol": "m3u8_native", "height": 480, "vcodec": "avc1", "acodec": "mp4a"})
	data, _ := json.Marshal(info)
	result, err := parseResult(data, 720, time.Unix(1900000000, 0))
	if err != nil || result.VideoURL != "" || !strings.HasSuffix(result.URL, "/muxed.m3u8") {
		t.Fatalf("existing muxed path changed: %+v %v", result, err)
	}
}
