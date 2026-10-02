package server

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// DailyStats 按日聚合带 usage 的成功请求观测（模型 / token 三段 / 真实扣费），
// 供面板统计页的长期趋势（Token 活动热力图、按模型用量、积分消耗）。
//
// 口径与成本账本一致：只在观测到 usage 的成功请求处记录，缺失≠0——把「没观测到
// usage」的失败请求记成 0 token 会虚增请求数、稀释日均用量。写点在 note*Observation
// 与 chatCompletions 的同构观测处（四处），因此 chat / responses / anthropic 三种
// 入站协议天然覆盖。
//
// 持久化：内存 map + 30s 脏刷盘（原子替换写 data/stats_daily.json），保留最近
// dailyStatsKeepDays 天；重启时加载。nil 接收者全部安全（未接线 = 记录为空）。
type DailyStats struct {
	mu    sync.Mutex
	days  map[string]*dailyDayStat
	path  string
	dirty bool
	stop  chan struct{}
	done  chan struct{}
}

const dailyStatsKeepDays = 400

type dailyModelStat struct {
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CacheHitTokens   int64   `json:"cache_hit_tokens"`
	Credits          float64 `json:"credits"`
}

type dailyDayStat struct {
	Date             string                     `json:"date"` // 本地日期 2006-01-02
	Requests         int64                      `json:"requests"`
	PromptTokens     int64                      `json:"prompt_tokens"`
	CompletionTokens int64                      `json:"completion_tokens"`
	CacheHitTokens   int64                      `json:"cache_hit_tokens"`
	Credits          float64                    `json:"credits"`
	Models           map[string]*dailyModelStat `json:"models"`
}

// NewDailyStats 加载历史并启动 30s 脏刷盘循环。
func NewDailyStats(path string) *DailyStats {
	d := &DailyStats{
		days: map[string]*dailyDayStat{},
		path: path,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	d.load()
	go d.flushLoop()
	return d
}

// Close 停止刷盘循环并做最后一次落盘（进程退出前调用）。
func (d *DailyStats) Close() {
	if d == nil {
		return
	}
	close(d.stop)
	<-d.done
}

// Record 记一次观测（按当前本地日期入桶）。
func (d *DailyStats) Record(model string, prompt, completion, cacheHit int, credit float64) {
	d.recordAt(time.Now(), model, prompt, completion, cacheHit, credit)
}

// recordAt Record 的可测内核：日期取自 at（本地时区）。
func (d *DailyStats) recordAt(at time.Time, model string, prompt, completion, cacheHit int, credit float64) {
	if d == nil || model == "" {
		return
	}
	if prompt < 0 {
		prompt = 0
	}
	if completion < 0 {
		completion = 0
	}
	if cacheHit < 0 {
		cacheHit = 0
	}
	day := at.Format("2006-01-02")
	d.mu.Lock()
	defer d.mu.Unlock()
	dd := d.days[day]
	if dd == nil {
		dd = &dailyDayStat{Date: day, Models: map[string]*dailyModelStat{}}
		d.days[day] = dd
	}
	dd.Requests++
	dd.PromptTokens += int64(prompt)
	dd.CompletionTokens += int64(completion)
	dd.CacheHitTokens += int64(cacheHit)
	dd.Credits += credit
	m := dd.Models[model]
	if m == nil {
		m = &dailyModelStat{}
		dd.Models[model] = m
	}
	m.Requests++
	m.PromptTokens += int64(prompt)
	m.CompletionTokens += int64(completion)
	m.CacheHitTokens += int64(cacheHit)
	m.Credits += credit
	d.dirty = true
}

// Snapshot 返回最近 days 天（升序）的副本；models 一并深拷贝，调用方拿到的
// 不再引用内部状态。
func (d *DailyStats) Snapshot(days int) []dailyDayStat {
	if d == nil || days <= 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	dates := make([]string, 0, len(d.days))
	for k := range d.days {
		dates = append(dates, k)
	}
	sort.Strings(dates)
	if len(dates) > days {
		dates = dates[len(dates)-days:]
	}
	out := make([]dailyDayStat, 0, len(dates))
	for _, k := range dates {
		src := d.days[k]
		day := *src
		day.Models = make(map[string]*dailyModelStat, len(src.Models))
		for name, m := range src.Models {
			mm := *m
			day.Models[name] = &mm
		}
		out = append(out, day)
	}
	return out
}

func (d *DailyStats) flushLoop() {
	defer close(d.done)
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-d.stop:
			d.flush()
			return
		case <-t.C:
			d.flush()
		}
	}
}

func (d *DailyStats) flush() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.dirty {
		return
	}
	if err := os.MkdirAll(filepath.Dir(d.path), 0o755); err != nil {
		log.Printf("WARN: [server] daily stats 目录创建失败 %s: %v", d.path, err)
		return
	}
	d.pruneLocked()
	dates := make([]string, 0, len(d.days))
	for k := range d.days {
		dates = append(dates, k)
	}
	sort.Strings(dates)
	days := make([]*dailyDayStat, 0, len(dates))
	for _, k := range dates {
		days = append(days, d.days[k])
	}
	raw, err := json.Marshal(map[string]any{"days": days})
	if err != nil {
		log.Printf("WARN: [server] daily stats 序列化失败: %v", err)
		return
	}
	tmp := d.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		log.Printf("WARN: [server] daily stats 写盘失败 %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, d.path); err != nil {
		log.Printf("WARN: [server] daily stats 替换失败 %s: %v", d.path, err)
		return
	}
	d.dirty = false
}

// pruneLocked 只保留最近 dailyStatsKeepDays 天（按日期字典序即可）。
func (d *DailyStats) pruneLocked() {
	if len(d.days) <= dailyStatsKeepDays {
		return
	}
	dates := make([]string, 0, len(d.days))
	for k := range d.days {
		dates = append(dates, k)
	}
	sort.Strings(dates)
	for _, k := range dates[:len(dates)-dailyStatsKeepDays] {
		delete(d.days, k)
	}
}

func (d *DailyStats) load() {
	raw, err := os.ReadFile(d.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("WARN: [server] daily stats 读取失败 %s: %v（从空开始）", d.path, err)
		}
		return
	}
	var disk struct {
		Days []*dailyDayStat `json:"days"`
	}
	if err := json.Unmarshal(raw, &disk); err != nil {
		log.Printf("WARN: [server] daily stats 解析失败 %s: %v（从空开始）", d.path, err)
		return
	}
	for _, dd := range disk.Days {
		if dd == nil || dd.Date == "" {
			continue
		}
		if dd.Models == nil {
			dd.Models = map[string]*dailyModelStat{}
		}
		d.days[dd.Date] = dd
	}
}

// statsHistory GET /v1/stats/history?days=N：按日聚合（升序，最近 N 天）。
func (h *Handler) statsHistory(w http.ResponseWriter, r *http.Request) {
	days := 90
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			days = n
		}
	}
	if days < 1 {
		days = 1
	} else if days > dailyStatsKeepDays {
		days = dailyStatsKeepDays
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"days":         h.daily.Snapshot(days),
		"generated_at": time.Now().Format(time.RFC3339),
	})
}
