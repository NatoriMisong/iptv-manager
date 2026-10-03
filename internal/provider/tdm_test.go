package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"iptv-manager/internal/core"
)

const tdmDomainBody = `{"message":"OK","data":{"domains":{"https://live3.tdm.com.mo":"https://live5.tdm.com.mo","https://locallive.tdm.com.mo":"https://globallive.tdm.com.mo"},"liveDomain":"https://locallive.tdm.com.mo","sourceLiveDomain":"https://live3.tdm.com.mo","ip":"203.0.113.9"},"code":0,"type":"Success"}`

type tdmFixture struct {
	mu      sync.Mutex
	calls   []string // proxy URL per API call
	body    string
	fail    bool
	clock   time.Time
	factory func(string) (*http.Client, error)
}

func newTDMFixture(t *testing.T) *tdmFixture {
	t.Helper()
	f := &tdmFixture{body: tdmDomainBody, clock: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	f.factory = func(proxy string) (*http.Client, error) {
		return &http.Client{Transport: tvbTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() != tdmDomainAPI || r.Method != http.MethodGet {
				t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			}
			f.mu.Lock()
			f.calls = append(f.calls, proxy)
			fail, body := f.fail, f.body
			f.mu.Unlock()
			if fail {
				return nil, errors.New("dial tcp: connection refused")
			}
			w := httptest.NewRecorder()
			w.Header().Set("Content-Type", "application/json")
			w.WriteString(body)
			resp := w.Result()
			resp.Request = r
			return resp, nil
		})}, nil
	}
	return f
}

func (f *tdmFixture) resolver() *tdmResolver {
	return newTDMResolver(f.factory, func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.clock })
}

func (f *tdmFixture) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

func tdmChannel(id string) core.Channel {
	item, _ := ChannelInfo("tdm", id)
	return core.Channel{ID: "db-" + id, SourceType: "builtin", ProviderID: "tdm", ProviderChannelID: id, URL: item.URL}
}

func TestTDMRewritesLiveDomainPerEgressAndCaches(t *testing.T) {
	f := newTDMFixture(t)
	m := f.resolver()
	ch := tdmChannel("ctvp")
	for i := 0; i < 3; i++ {
		p, err := m.Resolve(context.Background(), ch, core.Settings{}, "cfg")
		if err != nil || p.Session != nil || p.URL != "https://globallive.tdm.com.mo/ch1/ch1.live/playlist.m3u8" {
			t.Fatalf("resolve %d = %+v, %v", i, p, err)
		}
	}
	other, err := m.Resolve(context.Background(), tdmChannel("cgtn"), core.Settings{}, "cfg")
	if err != nil || other.URL != "https://globallive.tdm.com.mo/cgtn/cgtn73/playlist.m3u8" {
		t.Fatalf("second channel = %+v, %v", other, err)
	}
	if f.count() != 1 {
		t.Fatalf("API called %d times for one egress", f.count())
	}
	settings := core.Settings{Proxies: []core.Proxy{{ID: "0123456789abcdef01234567", Name: "p", Scheme: "socks5", Host: "proxy.example", Port: 1080}}, ProviderProxies: map[string]string{"tdm": "0123456789abcdef01234567"}}
	if p, err := m.Resolve(context.Background(), ch, settings, "cfg2"); err != nil || p.URL != "https://globallive.tdm.com.mo/ch1/ch1.live/playlist.m3u8" {
		t.Fatalf("proxied resolve = %+v, %v", p, err)
	}
	f.mu.Lock()
	calls := append([]string(nil), f.calls...)
	f.mu.Unlock()
	if len(calls) != 2 || calls[0] != "" || calls[1] != "socks5://proxy.example:1080" {
		t.Fatalf("API egress per call = %v", calls)
	}
	// The table expires after an hour; a manual refresh drops it immediately.
	f.mu.Lock()
	f.clock = f.clock.Add(tdmDomainTTL + time.Second)
	f.mu.Unlock()
	_, _ = m.Resolve(context.Background(), ch, core.Settings{}, "cfg")
	if f.count() != 3 {
		t.Fatalf("expired table not refreshed: %d calls", f.count())
	}
	m.Invalidate(ch.ID)
	_, _ = m.Resolve(context.Background(), ch, core.Settings{}, "cfg")
	if f.count() != 4 {
		t.Fatalf("invalidate did not clear the table: %d calls", f.count())
	}
}

func TestTDMReplacementOrderAndLocalTable(t *testing.T) {
	// Entries apply in response order after the source/live swap, exactly like the website.
	for name, test := range map[string]struct{ body, want string }{
		"global": {tdmDomainBody, "https://globallive.tdm.com.mo/ch1/ch1.live/playlist.m3u8"},
		"local":  {`{"code":0,"data":{"domains":{},"liveDomain":"https://live3.tdm.com.mo","sourceLiveDomain":"https://live3.tdm.com.mo"}}`, "https://live3.tdm.com.mo/ch1/ch1.live/playlist.m3u8"},
		"null":   {`{"code":0,"data":{"domains":null,"liveDomain":"https://live5.tdm.com.mo","sourceLiveDomain":"https://live3.tdm.com.mo"}}`, "https://live5.tdm.com.mo/ch1/ch1.live/playlist.m3u8"},
		"chain":  {`{"code":0,"data":{"domains":{"https://live4.tdm.com.mo":"https://live6.tdm.com.mo","https://live3.tdm.com.mo":"https://live4.tdm.com.mo"},"liveDomain":"https://live3.tdm.com.mo","sourceLiveDomain":"https://live3.tdm.com.mo"}}`, "https://live4.tdm.com.mo/ch1/ch1.live/playlist.m3u8"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newTDMFixture(t)
			f.body = test.body
			p, err := f.resolver().Resolve(context.Background(), tdmChannel("ctvp"), core.Settings{}, "cfg")
			if err != nil || p.URL != test.want {
				t.Fatalf("resolve = %+v, %v", p, err)
			}
		})
	}
}

func TestTDMFallsBackToCatalogAddressOnFailureOrUnsafeTable(t *testing.T) {
	for name, body := range map[string]string{
		"network":      "",
		"foreign-host": `{"code":0,"data":{"domains":{"https://live3.tdm.com.mo":"https://evil.invalid"},"liveDomain":"https://live3.tdm.com.mo","sourceLiveDomain":"https://live3.tdm.com.mo"}}`,
		"http-scheme":  `{"code":0,"data":{"domains":{},"liveDomain":"http://live3.tdm.com.mo","sourceLiveDomain":"https://live3.tdm.com.mo"}}`,
		"error-code":   `{"code":1,"data":null}`,
		"not-json":     `<html>blocked</html>`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newTDMFixture(t)
			f.body = body
			f.fail = body == ""
			m := f.resolver()
			ch := tdmChannel("ctvp")
			for i := 0; i < 2; i++ {
				p, err := m.Resolve(context.Background(), ch, core.Settings{}, "cfg")
				if err != nil || p.URL != ch.URL || strings.Contains(p.URL, "evil") {
					t.Fatalf("fallback = %+v, %v", p, err)
				}
			}
			if f.count() != 1 {
				t.Fatalf("failure not cached briefly: %d calls", f.count())
			}
			f.mu.Lock()
			f.clock = f.clock.Add(tdmFailureTTL + time.Second)
			f.mu.Unlock()
			_, _ = m.Resolve(context.Background(), ch, core.Settings{}, "cfg")
			if f.count() != 2 {
				t.Fatal("failure cache did not expire")
			}
		})
	}
}

func TestTDMTableParsingRejectsOversizedOrMalformedInput(t *testing.T) {
	entries := make([]string, 0, 25)
	for i := 0; i < 25; i++ {
		entries = append(entries, `"https://a`+strings.Repeat("b", i)+`.tdm.com.mo":"https://c.tdm.com.mo"`)
	}
	big := `{"code":0,"data":{"domains":{` + strings.Join(entries, ",") + `},"liveDomain":"https://live3.tdm.com.mo","sourceLiveDomain":"https://live3.tdm.com.mo"}}`
	for name, body := range map[string]string{"oversized": big, "array": `{"code":0,"data":{"domains":["x"]}}`, "non-string": `{"code":0,"data":{"domains":{"https://live3.tdm.com.mo":1}}}`} {
		if _, err := parseTDMDomains([]byte(body)); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	table, err := parseTDMDomains([]byte(tdmDomainBody))
	if err != nil || len(table.replacements) != 2 || table.replacements[0][0] != "https://live3.tdm.com.mo" || table.replacements[1][1] != "https://globallive.tdm.com.mo" {
		t.Fatalf("parsed table = %+v, %v", table, err)
	}
	var generic map[string]any
	if json.Unmarshal([]byte(tdmDomainBody), &generic) != nil {
		t.Fatal("fixture is not valid JSON")
	}
}
