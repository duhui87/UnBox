package mpvproc

import (
	"context"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unbox/unbox/internal/player"
)

// lookMPV 按平台名查找 mpv 并跳过无 mpv 的集成测试。Windows 必须显式找
// mpv.exe：裸 "mpv" 按 PATHEXT 优先命中同目录的 mpv.com 启动器，其拉起的
// 真 mpv.exe 是孙进程，Close 的 Kill 杀不到它，进程泄漏并把收尸管道攥住。
func lookMPV(t *testing.T) string {
	t.Helper()
	name := "mpv"
	if runtime.GOOS == "windows" {
		name = "mpv.exe"
	}
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("本机无 mpv，跳过集成测试: %v", err)
	}
	return p
}

// drainEvents 在后台消费 p.Events()，模拟生产端读取，并返回停止函数。
// 不消费会让事件通道堆满后终端事件（EOF/Error，按设计必须送达）阻塞
// readLoop，管道随之被写满，mpv 自身和在飞的命令 Write 一起卡死——压测里
// 曾把 send 卡到 ctx 超时（119s）才由子进程被杀解除。
func drainEvents(p player.Player) (stop func()) {
	stopCh := make(chan struct{})
	go func() {
		for {
			select {
			case <-p.Events():
			case <-stopCh:
				return
			}
		}
	}()
	return func() { close(stopCh) }
}

func TestLoadPlayClose(t *testing.T) {
	mpv := lookMPV(t)
	p, err := New(mpv)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()
	defer drainEvents(p)()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 用本地生成的静音短视频？M1 不引入 ffmpeg，这里用一个不会立即 EOF 的
	// 短循环流不好造，退而求其次：验证 Load 能启动 mpv 并建立 IPC 即可，
	// 播放本身由 Task 5 的冒烟在真实环境人工确认。
	if err := p.Load(ctx, player.Stream{URL: "https://example.com/none.m3u8"}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	// 假 URL 会被 mpv 快速判失败并发 end-file error → StateStopped，与本断言
	// 竞速（Load 乐观置为 playing，错误事件到达即改写），故允许两者之一；
	// 真正的播放链路由冒烟在真实环境人工确认。
	if st := p.State(); st.Playing != player.StatePlaying && st.Playing != player.StateStopped {
		t.Fatalf("Load 后 Playing = %v, want playing 或 stopped", st.Playing)
	}
	if err := p.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
}

// TestConcurrentReload 并发交错 Load/Play/Pause/Seek/Close，令旧会话 send 与
// 新会话 Load 重叠，验证：会话代际丢弃跨会话串味的迟到应答、指针同一性判断
// 不误杀后继会话，且全程无 race/panic/死锁。末尾再做一次 Load+Play+Pause 确认
// 实例仍可用。
func TestConcurrentReload(t *testing.T) {
	mpv := lookMPV(t)
	p, err := New(mpv)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()
	defer drainEvents(p)()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const url = "https://example.com/none.m3u8"

	// 初始会话。
	if err := p.Load(ctx, player.Stream{URL: url}); err != nil {
		t.Fatalf("Load: %v", err)
	}

	var wg sync.WaitGroup
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 3; i++ {
				_ = p.Load(ctx, player.Stream{URL: url})
				_ = p.Play()
				_ = p.Pause()
				_ = p.Seek(1)
				_ = p.Close()
			}
		}()
	}
	wg.Wait()

	// 结束后仍可用：串味被代际丢弃，无死锁。
	if err := p.Load(ctx, player.Stream{URL: url}); err != nil {
		t.Fatalf("末尾 Load: %v", err)
	}
	if err := p.Play(); err != nil {
		t.Fatalf("末尾 Play: %v", err)
	}
	if err := p.Pause(); err != nil {
		t.Fatalf("末尾 Pause: %v", err)
	}
}

func TestSendEventTerminalBlocksUntilRead(t *testing.T) {
	ch := make(chan player.Event) // 无缓冲：无人读取时必然阻塞
	done := make(chan struct{})
	go func() {
		sendEvent(ch, player.Event{Kind: player.EventEOF})
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("终端事件在无人读取时被丢弃了（不应发生）")
	case <-time.After(20 * time.Millisecond):
		// 预期：阻塞在发送上，done 未关闭
	}
	<-ch // 读取后放行
	<-done
}

func TestSendEventPositionDropsWhenFull(t *testing.T) {
	ch := make(chan player.Event, 1)
	ch <- player.Event{Kind: player.EventPosition}          // 占满
	sendEvent(ch, player.Event{Kind: player.EventPosition}) // 应丢弃而非阻塞
}

// TestSendAbortsWhenSessionTornDown 保证会话被拆除（Close / 被新 Load 顶掉）时，
// 在飞的命令立即返回，而不是干等 cmdResponseTimeout（5s）——否则切换/关闭会被
// 卡住一整拍，并发压测也会被 5s 级超时拖爆预算。
func TestSendAbortsWhenSessionTornDown(t *testing.T) {
	p := &mpvProc{
		responses: make(chan response, 16),
		events:    make(chan player.Event, 64),
		state:     player.State{Playing: player.StateStopped, Duration: -1},
	}
	conn := newSendStubConn()
	p.lifecycleMu.Lock()
	p.session = 1
	p.conn = conn
	p.done = make(chan struct{})
	p.lifecycleMu.Unlock()

	errCh := make(chan error, 1)
	go func() { errCh <- p.send("set_property", "pause", true) }()
	<-conn.wrote // send 已写入并进入等待应答

	if err := p.Close(); err != nil { // 拆除当前会话
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("send err = nil, want 会话已关闭的错误")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("会话拆除后 send 未及时返回（仍在干等应答超时）")
	}
}

// TestRegisterSessionTearsDownDisplacedSession 保证并发 Load 顶掉刚登记的旧
// 会话时，旧 mpv 被就地收尸。Load 开头的 Close 可能在别人登记之前就跑过，
// 若登记时直接覆盖 p.cmd，被覆盖的 mpv 无人认领——进程与窗口永久泄漏
// （实测并发压测每次运行泄漏一个 mpv）。
func TestRegisterSessionTearsDownDisplacedSession(t *testing.T) {
	p := &mpvProc{
		responses: make(chan response, 16),
		events:    make(chan player.Event, 64),
		state:     player.State{Playing: player.StateStopped, Duration: -1},
	}

	h1 := helperSleepProcess(t)
	w1 := startWaiter(h1)
	p.registerSession(h1, w1, newSendStubConn(), "")

	h2 := helperSleepProcess(t)
	w2 := startWaiter(h2)
	defer func() {
		_ = p.Close()
		_ = w2.wait()
	}()
	p.registerSession(h2, w2, newSendStubConn(), "")

	select {
	case <-w1.exited():
	case <-time.After(3 * time.Second):
		t.Fatal("被顶掉的会话进程仍未退出（mpv 泄漏）")
	}
}

// helperSleepProcess 启动一个长时间安睡的本测试二进制副本，充当「mpv 子进程」。
func helperSleepProcess(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperSleepProcess")
	cmd.Env = append(os.Environ(), "UNBOX_HELPER_SLEEP=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 helper 进程失败: %v", err)
	}
	return cmd
}

func TestHelperSleepProcess(t *testing.T) {
	if os.Getenv("UNBOX_HELPER_SLEEP") != "1" {
		return
	}
	time.Sleep(time.Minute)
}

// TestHelperExitProcess 按 UNBOX_HELPER_EXIT_CODE 立即以指定退出码退出，
// 充当「以该退出码自然结束/异常崩溃的 mpv」。未设置环境变量时不动作。
func TestHelperExitProcess(t *testing.T) {
	v := os.Getenv("UNBOX_HELPER_EXIT_CODE")
	if v == "" {
		return
	}
	code, err := strconv.Atoi(v)
	if err != nil {
		code = 1
	}
	os.Exit(code)
}

// helperExitProcess 启动一个会以 code 退出的本测试二进制副本。
func helperExitProcess(t *testing.T, code int) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperExitProcess")
	cmd.Env = append(os.Environ(), "UNBOX_HELPER_EXIT_CODE="+strconv.Itoa(code))
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 helper 进程失败: %v", err)
	}
	return cmd
}

// waitEvent 等待并返回下一个播放器事件；超时视为失败。
func waitEvent(t *testing.T, ch <-chan player.Event, timeout time.Duration) player.Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("事件通道已关闭（不应发生）")
		}
		return ev
	case <-time.After(timeout):
		t.Fatal("等待播放器事件超时")
	}
	return player.Event{}
}

// startHelperSession 用 helper 进程 + stub 连接登记一个会话并启动读循环，
// 返回会话代际与等待器。读循环模拟真实链路：进程退出后由调用方关连接
// （对应真实场景下 mpv 死亡导致 IPC 管道 EOF）。
func startHelperSession(t *testing.T, p *mpvProc, cmd *exec.Cmd) (int64, *waiter, *sendStubConn) {
	t.Helper()
	w := startWaiter(cmd)
	conn := newSendStubConn()
	sess := p.registerSession(cmd, w, conn, "")
	go p.readLoop(conn, sess)
	return sess, w, conn
}

// newBareProc 返回只有事件/状态基础设施的 mpvProc，供不依赖真 mpv 的
// 进程退出路径测试使用。
func newBareProc() *mpvProc {
	return &mpvProc{
		responses: make(chan response, 16),
		events:    make(chan player.Event, 64),
		state:     player.State{Playing: player.StateStopped, Duration: -1},
	}
}

// TestNaturalExitWithZeroCodeEmitsEventQuit 保证 mpv 进程以退出码 0 自然退出
// （用户点 mpv OSC 的关闭按钮）时上报 EventQuit、复位状态并拆掉会话——
// 否则嵌入模式下 overlay 永远盖着主窗口，前端状态也停在 playing。
func TestNaturalExitWithZeroCodeEmitsEventQuit(t *testing.T) {
	p := newBareProc()
	h := helperExitProcess(t, 0)
	_, w, conn := startHelperSession(t, p, h)

	<-w.exited() // 进程已退
	_ = conn.Close()

	ev := waitEvent(t, p.events, 3*time.Second)
	if ev.Kind != player.EventQuit {
		t.Fatalf("事件 Kind = %v, want EventQuit", ev.Kind)
	}
	if st := p.State(); st.Playing != player.StateStopped {
		t.Fatalf("退出后 Playing = %v, want stopped", st.Playing)
	}
	p.lifecycleMu.Lock()
	stillHeld := p.conn != nil || p.cmd != nil
	p.lifecycleMu.Unlock()
	if stillHeld {
		t.Fatal("自然退出后会话资源未被拆掉（conn/cmd 仍登记）")
	}
}

// TestNaturalExitWithNonZeroCodeEmitsEventError 保证 mpv 异常崩溃（非 0 退出码）
// 上报带退出码信息的 EventError，让故障切换与错误提示有据可依。
func TestNaturalExitWithNonZeroCodeEmitsEventError(t *testing.T) {
	p := newBareProc()
	h := helperExitProcess(t, 7)
	_, w, conn := startHelperSession(t, p, h)

	<-w.exited()
	_ = conn.Close()

	ev := waitEvent(t, p.events, 3*time.Second)
	if ev.Kind != player.EventError {
		t.Fatalf("事件 Kind = %v, want EventError", ev.Kind)
	}
	if ev.Err == nil || !strings.Contains(ev.Err.Error(), "mpv 进程异常退出") {
		t.Fatalf("Err = %v, want 含「mpv 进程异常退出」", ev.Err)
	}
	if !strings.Contains(ev.Err.Error(), "7") {
		t.Fatalf("Err = %v, want 含退出码 7", ev.Err)
	}
}

// TestCloseDoesNotEmitExitEvent 保证主动 Close 拆掉的会话不产生退出事件：
// Close 语义是「本方主动停止」，向下游只该表现为静默收场，否则每次切换/
// 关闭都会被当成一次播放失败。
func TestCloseDoesNotEmitExitEvent(t *testing.T) {
	p := newBareProc()
	h := helperSleepProcess(t)
	_, _, conn := startHelperSession(t, p, h)
	defer func() {
		_ = p.Close()
		_ = conn.Close()
	}()

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case ev := <-p.events:
		t.Fatalf("Close 后不应有退出事件, got %+v", ev)
	case <-time.After(400 * time.Millisecond):
	}
}

// sendStubConn 吞掉写入、首次写入时发信号；Read 阻塞到 Close。
type sendStubConn struct {
	wroteOnce sync.Once
	wrote     chan struct{}
	closeOnce sync.Once
	closed    chan struct{}
}

func newSendStubConn() *sendStubConn {
	return &sendStubConn{wrote: make(chan struct{}), closed: make(chan struct{})}
}

func (c *sendStubConn) Write(b []byte) (int, error) {
	c.wroteOnce.Do(func() { close(c.wrote) })
	return len(b), nil
}

func (c *sendStubConn) Read(b []byte) (int, error) {
	<-c.closed
	return 0, io.EOF
}

func (c *sendStubConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}
