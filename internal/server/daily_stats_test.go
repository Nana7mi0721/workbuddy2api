package server

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestDailyStats() *DailyStats {
	return &DailyStats{days: map[string]*dailyDayStat{}}
}

func dateAt(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

func TestDailyStatsRecordSnapshot(t *testing.T) {
	d := newTestDailyStats()
	d.recordAt(dateAt("2026-10-01").Add(3*time.Hour), "cn:glm-5.3", 100, 50, 60, 0.25)
	d.recordAt(dateAt("2026-10-01").Add(4*time.Hour), "cn:glm-5.3", 200, 30, 0, 1.5)
	d.recordAt(dateAt("2026-10-01").Add(5*time.Hour), "cn:deepseek", 10, 5, 5, 0)
	// -1 哨兵必须被钳到 0，不允许稀释统计。
	d.recordAt(dateAt("2026-10-02").Add(3*time.Hour), "cn:glm-5.3", -1, -1, -1, 0.5)

	all := d.Snapshot(400)
	if len(all) != 2 {
		t.Fatalf("Snapshot 天数 = %d, want 2", len(all))
	}
	if all[0].Date != "2026-10-01" || all[1].Date != "2026-10-02" {
		t.Fatalf("Snapshot 未按日期升序: %s, %s", all[0].Date, all[1].Date)
	}
	day1 := all[0]
	if day1.Requests != 3 || day1.PromptTokens != 310 || day1.CompletionTokens != 85 || day1.CacheHitTokens != 65 {
		t.Fatalf("10-01 汇总不符: %+v", day1)
	}
	if got := day1.Credits; got < 1.749 || got > 1.751 {
		t.Fatalf("10-01 credits = %v, want 1.75", got)
	}
	m := day1.Models["cn:glm-5.3"]
	if m == nil || m.Requests != 2 || m.PromptTokens != 300 {
		t.Fatalf("10-01 模型桶不符: %+v", m)
	}
	if day2 := all[1]; day2.Requests != 1 || day2.PromptTokens != 0 || day2.CompletionTokens != 0 {
		t.Fatalf("10-02 负值未钳零: %+v", day2)
	}
	// Snapshot 是深拷贝：改动副本不得污染内部状态。
	all[0].Models["cn:glm-5.3"].Requests = 999
	if d.Snapshot(400)[0].Models["cn:glm-5.3"].Requests != 2 {
		t.Fatal("Snapshot 未深拷贝，内部状态被污染")
	}
}

func TestDailyStatsNilSafe(t *testing.T) {
	var d *DailyStats
	d.Record("m", 1, 2, 3, 0.5)     // 不得 panic
	if got := d.Snapshot(10); got != nil {
		t.Fatalf("nil Snapshot = %v, want nil", got)
	}
	d.Close() // 不得 panic
}

func TestDailyStatsFlushLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "stats_daily.json")
	d := &DailyStats{days: map[string]*dailyDayStat{}, path: path}
	d.recordAt(dateAt("2026-09-30"), "m1", 10, 20, 0, 0.1)
	d.recordAt(dateAt("2026-10-01"), "m2", 5, 5, 5, 0.2)
	d.dirty = true
	d.flush()
	if d.dirty {
		t.Fatal("flush 后 dirty 应为 false")
	}

	d2 := &DailyStats{days: map[string]*dailyDayStat{}, path: path}
	d2.load()
	got := d2.Snapshot(400)
	if len(got) != 2 || got[0].Date != "2026-09-30" || got[1].Date != "2026-10-01" {
		t.Fatalf("回读不符: %+v", got)
	}
	if got[1].Models["m2"] == nil || got[1].Models["m2"].PromptTokens != 5 {
		t.Fatalf("回读模型桶缺失: %+v", got[1])
	}
}

func TestDailyStatsPrune(t *testing.T) {
	d := &DailyStats{days: map[string]*dailyDayStat{}, path: filepath.Join(t.TempDir(), "s.json")}
	base := dateAt("2025-01-01")
	for i := 0; i < dailyStatsKeepDays+30; i++ {
		d.recordAt(base.Add(time.Duration(i)*24*time.Hour), "m", 1, 1, 0, 0)
	}
	d.dirty = true
	d.flush()
	if got := len(d.days); got != dailyStatsKeepDays {
		t.Fatalf("prune 后天数 = %d, want %d", got, dailyStatsKeepDays)
	}
	all := d.Snapshot(dailyStatsKeepDays + 100)
	if len(all) != dailyStatsKeepDays {
		t.Fatalf("Snapshot 天数 = %d, want %d", len(all), dailyStatsKeepDays)
	}
}
