package store

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"iptv-manager/internal/core"
)

func TestBulkChannelsMixedRowsAndRetry(t *testing.T) {
	s := testStore(t)
	before, _ := s.Export(testContext)
	// Keep a gap in ordering to verify that additions use the maximum, not count.
	if _, err := s.db.Exec(`UPDATE channels SET sort_order=7 WHERE sort_order=1`); err != nil {
		t.Fatal(err)
	}
	before.Channels[1].SortOrder = 7
	req := core.BulkChannelRequest{
		Text: "\uFEFF\r\n" +
			"  新闻 A, https://youtu.be/AAAAAAAAAAA?si=tracking  \r\n" +
			"已有频道，https://m.youtube.com/watch?v=vr3XyVCR4T0\r\n" +
			"https://www.youtube.com/watch?v=AAAAAAAAAAA&feature=share\r\n" +
			"\r\n" +
			"https://youtu.be/BBBBBBBBBBB?list=a,b\r\n" +
			"新闻 C\thttps://youtube.com/watch?v=CCCCCCCCCCC\r\n" +
			"错误网址,https://example.com/watch?v=DDDDDDDDDDD\r\n" +
			",https://youtu.be/EEEEEEEEEEE\r\n" +
			"坏\x00名称,https://youtu.be/FFFFFFFFFFF\r\n",
		Group: " 新闻 ", Mode: "direct", Quality: 480,
	}
	result, err := s.AddChannels(testContext, req)
	if err != nil || result.Added != 3 || result.Skipped != 2 || result.Failed != 3 {
		t.Fatalf("mixed batch: %+v %v", result, err)
	}
	wantLines := []int{2, 3, 4, 6, 7, 8, 9, 10}
	wantStatuses := []string{"added", "skipped", "skipped", "added", "added", "failed", "failed", "failed"}
	if len(result.Results) != len(wantLines) {
		t.Fatalf("results: %+v", result.Results)
	}
	for i, item := range result.Results {
		if item.Line != wantLines[i] || item.Status != wantStatuses[i] || item.Message == "" {
			t.Errorf("row %d: %+v", i, item)
		}
	}
	if result.Results[1].ChannelID != before.Channels[0].ID || result.Results[2].ChannelID != result.Results[0].ChannelID {
		t.Fatal("duplicates did not refer to existing/new channel IDs")
	}
	after, err := s.Export(testContext)
	if err != nil || len(after.Channels) != 5 {
		t.Fatalf("saved batch: %+v %v", after, err)
	}
	if !reflect.DeepEqual(before.Channels, after.Channels[:2]) || !reflect.DeepEqual(before.Settings, after.Settings) {
		t.Fatal("bulk add modified existing channels or settings")
	}
	for i, ch := range after.Channels[2:] {
		if ch.SortOrder != 8+i || ch.Group != "新闻" || ch.Mode != "direct" || ch.Quality != 480 || ch.Proxy != "inherit" || !ch.Enabled || ch.ID == "" {
			t.Errorf("new channel: %+v", ch)
		}
	}
	if after.Channels[3].Name != "YouTube BBBBBBBBBBB" || after.Channels[3].URL != "https://www.youtube.com/watch?v=BBBBBBBBBBB" {
		t.Fatalf("URL-only input: %+v", after.Channels[3])
	}
	retry, err := s.AddChannels(testContext, req)
	if err != nil || retry.Added != 0 || retry.Skipped != 5 || retry.Failed != 3 {
		t.Fatalf("retry: %+v %v", retry, err)
	}
	unchanged, _ := s.Export(testContext)
	if !reflect.DeepEqual(after, unchanged) {
		t.Fatal("retry changed database")
	}
}

func TestBulkLimitsAndValidation(t *testing.T) {
	s := testStore(t)
	before, _ := s.Export(testContext)
	validURL := "https://youtu.be/AAAAAAAAAAA"
	for _, req := range []core.BulkChannelRequest{
		{},
		{Text: " \n\r\n\t "},
		{Text: strings.Repeat(validURL+"\n", 101)},
		{Text: strings.Repeat("a", maxBulkText+1)},
		{Text: validURL, Group: "news\ninvalid"},
		{Text: validURL, Group: strings.Repeat("中", 51)},
		{Text: validURL, Mode: "invalid"},
		{Text: validURL, Quality: 999},
	} {
		if _, err := s.AddChannels(testContext, req); !errors.Is(err, ErrValidation) {
			t.Fatalf("invalid batch accepted: %v", err)
		}
		after, _ := s.Export(testContext)
		if !reflect.DeepEqual(before, after) {
			t.Fatal("invalid batch modified database")
		}
	}
	invalidRows := "https://youtu.be/too-short\n" + strings.Repeat("中", 101) + "," + validURL + "\n" + `name"quote,` + validURL
	result, err := s.AddChannels(testContext, core.BulkChannelRequest{Text: invalidRows})
	if err != nil || result.Added != 0 || result.Failed != 3 || result.Skipped != 0 {
		t.Fatalf("invalid rows: %+v %v", result, err)
	}
	var text strings.Builder
	for i := range maxBulkChannels {
		fmt.Fprintf(&text, "\nhttps://youtu.be/%011d\n", i)
	}
	result, err = s.AddChannels(testContext, core.BulkChannelRequest{Text: text.String()})
	if err != nil || result.Added != maxBulkChannels || result.Failed != 0 || result.Skipped != 0 {
		t.Fatalf("100-row batch: %+v %v", result, err)
	}
	channels, _ := s.Channels(testContext)
	if channels[2].Mode != "inherit" || channels[2].Quality != 0 || channels[2].Proxy != "inherit" {
		t.Fatalf("defaults: %+v", channels[2])
	}
}

func TestBulkDatabaseFailureRollsBack(t *testing.T) {
	s := testStore(t)
	before, _ := s.Export(testContext)
	_, err := s.db.Exec(`CREATE TRIGGER reject_bulk BEFORE INSERT ON channels WHEN NEW.name='fail-insert' BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.AddChannels(testContext, core.BulkChannelRequest{Text: "first,https://youtu.be/AAAAAAAAAAA\nfail-insert,https://youtu.be/BBBBBBBBBBB"})
	if err == nil || errors.Is(err, ErrValidation) || !reflect.DeepEqual(result, core.BulkChannelResult{}) {
		t.Fatalf("database failure: %+v %v", result, err)
	}
	after, _ := s.Export(testContext)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed batch partially committed")
	}
}

func TestConcurrentBulkRequestsDoNotCreateDuplicates(t *testing.T) {
	s := testStore(t)
	type outcome struct {
		result core.BulkChannelResult
		err    error
	}
	outcomes := make(chan outcome, 2)
	for range 2 {
		go func() {
			result, err := s.AddChannels(testContext, core.BulkChannelRequest{Text: "https://youtu.be/AAAAAAAAAAA"})
			outcomes <- outcome{result, err}
		}()
	}
	added, skipped := 0, 0
	for range 2 {
		out := <-outcomes
		if out.err != nil {
			t.Fatal(out.err)
		}
		added += out.result.Added
		skipped += out.result.Skipped
	}
	if added != 1 || skipped != 1 {
		t.Fatalf("concurrent results: added=%d skipped=%d", added, skipped)
	}
	channels, _ := s.Channels(testContext)
	if len(channels) != 3 {
		t.Fatalf("duplicate channels: %+v", channels)
	}
}
