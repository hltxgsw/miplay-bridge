package output

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 单投送源互斥（2026-10-05 miplay-exclusive）：
// 现场故障是两路 ffmpeg|aplay 同时写 hw:0,0 —— 后来者 Resource busy、先来者
// underrun。下面几条把"同一时刻只有一个写者"钉死。

func mustAcquire(t *testing.T, engine *Engine, h Holder) {
	t.Helper()
	if err := engine.Acquire(context.Background(), h); err != nil {
		t.Fatalf("Acquire(%s/%s): %v", h.Source, h.peer(), err)
	}
}

func (k *fakeKernel) stateNow() (string, int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.state, k.volume
}

// 新源踢旧源：不同 peer 的新投送必须先停掉旧投送，并且**等它真的放手**才接管。
func TestAcquirePreemptsTheCurrentHolder(t *testing.T) {
	kernel := newFakeKernel(t)
	engine := testEngine(t, kernel)

	const holdFor = 150 * time.Millisecond
	var evictedA atomic.Bool
	entered := make(chan struct{})
	evictA := func(ctx context.Context) error {
		evictedA.Store(true)
		close(entered)
		time.Sleep(holdFor)
		return nil
	}
	mustAcquire(t, engine, Holder{Source: SourceMiPlay, Peer: "192.168.31.244", Token: "A#1", Evict: evictA})
	if got := engine.CurrentHolder(); got != "A#1" {
		t.Fatalf("holder = %q, want A#1", got)
	}

	begin := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- engine.Acquire(context.Background(), Holder{
			Source: SourceMiPlay, Peer: "192.168.31.131", Token: "B#1",
			Evict: func(context.Context) error { return nil },
		})
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the previous holder was never asked to stop")
	}
	// 旧的那一路还没退干净之前，槽位绝不能已经换人。
	if got := engine.CurrentHolder(); got != "A#1" {
		t.Fatalf("holder changed to %q while A was still holding the device", got)
	}
	if err := <-done; err != nil {
		t.Fatalf("Acquire(B): %v", err)
	}
	if elapsed := time.Since(begin); elapsed < holdFor {
		t.Fatalf("Acquire(B) returned after %v, before A let go (%v)", elapsed, holdFor)
	}
	if !evictedA.Load() {
		t.Fatal("A's teardown was never called")
	}
	if got := engine.CurrentHolder(); got != "B#1" {
		t.Fatalf("holder = %q, want B#1", got)
	}
}

// 同一 peer 重连不是新来源：不得把自己踢掉。
func TestAcquireSamePeerDoesNotEvictItself(t *testing.T) {
	kernel := newFakeKernel(t)
	engine := testEngine(t, kernel)

	var evictions int32
	evict := func(ctx context.Context) error {
		atomic.AddInt32(&evictions, 1)
		return nil
	}
	first := Holder{Source: SourceMiPlay, Peer: "192.168.31.244", Token: "A#1", Evict: evict}
	mustAcquire(t, engine, first)

	// 同一部手机换了一条控制连接：同一个 peer，新的 token。
	second := Holder{Source: SourceMiPlay, Peer: "192.168.31.244", Token: "A#2", Evict: evict}
	mustAcquire(t, engine, second)

	if got := atomic.LoadInt32(&evictions); got != 0 {
		t.Fatalf("a reconnect from the same peer evicted itself %d time(s)", got)
	}
	if got := engine.CurrentHolder(); got != "A#2" {
		t.Fatalf("holder = %q, want the refreshed session A#2", got)
	}
	if engine.ActiveSource() != SourceMiPlay {
		t.Fatalf("active = %q, want miplay", engine.ActiveSource())
	}
}

// 被抢占的会话迟到收尾时，不得释放继任者的槽位。
func TestStaleReleaseDoesNotFreeTheSuccessor(t *testing.T) {
	kernel := newFakeKernel(t)
	engine := testEngine(t, kernel)

	staleA := Holder{Source: SourceMiPlay, Peer: "192.168.31.244", Token: "A#1",
		Evict: func(context.Context) error { return nil }}
	mustAcquire(t, engine, staleA)
	mustAcquire(t, engine, Holder{Source: SourceMiPlay, Peer: "192.168.31.131", Token: "B#1",
		Evict: func(context.Context) error { return nil }})

	// A 的会话 goroutine 现在才 unwind，调用 Release。
	engine.ReleaseHolder(staleA)

	if got := engine.CurrentHolder(); got != "B#1" {
		t.Fatalf("holder = %q after a stale release, want B#1", got)
	}
	if engine.ActiveSource() != SourceMiPlay {
		t.Fatalf("active = %q, want miplay still held", engine.ActiveSource())
	}
}

// DLNA 与 MiPlay 共用一块独占声卡，两个方向都必须互斥（新的赢，旧的先停）。
func TestDLNAAndMiPlayAreMutuallyExclusive(t *testing.T) {
	kernel := newFakeKernel(t)
	engine := testEngine(t, kernel)

	// 方向一：MiPlay 在投送，DLNA 按了 Play。
	var miplayEvicted atomic.Bool
	mustAcquire(t, engine, Holder{
		Source: SourceMiPlay, Peer: "192.168.31.244", Token: "A#1",
		Evict: func(context.Context) error { miplayEvicted.Store(true); return nil },
	})
	if err := engine.ClaimDLNA(); err != nil {
		t.Fatalf("ClaimDLNA while miplay holds: %v", err)
	}
	if !miplayEvicted.Load() {
		t.Fatal("the miplay cast was not stopped before DLNA took the card")
	}
	if engine.ActiveSource() != SourceDLNA {
		t.Fatalf("active = %q, want dlna", engine.ActiveSource())
	}

	// 方向二：DLNA 在播，手机又开始投送 → 内核要被停掉才能让位。
	kernel.setState("play")
	before := len(kernel.recorded())
	mustAcquire(t, engine, Holder{
		Source: SourceMiPlay, Peer: "192.168.31.244", Token: "A#2",
		Evict: func(context.Context) error { return nil },
	})
	if engine.ActiveSource() != SourceMiPlay {
		t.Fatalf("active = %q, want miplay", engine.ActiveSource())
	}
	commands := strings.Join(kernel.recorded()[before:], "\n")
	if !strings.Contains(commands, "stop") {
		t.Fatalf("the playback kernel was not stopped for the hand-off; commands:\n%s", commands)
	}
	if state, _ := kernel.stateNow(); state == "play" {
		t.Fatalf("kernel still playing after the hand-off: %q", state)
	}
}

// 非抢占路径（HTTP API / 测试音）保持不变：遇到忙的声卡老实回 busy，
// 绝不打断正在投送的源。这条是「不要为了互斥把 API 也变成抢占」的守卫。
func TestClaimStaysNonPreemptive(t *testing.T) {
	kernel := newFakeKernel(t)
	engine := testEngine(t, kernel)

	var evicted atomic.Bool
	mustAcquire(t, engine, Holder{
		Source: SourceMiPlay, Peer: "192.168.31.244", Token: "A#1",
		Evict: func(context.Context) error { evicted.Store(true); return nil },
	})

	if err := engine.Claim(SourceAPI); !errors.Is(err, ErrSourceBusy) {
		t.Fatalf("Claim(api) while miplay holds = %v, want ErrSourceBusy", err)
	}
	if evicted.Load() {
		t.Fatal("the non-preemptive Claim must not stop the current cast")
	}
	if got := engine.CurrentHolder(); got != "A#1" {
		t.Fatalf("holder = %q, want A#1 untouched", got)
	}
}

// 抢占过程中旧持有者迟迟不退时不能永远挂住调用方：ctx 取消要能返回。
func TestAcquireHonoursContextWhileWaitingForTheOldHolder(t *testing.T) {
	kernel := newFakeKernel(t)
	engine := testEngine(t, kernel)

	blocked := make(chan struct{})
	evictNeverFinishes := func(ctx context.Context) error {
		close(blocked)
		<-ctx.Done()
		return ctx.Err()
	}
	mustAcquire(t, engine, Holder{Source: SourceMiPlay, Peer: "192.168.31.244", Token: "A#1",
		Evict: evictNeverFinishes})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := engine.Acquire(ctx, Holder{Source: SourceDLNA, Peer: "dlna", Token: "dlna"})
	if err == nil {
		t.Fatal("Acquire must fail when the previous holder never lets go")
	}
	select {
	case <-blocked:
	default:
		t.Fatal("the previous holder was never asked to stop")
	}
	// 让位失败时槽位保持原样，不能出现"两个都不持有"的真空。
	if got := engine.CurrentHolder(); got != "A#1" {
		t.Fatalf("holder = %q after a failed hand-off, want A#1", got)
	}
}

// 真机现场的原样复刻（2026-10-05 12:54）：**两部手机都在 MiPlay 侧互抢**，
// 与 DLNA 无关。.244 持卡在放音，.131 是"能抢但不放音"的残留/自动重连会话。
// 这条守住的是 Peer 维度：同 Source、不同 Peer → 必须互斥（新者接管）。
//
// 说明：语义上它与 TestAcquirePreemptsTheCurrentHolder 同源，但那一条讲的是
// "抢占要等旧持有者退场"；这一条把真机日志的场景（两个具体 IP、一个在播一个空抢）
// 直接摆出来，让"同源多会话互斥"这个被现场证实的场景在测试里可见、可读。
func TestDifferentPeerMiPlaySessionsAreExclusive(t *testing.T) {
	kernel := newFakeKernel(t)
	engine := testEngine(t, kernel)

	const (
		peerPlaying  = "192.168.31.244" // 主人的手机：持卡在放音（media_frames=26839）
		peerStarving = "192.168.31.131" // 残留/自动重连：抢到但不放音（media_frames=0）
	)

	var playingEvicted atomic.Bool
	mustAcquire(t, engine, Holder{
		Source: SourceMiPlay, Peer: peerPlaying, Token: peerPlaying + "#1",
		Evict: func(context.Context) error { playingEvicted.Store(true); return nil },
	})

	// 残留会话进来认领：不同 Peer → 必须接管，且先把在播那路停掉。
	mustAcquire(t, engine, Holder{
		Source: SourceMiPlay, Peer: peerStarving, Token: peerStarving + "#1",
		Evict: func(context.Context) error { return nil },
	})

	if !playingEvicted.Load() {
		t.Fatal("the starving peer took the card without stopping the playing session")
	}
	if got := engine.CurrentHolder(); got != peerStarving+"#1" {
		t.Fatalf("holder = %q, want the newcomer", got)
	}
	if engine.ActiveSource() != SourceMiPlay {
		t.Fatalf("active = %q, want miplay", engine.ActiveSource())
	}

	// 主人那台重连 → 互相不得共存：又换回 .244，且 .131 的槽位被放掉。
	mustAcquire(t, engine, Holder{
		Source: SourceMiPlay, Peer: peerPlaying, Token: peerPlaying + "#2",
		Evict: func(context.Context) error { return nil },
	})
	if got := engine.CurrentHolder(); got != peerPlaying+"#2" {
		t.Fatalf("holder = %q, want the reconnected player", got)
	}
	// 任何时刻都只有一个持有人 —— 这是本单唯一的硬不变量。
	engine.ReleaseHolder(Holder{Source: SourceMiPlay, Peer: peerPlaying, Token: peerPlaying + "#1"})
	if got := engine.CurrentHolder(); got != peerPlaying+"#2" {
		t.Fatalf("a superseded session's release freed %q", got)
	}
}
