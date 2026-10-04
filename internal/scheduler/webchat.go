// webchat.go 国际版每日活跃打卡（webchat 任务， seventh 排程类）。
//
// 为什么单独一个任务类而并入签到：国际版没有签到/任务中心（scheduler.go D4 门控
// 跳过 global），积分回血通道是官方「每日活跃 30/50 积分」——网页通道起一次跑完
// 的 agent 会话（上游实现在 upstream.WebDailyCheckin）。打卡真实消耗少量积分，
// 因此幂等闸必须**落盘**（WebchatStateFile，uid → CST 自然日）：进程重启不能对
// 同一批号重跑白烧积分；这与 rewardClaimed（内存即可，上游幂等兜底）刻意不同。
// 自然日按 CST 划（travelDay，与签到/奖励同一口径）。
package scheduler

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"

	"workbuddy2api/internal/logfmt"
)

// webchatAccountDelay 账号间限速：打卡会真实起 agent 会话，串行 + 间隔对上游友好
// （与 activityAccountDelay 同口径，测试可置 0）。
var webchatAccountDelay = 2 * time.Second

// RunWebchatNow 立即为所有国际版账号打一次每日活跃卡的人工入口
// （/admin/tasks/webchat/run、测试）。当日已成功的账号由落盘闸门跳过。
func (s *Scheduler) RunWebchatNow() { s.runWebchat(context.Background(), triggerManual) }

// runWebchat 执行一轮国际版每日打卡并记台账。
//
// 计数口径（与 runActivity 一致）：Total 为参与遍历的账号数；跑完记 OK、
// 当日已打过记 Already、失败记 Fail；禁用 / 无凭证 / 非 global（含逃生门降级）
// 记 Skipped。ctx 取消打断时标 interrupted——计数是部分的，不判全灭、不排重试。
//
// 失败不落闸（当日第二槽 21 点会自然重试）；成功才 markWebchatDone。
func (s *Scheduler) runWebchat(ctx context.Context, trigger string) {
	started := time.Now()
	var t runTally
	defer func() { s.recordRun(taskWebchat, trigger, started, t) }()

	first := true
	for _, st := range s.cfg.Pool.List() {
		if ctx.Err() != nil {
			t.interrupted = true
			return
		}
		if st.Disabled {
			t.skipped++
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			t.skipped++
			continue
		}
		// 双闸门（与 chatBase/billingBase 同源）：global.enabled=false 时 global
		// 账号被降级为 CN——绝不打网页通道；CN 账号天然不在列。
		if !s.cfg.Upstream.WebChatEligible(a) {
			t.skipped++
			continue
		}
		if s.webchatDoneToday(st.UID) {
			t.already++
			continue
		}
		if !first {
			if !sleepCtx(ctx, webchatAccountDelay) {
				t.interrupted = true
				return
			}
		}
		first = false
		t.total++
		// token 临期先补刷新（与签到同窗口 skew），否则建会话必然 401 白跑。
		// 刷新失败只跳过本号（不判罚——下一次槽位自然重试），落盘台账照记。
		if a.NeedsRefresh(checkinRefreshSkew) {
			if err := s.cfg.Upstream.RefreshToken(a); err != nil {
				s.cfg.Pool.NoteRefreshFail(st.UID, err) // P0-1 续期观测台账（只记录，不判罚）
				t.fail++
				log.Printf("webchat %s: refresh: %v", logfmt.Label(st.UID, st.Nickname), err)
				continue
			}
			s.cfg.Pool.NoteRefreshOK(st.UID)
			a.BackfillRealm()
			if err := a.SaveAtomic(); err != nil {
				log.Printf("webchat %s save: %v", logfmt.Label(st.UID, st.Nickname), err)
			}
		}
		res, err := s.cfg.Upstream.WebDailyCheckin(a)
		if err != nil {
			t.fail++
			log.Printf("webchat %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			continue
		}
		t.ok++
		s.markWebchatDone(st.UID)
		log.Printf("webchat %s: ok conversation=%s status=%s chunks=%d updates=%d elapsed=%dms",
			logfmt.Label(st.UID, st.Nickname), res.Conversation, res.Status, res.Chunks, res.Updates, res.ElapsedMS)
	}
}

// webchatDoneToday 该账号当日（CST 自然日）是否已成功打过卡。
func (s *Scheduler) webchatDoneToday(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.webchatDone[uid] == travelDay(time.Now())
}

// markWebchatDone 记录该账号当日打卡完成并落盘（落盘失败只记日志：闸门退化为
// 内存级，重启后可能对同号重跑一次——观测组件不该让打卡结果丢失这件事静默成
// 「什么都不发生」，所以这里必须出日志）。
func (s *Scheduler) markWebchatDone(uid string) {
	day := travelDay(time.Now())
	s.mu.Lock()
	defer s.mu.Unlock()
	s.webchatDone[uid] = day
	path := s.cfg.WebchatStateFile
	if path == "" {
		return
	}
	if err := saveWebchatStateFile(path, s.webchatDone); err != nil {
		log.Printf("WARN: webchat state save %s: %v（重启后当日闸门失效，可能对同号重跑一次）", path, err)
	}
}

// loadWebchatStateFile 启动时读回当日打卡台账（uid → CST 自然日）；文件缺失按
// 空表处理（首次运行/从未打过）。解析失败视为损坏，从空表开始——宁可当日重打
// 一次（上游按日幂等，多耗一次会话）也不能让一条坏文件永久堵死打卡。
func loadWebchatStateFile(path string) map[string]string {
	m := make(map[string]string)
	if path == "" {
		return m
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return m // 不存在/不可读 = 空
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		log.Printf("WARN: webchat state %s 解析失败，按空表处理: %v", path, err)
		return make(map[string]string)
	}
	if m == nil {
		m = make(map[string]string)
	}
	return m
}

// saveWebchatStateFile 原子落盘（tmp + rename，与 pool state / panel config 同套路）。
func saveWebchatStateFile(path string, m map[string]string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
