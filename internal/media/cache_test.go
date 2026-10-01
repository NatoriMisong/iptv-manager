package media

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestCacheBudgetIncludesCapacityAndMetadata(t *testing.T) {
	c := newSegmentCache(2048)
	data := make([]byte, 1, 4096)
	c.put(cachedSegment{key: "oversized-capacity", data: data})
	if _, ok := c.get("oversized-capacity"); ok {
		t.Fatal("backing array capacity must count against memory budget")
	}
	for i := 0; i < 100; i++ {
		c.put(cachedSegment{key: fmt.Sprint(i), data: []byte{1}, header: http.Header{"Content-Type": {"video/mp2t"}}})
	}
	if c.used > c.max || len(c.items) >= 100 {
		t.Fatal("small segments bypassed metadata budget")
	}
}

func TestCacheEntryCapAndExpiredEviction(t *testing.T) {
	c := newSegmentCache(32 * 1024 * 1024)
	for i := 0; i < 600; i++ {
		c.put(cachedSegment{key: fmt.Sprint(i), data: []byte{1}})
	}
	if len(c.items) > 512 {
		t.Fatal("cache entry count is unbounded")
	}
	for _, e := range c.items {
		v := e.Value.(cachedSegment)
		v.expires = time.Now().Add(-time.Second)
		e.Value = v
	}
	c.put(cachedSegment{key: "new", data: []byte{2}})
	if len(c.items) != 1 {
		t.Fatal("expired entries were not cleaned")
	}
}

func TestUpstreamURLBoundary(t *testing.T) {
	for _, raw := range []string{"https://manifest.googlevideo.com/api/live", "https://rr1---sn.googlevideo.com/videoplayback", "https://www.youtube.com/api/manifest"} {
		if err := ValidateUpstream(raw); err != nil {
			t.Errorf("rejected valid URL %q", raw)
		}
	}
	for _, raw := range []string{"http://manifest.googlevideo.com/api/live", "https://googlevideo.com.evil.example/a", "https://127.0.0.1/a", "https://googlevideo.com@evil.example/a", "file:///etc/passwd", "https://googlevideo.com:444/a"} {
		if err := ValidateUpstream(raw); err == nil {
			t.Errorf("accepted unsafe URL %q", raw)
		}
	}
}
