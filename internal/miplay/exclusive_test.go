package miplay

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miplay-bridge/miplay-bridge/internal/config"
)

// 单投送源互斥（2026-10-05 miplay-exclusive）。
//
// 现场故障：两路 ffmpeg|aplay 同时持有 hw:0,0（`ls -l /proc/*/fd | grep -c snd`
// == 2），后来者 "Resource busy"、先来者 underrun，听感就是「一直卡然后断开」。
// 这一组用例守住三件事：新源踢旧源、同源重连不自杀、以及**旧的真的走了新的才起**。

// fakeArbiter 复刻仲裁引擎对接收端可见的那部分契约：
// 同一时刻只有一个持有者；不同 peer 抢占；同 peer 视为重连；Release 按 token 匹配。
type fakeArbiter struct {
	mu        sync.Mutex
	peer      string
	token     string
	evict     func(context.Context) error
	evictions int
}

func (a *fakeArbiter) claim(ctx context.Context, req ClaimRequest) error {
	a.mu.Lock()
	if a.peer != "" && a.peer != req.Peer {
		victim := a.evict
		a.mu.Unlock()
		if victim != nil {
			if err := victim(ctx); err != nil {
				return err
			}
		}
		a.mu.Lock()
		a.evictions++
	}
	a.peer, a.token, a.evict = req.Peer, req.Token, req.Evict
	a.mu.Unlock()
	return nil
}

func (a *fakeArbiter) release(peer, token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token == token {
		a.peer, a.token, a.evict = "", "", nil
	}
}

func (a *fakeArbiter) snapshot() (string, string, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.peer, a.token, a.evictions
}

// writeFakeChild 造一个"像 aplay/ffmpeg 那样读 stdin 直到 EOF"的假子进程，
// 让 Pipeline 能在测试里真的跑起来（Start/Stop 的进程生命周期是真的）。
func writeFakeChild(t *testing.T, name string, prelude string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	script := "#!/bin/sh\n" + prelude + "exec cat > /dev/null\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func exclusiveTestConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.DataDirectory = t.TempDir()
	cfg.FFmpegPath = writeFakeChild(t, "fake-ffmpeg", "")
	cfg.AplayPath = writeFakeChild(t, "fake-aplay", "")
	return cfg
}

func newExclusiveReceiver(t *testing.T, arb *fakeArbiter) *Receiver {
	t.Helper()
	cfg := exclusiveTestConfig(t)
	receiver := NewReceiver(cfg, discardLogger(), Hooks{Claim: arb.claim, Release: arb.release})
	t.Cleanup(func() {
		// 这些用例没有真的会话 goroutine 来清 active；先清掉，收尾才不用等
		// Disconnect 的 5 秒兜底（生产里那 5 秒是等真实会话退场的）。
		receiver.mu.Lock()
		receiver.active = nil
		receiver.mu.Unlock()
		receiver.Shutdown()
	})
	return receiver
}

// startVictimUnwind 模拟"被踢会话的 goroutine"：WFD 被取消后它自己会走
// endSession 收尾（清 active、释放槽位）。互斥逻辑必须等它走完。
func startVictimUnwind(t *testing.T, receiver *Receiver, report *sessionRuntime) <-chan struct{} {
	t.Helper()
	finished := make(chan struct{})
	cancelled := make(chan struct{})
	report.cancelWFD = func() { close(cancelled) }
	go func() {
		<-cancelled
		time.Sleep(60 * time.Millisecond) // 收尾要花一点时间，真实会话也是
		// endSession 也会回调 cancelWFD；已经被我们触发过，置空避免重复关闭。
		report.cancelWFD = nil
		receiver.endSession(report)
		close(finished)
	}()
	return finished
}

func newSessionReport(receiver *Receiver, peer string) *sessionRuntime {
	return &sessionRuntime{peer: peer, token: nextSessionToken(peer), trackVersion: -1}
}

// 新源踢旧源：第二部手机开始投送时，前一个会话必须被终止，而且新会话接管前
// 要等旧会话真的收尾完。
func TestSecondPeerPreemptsTheFirstCast(t *testing.T) {
	arb := &fakeArbiter{}
	receiver := newExclusiveReceiver(t, arb)

	first := newSessionReport(receiver, "192.168.31.244")
	startVictimUnwind(t, receiver, first)
	if !receiver.beginSession(context.Background(), first) {
		t.Fatal("the first cast must take the card")
	}

	second := newSessionReport(receiver, "192.168.31.131")
	begin := time.Now()
	if !receiver.beginSession(context.Background(), second) {
		t.Fatal("the second cast must preempt the first")
	}
	elapsed := time.Since(begin)

	peer, token, evictions := arb.snapshot()
	if evictions != 1 {
		t.Fatalf("evictions = %d, want exactly 1", evictions)
	}
	if peer != "192.168.31.131" || token != second.token {
		t.Fatalf("arbiter holds %s/%s, want %s/%s", peer, token, second.peer, second.token)
	}
	if elapsed < 60*time.Millisecond {
		t.Fatalf("the hand-off took %v: the newcomer did not wait for the old cast to let go", elapsed)
	}

	receiver.mu.Lock()
	active := receiver.active
	receiver.mu.Unlock()
	if active != second {
		t.Fatalf("active = %v, want the second cast", active)
	}
	if first.claimed {
		t.Fatal("the preempted cast still believes it holds the card")
	}
}

// 同一部手机重连：不得把自己踢掉，但上一个会话的尸体必须收掉，
// 否则它占着管线，新会话起不来。
func TestSamePeerReconnectDoesNotEvictItself(t *testing.T) {
	arb := &fakeArbiter{}
	receiver := newExclusiveReceiver(t, arb)

	first := newSessionReport(receiver, "192.168.31.244")
	firstUnwound := startVictimUnwind(t, receiver, first)
	if !receiver.beginSession(context.Background(), first) {
		t.Fatal("the first cast must take the card")
	}

	second := newSessionReport(receiver, "192.168.31.244") // 同一个 peer
	begin := time.Now()
	if !receiver.beginSession(context.Background(), second) {
		t.Fatal("a reconnect from the same peer must be accepted")
	}
	elapsed := time.Since(begin)

	// 上一个会话必须被收掉（否则它占着管线，新会话起不来）。
	select {
	case <-firstUnwound:
	case <-time.After(2 * time.Second):
		t.Fatal("the replaced session was never collected")
	}

	peer, token, evictions := arb.snapshot()
	if evictions != 0 {
		t.Fatalf("a same-peer reconnect evicted itself %d time(s)", evictions)
	}
	if peer != "192.168.31.244" || token != second.token {
		t.Fatalf("arbiter holds %s/%s, want %s/%s", peer, token, second.peer, second.token)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("the reconnect stalled for %v", elapsed)
	}

	receiver.mu.Lock()
	active := receiver.active
	receiver.mu.Unlock()
	if active != second {
		t.Fatalf("active = %v, want the reconnected session", active)
	}
	if first.claimed {
		t.Fatal("the replaced session still believes it holds the card")
	}
}

// 被替换的会话迟到 unlock 时，Release 不得把继任者的槽位放掉。
func TestStaleReleaseDoesNotFreeTheReconnectedSession(t *testing.T) {
	arb := &fakeArbiter{}
	receiver := newExclusiveReceiver(t, arb)

	first := newSessionReport(receiver, "192.168.31.244")
	if !receiver.beginSession(context.Background(), first) {
		t.Fatal("the first cast must take the card")
	}
	second := newSessionReport(receiver, "192.168.31.244")
	if !receiver.beginSession(context.Background(), second) {
		t.Fatal("the reconnect must be accepted")
	}

	// 旧会话这时候才走 endSession。
	receiver.endSession(first)

	peer, token, _ := arb.snapshot()
	if peer != "192.168.31.244" || token != second.token {
		t.Fatalf("a stale release freed the successor's slot: %s/%s", peer, token)
	}
}

// 管线只认一个所有者：外来会话既不能停它，也不能接管它。
func TestForeignSessionCannotStopOrAdoptThePipeline(t *testing.T) {
	cfg := exclusiveTestConfig(t)
	pipeline := NewPipeline(cfg, discardLogger())

	if err := pipeline.Start("A#1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer pipeline.Stop()

	pipeline.StopOwned("B#1")
	if !pipeline.Running() {
		t.Fatal("a session that never owned the pipeline stopped it")
	}

	if err := pipeline.Start("A#2"); !errors.Is(err, errPipelineBusy) {
		t.Fatalf("Start with a foreign owner = %v, want errPipelineBusy", err)
	}
	if err := pipeline.Start("A#1"); err != nil {
		t.Fatalf("re-Start for the owning session must be a no-op: %v", err)
	}

	pipeline.StopOwned("A#1")
	if pipeline.Running() {
		t.Fatal("the owning session could not stop its own pipeline")
	}
}

// 换手必须串行：Start 不能插进 Stop 中间（那正是两路 aplay 同时活着的成因）。
func TestStartWaitsForAnInFlightStop(t *testing.T) {
	cfg := config.Default()
	cfg.DataDirectory = t.TempDir()
	// 子进程收到 EOF 后还要睡一会儿才退，制造一个可观测的 Stop 窗口。
	cfg.FFmpegPath = writeFakeChild(t, "slow-ffmpeg", "sleep 0.4\n")
	cfg.AplayPath = writeFakeChild(t, "slow-aplay", "sleep 0.4\n")
	pipeline := NewPipeline(cfg, discardLogger())

	if err := pipeline.Start("A#1"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	stopDone := make(chan struct{})
	go func() {
		pipeline.StopOwned("A#1")
		close(stopDone)
	}()
	time.Sleep(50 * time.Millisecond) // 让 Stop 先进临界区

	startDone := make(chan error, 1)
	go func() { startDone <- pipeline.Start("B#1") }()

	select {
	case err := <-startDone:
		t.Fatalf("Start slipped into the middle of Stop (err=%v): two aplay would share hw:0,0", err)
	case <-time.After(150 * time.Millisecond):
	}

	<-stopDone
	select {
	case err := <-startDone:
		if err != nil {
			t.Fatalf("Start after the hand-off finished: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start never completed after Stop finished")
	}
	if !pipeline.Running() {
		t.Fatal("the replacement pipeline is not running")
	}
	pipeline.Stop()
}

// ---------------------------------------------------------------------------
// 2026-10-05 事故复现（P0：部署后主人投歌完全无声）
//
// 真机时序（reports/live-evidence/current-status.log，同一部手机 .244）：
//	14:01:28.104  miplay disconnect requested          ← 手机断开
//	14:01:28.415  pipeline stopped / sound card released
//	14:01:28.417  pipeline started owner=.244#3        ← ★ 已结束的会话把管线又拉起来了
//	14:01:51      control-leg media frame failed ×N    ← 新会话 #4 每帧被拒（held by #3）
//	（容器里 aplay 508 / ffmpeg 509 从此跑了 5 分 50 秒，独占 hw:0,0 → 主人完全无声）
//
// 这一组用例守住两条不变量：
//	① **进入 teardown 的会话不得再启动/接管管线** —— 否则它留下一个没人喂的 aplay
//	   独占声卡，就是那个僵尸（本次事故真正的成因，在「断开」这条路上必现）。
//	② **拿到卡的会话必须能收掉任何非自己的残留管线并重起**（自愈）—— 报错干等
//	   正是让主人一直没声音的那个行为。
// ---------------------------------------------------------------------------

// capturingLogger 记录日志文本，用来断言「管线只起了一次」这类行为，
// 而不是去翻 Pipeline 的私有字段。
type capturingHandler struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.buf.WriteString(record.Message)
	h.buf.WriteByte('\n')
	return nil
}

func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

func (h *capturingHandler) count(substr string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Count(h.buf.String(), substr)
}

func newExclusiveReceiverWithLog(t *testing.T, arb *fakeArbiter) (*Receiver, *capturingHandler) {
	t.Helper()
	handler := &capturingHandler{}
	receiver := NewReceiver(exclusiveTestConfig(t), slog.New(handler),
		Hooks{Claim: arb.claim, Release: arb.release})
	t.Cleanup(func() {
		receiver.mu.Lock()
		receiver.active = nil
		receiver.mu.Unlock()
		receiver.Shutdown()
	})
	return receiver, handler
}

// ① 手机断开后，那一帧迟到的媒体数据不得把管线重新拉起来。
// 改前：Start 会成功（会话已被 teardown，owner 已清空）→ 僵尸 aplay 独占 hw:0,0。
func TestDisconnectedSessionCannotRestartThePipeline(t *testing.T) {
	receiver, _ := newExclusiveReceiverWithLog(t, &fakeArbiter{})
	report := newSessionReport(receiver, "192.168.31.244")

	if !receiver.beginSession(context.Background(), report) {
		t.Fatal("the cast must take the sound card")
	}
	if err := receiver.pipeline.Start(report.token); err != nil {
		t.Fatalf("start: %v", err)
	}

	// 真机断开走的就是这两步（Disconnect 处理）。
	receiver.stopCast(report)
	receiver.pipeline.Stop()

	// 断开之后，旧会话的读循环还飞进来一帧：这里必须被拒。
	if err := receiver.pipeline.Start(report.token); err == nil {
		t.Fatal("a torn-down session restarted the pipeline: its aplay keeps hw:0,0 " +
			"open with nobody feeding it -- that is the live zombie (aplay 508 / ffmpeg 509)")
	}
	if receiver.pipeline.Running() {
		t.Fatal("the pipeline came back up after teardown")
	}
	// 同一个窗口里，接收端这条路也必须拒绝：stopCast 之后、r.active 清空之前，
	// 这个会话仍然是 active，只能靠 retired 标记挡住它（真机 14:01:28 就在这里）。
	if err := receiver.ensurePipeline(report); !errors.Is(err, errSessionOver) {
		t.Fatalf("a torn-down session went through ensurePipeline: err = %v", err)
	}
	if receiver.pipeline.Running() {
		t.Fatal("ensurePipeline restarted the pipeline for a torn-down session")
	}
}

// ② 新会话自愈：拿到卡就必须能把残留管线收掉重起，而不是每帧报错干等。
func TestNewCastReclaimsALeftoverPipeline(t *testing.T) {
	receiver, _ := newExclusiveReceiverWithLog(t, &fakeArbiter{})
	first := newSessionReport(receiver, "192.168.31.244")
	if !receiver.beginSession(context.Background(), first) {
		t.Fatal("the first cast must take the sound card")
	}
	if err := receiver.pipeline.Start(first.token); err != nil {
		t.Fatalf("start: %v", err)
	}

	// 旧会话已经离场（真机上它的 endSession 早就跑完了），只剩一条没人拥有的管线。
	receiver.mu.Lock()
	receiver.active = nil
	receiver.mu.Unlock()

	second := newSessionReport(receiver, "192.168.31.244")
	if !receiver.beginSession(context.Background(), second) {
		t.Fatal("the reconnect must take the sound card")
	}
	if err := receiver.ensurePipeline(second); err != nil {
		t.Fatalf("the card holder could not reclaim the leftover pipeline "+
			"(every media frame would be rejected from here on): %v", err)
	}
	if got := receiver.pipeline.currentOwner(); got != second.token {
		t.Fatalf("pipeline owner = %q, want %q", got, second.token)
	}
	if !receiver.pipeline.Running() {
		t.Fatal("the pipeline is not running for the new session")
	}
}

// ③ 同 token 幂等：同一个会话每帧都会走到这里，绝不能每帧重启一次管线。
func TestPipelineReclaimIsIdempotentForTheOwningSession(t *testing.T) {
	receiver, handler := newExclusiveReceiverWithLog(t, &fakeArbiter{})
	report := newSessionReport(receiver, "192.168.31.244")
	if !receiver.beginSession(context.Background(), report) {
		t.Fatal("the cast must take the sound card")
	}

	for i := 0; i < 20; i++ {
		if err := receiver.ensurePipeline(report); err != nil {
			t.Fatalf("reclaim #%d: %v", i, err)
		}
	}
	if got := handler.count("miplay realtime pipeline started"); got != 1 {
		t.Fatalf("the pipeline was started %d times for one session, want 1", got)
	}
	if got := handler.count("miplay realtime pipeline stopped"); got != 0 {
		t.Fatalf("the pipeline was stopped %d times for one session, want 0", got)
	}
}

// ④ 被取代的会话不许把管线抢回去（否则新老会话会互相踩）。
func TestSupersededSessionCannotReclaimThePipeline(t *testing.T) {
	receiver, _ := newExclusiveReceiverWithLog(t, &fakeArbiter{})
	first := newSessionReport(receiver, "192.168.31.244")
	if !receiver.beginSession(context.Background(), first) {
		t.Fatal("the first cast must take the sound card")
	}
	if err := receiver.ensurePipeline(first); err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	second := newSessionReport(receiver, "192.168.31.244")
	if !receiver.beginSession(context.Background(), second) {
		t.Fatal("the reconnect must take the sound card")
	}
	if err := receiver.ensurePipeline(second); err != nil {
		t.Fatalf("reclaim by the new session: %v", err)
	}

	// 旧会话的读循环延迟醒来，还想写一帧。
	if err := receiver.ensurePipeline(first); err == nil {
		t.Fatal("a superseded session grabbed the pipeline back from the current cast")
	}
	if got := receiver.pipeline.currentOwner(); got != second.token {
		t.Fatalf("pipeline owner = %q, want %q", got, second.token)
	}
}

// ⑤ 真机时序整段复刻：投送 → 手机断开（断开后飞来一帧）→ 主人重新投送。
// 期望：断开不留下僵尸，重新投送立刻有声（不需要重启容器）。
func TestLiveIncidentSequenceRecoversWithoutRestart(t *testing.T) {
	receiver, handler := newExclusiveReceiverWithLog(t, &fakeArbiter{})

	// 1) 投送中
	first := newSessionReport(receiver, "192.168.31.244")
	if !receiver.beginSession(context.Background(), first) {
		t.Fatal("the cast must take the sound card")
	}
	if err := receiver.ensurePipeline(first); err != nil {
		t.Fatalf("the first cast has no sound: %v", err)
	}

	// 2) 手机断开（真机 14:01:28 的 Disconnect 路径）
	receiver.stopCast(first)
	receiver.pipeline.Stop()

	// 3) 断开之后飞进来的那一帧：现场正是它把 ffmpeg/aplay 又拉起来的
	if err := receiver.pipeline.Start(first.token); err == nil {
		t.Fatal("the late frame after disconnect restarted the pipeline (zombie)")
	}

	// 4) 主人重新投送：同 peer、新 token，必须重新有声
	second := newSessionReport(receiver, "192.168.31.244")
	if !receiver.beginSession(context.Background(), second) {
		t.Fatal("the re-cast must take the sound card")
	}
	if err := receiver.ensurePipeline(second); err != nil {
		t.Fatalf("the owner re-cast and still has no sound: %v", err)
	}
	if got := receiver.pipeline.currentOwner(); got != second.token {
		t.Fatalf("pipeline owner = %q, want %q", got, second.token)
	}
	if !receiver.pipeline.Running() {
		t.Fatal("the pipeline is not running after the re-cast")
	}
	if got := handler.count("miplay realtime pipeline stopped"); got != 1 {
		// 只有断开那一次停；重新投送不该再停（旧实现会在每帧往返停/起）
		t.Fatalf("the pipeline was stopped %d times, want 1", got)
	}
}

// ⑥ 退休标记本身：stopCast / endSession 都必须置位。它是「此后不许再碰管线」的
// 第一道闸，也是被取代会话安静停手（不再每帧进管线锁）的依据。
func TestTeardownMarksTheSessionRetired(t *testing.T) {
	receiver, _ := newExclusiveReceiverWithLog(t, &fakeArbiter{})

	report := newSessionReport(receiver, "192.168.31.244")
	if report.isRetired() {
		t.Fatal("a fresh session must not be retired")
	}
	receiver.stopCast(report)
	if !report.isRetired() {
		t.Fatal("stopCast must retire the session (Disconnect path)")
	}

	other := newSessionReport(receiver, "192.168.31.244")
	receiver.endSession(other)
	if !other.isRetired() {
		t.Fatal("endSession must retire the session")
	}
}

// ⑦ 换手窗口：beginSession 是「先把 r.active 指向新会话，再 stopCast 旧会话」。
// 在这两步之间，旧会话**既不 active 也还没 retired**，而它的读循环可能正好送来
// 一帧。那帧必须被拒，而且不许把管线抢回去（真机 #3 → #4 的换手里就是这个窗口）。
func TestSessionInTheHandoverWindowCannotReclaim(t *testing.T) {
	receiver, _ := newExclusiveReceiverWithLog(t, &fakeArbiter{})
	old := newSessionReport(receiver, "192.168.31.244")
	if !receiver.beginSession(context.Background(), old) {
		t.Fatal("the first cast must take the sound card")
	}
	if err := receiver.ensurePipeline(old); err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	next := newSessionReport(receiver, "192.168.31.244")
	// 复刻 beginSession 的第一步：接管 r.active（旧会话此刻还没被 stopCast 退休）。
	receiver.mu.Lock()
	receiver.active = next
	receiver.mu.Unlock()

	if err := receiver.ensurePipeline(old); !errors.Is(err, errSessionOver) {
		t.Fatalf("a session that lost the card went through ensurePipeline: err = %v", err)
	}
	if got := receiver.pipeline.currentOwner(); got != old.token {
		t.Fatalf("pipeline owner = %q, want %q: the outgoing session still owns it until "+
			"the handover stops it", got, old.token)
	}
}
