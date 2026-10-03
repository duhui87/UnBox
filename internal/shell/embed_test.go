package shell

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/unbox/unbox/internal/player"
)

// fakeOverlay 记录 show/hide/resize 调用，show 返回可编程宿主句柄。
type fakeOverlay struct {
	mu      sync.Mutex
	hwnd    uintptr
	shown   int
	hidden  bool
	resized int
}

func (f *fakeOverlay) show(parent uintptr) uintptr {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shown++
	f.hidden = false
	return f.hwnd
}

func (f *fakeOverlay) resize() {
	f.mu.Lock()
	f.resized++
	f.mu.Unlock()
}

func (f *fakeOverlay) hide() {
	f.mu.Lock()
	f.hidden = true
	f.mu.Unlock()
}

func (f *fakeOverlay) snapshot() (shown int, hidden bool, resized int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shown, f.hidden, f.resized
}

// fakePlayer 是可编程内层播放器：记录 SetEmbedWindow 的句柄与各控制调用，
// Load 按 loadErr 返回错误，事件经 events 注入。
type fakePlayer struct {
	mu      sync.Mutex
	wid     uintptr
	widSet  int
	loadErr error
	loadN   int
	closed  int
	played  int
	events  chan player.Event
	state   player.State
}

func newFakePlayer() *fakePlayer {
	return &fakePlayer{
		events: make(chan player.Event, 16),
		state:  player.State{Playing: player.StateStopped, Duration: -1},
	}
}

func (f *fakePlayer) Load(context.Context, player.Stream) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadN++
	return f.loadErr
}

func (f *fakePlayer) SetEmbedWindow(id uintptr) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.widSet++
	f.wid = id
}

func (f *fakePlayer) Play() error {
	f.mu.Lock()
	f.played++
	f.mu.Unlock()
	return nil
}

func (f *fakePlayer) Pause() error { return nil }
func (f *fakePlayer) Seek(float64) error {
	return nil
}
func (f *fakePlayer) SetVolume(int) error { return nil }
func (f *fakePlayer) SelectTrack(player.TrackKind, int) error {
	return errors.New("未实现")
}

func (f *fakePlayer) State() player.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakePlayer) Events() <-chan player.Event { return f.events }

func (f *fakePlayer) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

func (f *fakePlayer) snapshot() (wid uintptr, widSet, loadN, closed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wid, f.widSet, f.loadN, f.closed
}

// waitFor 轮询等待条件成立；超时即失败。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

// TestEmbedLoadShowsOverlayAndPassesWid 保证嵌入层在 Load 前显示 overlay，
// 并把 overlay 句柄喂给内层播放器（mpv --wid 由此拿到宿主）。
func TestEmbedLoadShowsOverlayAndPassesWid(t *testing.T) {
	inner := newFakePlayer()
	ov := &fakeOverlay{hwnd: 777}
	e := newEmbedder(inner, ov, func() uintptr { return 42 })
	defer e.stop()

	if err := e.Load(context.Background(), player.Stream{URL: "x"}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if wid, widSet, _, _ := inner.snapshot(); widSet != 1 || wid != 777 {
		t.Fatalf("SetEmbedWindow = (id=%d, 次数=%d), want (777, 1)", wid, widSet)
	}
	shown, hidden, _ := ov.snapshot()
	if shown != 1 || hidden {
		t.Fatalf("overlay show=%d hidden=%v, want show=1 且可见", shown, hidden)
	}
}

// TestEmbedLoadFailureHidesOverlay 保证 Load 失败把主窗口还给 UI，
// 不留黑屏 overlay 盖在界面上。
func TestEmbedLoadFailureHidesOverlay(t *testing.T) {
	inner := newFakePlayer()
	inner.loadErr = errors.New("boom")
	ov := &fakeOverlay{hwnd: 777}
	e := newEmbedder(inner, ov, func() uintptr { return 42 })
	defer e.stop()

	if err := e.Load(context.Background(), player.Stream{}); err == nil {
		t.Fatal("Load err = nil, want 错误透传")
	}
	waitFor(t, "Load 失败后 overlay 被隐藏", func() bool {
		_, hidden, _ := ov.snapshot()
		return hidden
	})
}

// TestEmbedWithoutHostFallsBackToZeroWid 保证拿不到宿主窗口（窗口未就绪 /
// 非 Windows 平台）时回退 SetEmbedWindow(0) → mpv 独立窗口，绝不卡死在嵌入态。
func TestEmbedWithoutHostFallsBackToZeroWid(t *testing.T) {
	inner := newFakePlayer()
	ov := &fakeOverlay{hwnd: 777}
	e := newEmbedder(inner, ov, func() uintptr { return 0 })
	defer e.stop()

	if err := e.Load(context.Background(), player.Stream{}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if wid, widSet, _, _ := inner.snapshot(); widSet != 1 || wid != 0 {
		t.Fatalf("SetEmbedWindow = (id=%d, 次数=%d), want (0, 1) 独立窗口回退", wid, widSet)
	}
	if shown, _, _ := ov.snapshot(); shown != 0 {
		t.Fatalf("host=0 时不应创建 overlay, show=%d", shown)
	}
}

// TestEmbedTerminalEventsHideOverlay 保证内层终端事件（EOF/Error/Quit）到达
// 时隐藏 overlay：视频播完或用户点 mpv OSC 关闭后，主窗口必须回到 UI 手里。
func TestEmbedTerminalEventsHideOverlay(t *testing.T) {
	for _, kind := range []player.EventKind{player.EventEOF, player.EventError, player.EventQuit} {
		t.Run(eventKindName(kind), func(t *testing.T) {
			inner := newFakePlayer()
			ov := &fakeOverlay{hwnd: 777}
			e := newEmbedder(inner, ov, func() uintptr { return 42 })
			defer e.stop()

			if err := e.Load(context.Background(), player.Stream{}); err != nil {
				t.Fatalf("Load: %v", err)
			}
			inner.events <- player.Event{Kind: kind}
			waitFor(t, "终端事件后 overlay 被隐藏", func() bool {
				_, hidden, _ := ov.snapshot()
				return hidden
			})
		})
	}
}

// TestEmbedKeepsOverlayOnNonTerminalEvent 保证位置/缓冲等非终端事件不隐藏
// overlay，且事件照常转发给上层。
func TestEmbedKeepsOverlayOnNonTerminalEvent(t *testing.T) {
	inner := newFakePlayer()
	ov := &fakeOverlay{hwnd: 777}
	e := newEmbedder(inner, ov, func() uintptr { return 42 })
	defer e.stop()

	if err := e.Load(context.Background(), player.Stream{}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	inner.events <- player.Event{Kind: player.EventPosition, Position: 3.5}

	select {
	case ev := <-e.Events():
		if ev.Kind != player.EventPosition {
			t.Fatalf("转发事件 Kind = %v, want position", ev.Kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("非终端事件未被转发")
	}
	if _, hidden, _ := ov.snapshot(); hidden {
		t.Fatal("位置事件不应隐藏 overlay")
	}
}

// TestEmbedCloseHidesOverlay 保证 Close 拆播放器的同时把主窗口还给 UI。
func TestEmbedCloseHidesOverlay(t *testing.T) {
	inner := newFakePlayer()
	ov := &fakeOverlay{hwnd: 777}
	e := newEmbedder(inner, ov, func() uintptr { return 42 })
	defer e.stop()

	if err := e.Load(context.Background(), player.Stream{}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, hidden, _ := ov.snapshot(); !hidden {
		t.Fatal("Close 后 overlay 未隐藏")
	}
	if _, _, _, c := inner.snapshot(); c != 1 {
		t.Fatalf("内层 Close 次数 = %d, want 1", c)
	}
}

// TestEmbedPassThroughMethods 保证控制方法与状态原样透传内层。
func TestEmbedPassThroughMethods(t *testing.T) {
	inner := newFakePlayer()
	inner.state = player.State{Playing: player.StatePlaying, Position: 12, Duration: 34}
	ov := &fakeOverlay{hwnd: 1}
	e := newEmbedder(inner, ov, func() uintptr { return 0 })
	defer e.stop()

	if err := e.Play(); err != nil {
		t.Fatalf("Play: %v", err)
	}
	st := e.State()
	if st.Playing != player.StatePlaying || st.Position != 12 || st.Duration != 34 {
		t.Fatalf("State = %+v, want 内层状态透传", st)
	}
	inner.mu.Lock()
	got := inner.played
	inner.mu.Unlock()
	if got != 1 {
		t.Fatalf("内层 Play 次数 = %d, want 1", got)
	}
}

// TestEmbedStopIsIdempotent 保证 stop 幂等且事件循环确实退出（RefreshMPV
// 重包装时旧循环必须被收掉，否则 goroutine 泄漏）。
func TestEmbedStopIsIdempotent(t *testing.T) {
	inner := newFakePlayer()
	e := newEmbedder(inner, &fakeOverlay{hwnd: 1}, func() uintptr { return 0 })
	e.stop()
	e.stop() // 幂等
	select {
	case <-e.loopDone:
	default:
		t.Fatal("stop 后事件循环未退出")
	}
}

// eventKindName 为子测试生成可读名称。
func eventKindName(k player.EventKind) string {
	switch k {
	case player.EventEOF:
		return "EOF"
	case player.EventError:
		return "Error"
	case player.EventQuit:
		return "Quit"
	default:
		return "Other"
	}
}
