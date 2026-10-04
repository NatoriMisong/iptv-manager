package core

import "testing"

func TestSiteDomain(t *testing.T) {
	for host, want := range map[string]string{
		"a.demo.org":       "demo.org",
		"B.Demo.ORG.":      "demo.org",
		"c.test.com":       "test.com",
		"test.com":         "test.com",
		"localhost":        "localhost",
		"news.bbc.co.uk":   "bbc.co.uk",
		"live.cctv.com.cn": "cctv.com.cn",
		"cdn.example.co":   "example.co",
		"203.0.113.9":      "203.0.113.9",
		"[2001:db8::1]":    "2001:db8::1",
		"a.demo.org:8080":  "demo.org",
		"":                 "",
	} {
		if got := SiteDomain(host); got != want {
			t.Errorf("SiteDomain(%q) = %q, want %q", host, got, want)
		}
	}
	if got := URLDomain("http://a.demo.org:8080/1.m3u"); got != "demo.org" {
		t.Errorf("URLDomain = %q", got)
	}
	if got := URLDomain("not a url"); got != "" {
		t.Errorf("URLDomain(invalid) = %q", got)
	}
}
