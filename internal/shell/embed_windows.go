//go:build windows

package shell

import (
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/wailsapp/wails/v3/pkg/w32"
)

// embedWndClass 是覆盖窗口的窗口类名（进程内注册一次）。
const embedWndClass = "UnBoxEmbedOverlay"

// winOverlay 是主窗口上的 WS_CHILD 覆盖窗口：创建时铺满父窗口客户区并
// 置顶到 WebView 之上，mpv 以它为 --wid 宿主，画面即接管整个主窗口；
// hide 后它与其中的 mpv 视频子窗口一起不可见，UI 立即回到前台。
// 所有窗口操作经 runOverlayOp 排到专属泵线程执行（见 startOverlayPump），
// 因此可被并发调用（Load、事件循环、窗口事件来自不同 goroutine）。
type winOverlay struct {
	mu     sync.Mutex
	parent w32.HWND
	hwnd   w32.HWND
}

var (
	overlayClassOnce sync.Once
	overlayWndProc   = windows.NewCallback(overlayWndProcFn)
	overlayPumpOnce  sync.Once
	// 缓冲队列：调用方在排入命令时不阻塞，避免「泵线程正执行依赖调用方
	// 线程的 op、调用方却卡在发送上」的互锁。
	overlayCmds = make(chan func(), 8)
)

// overlayWndProc 只做默认处理：覆盖窗口本身不参与任何交互，输入由其中的
// mpv 视频子窗口接管。
func overlayWndProcFn(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	return uintptr(w32.DefWindowProc(w32.HWND(hwnd), msg, wParam, lParam))
}

func newOverlaySurface() overlaySurface { return &winOverlay{} }

// startOverlayPump 启动覆盖窗口专属的「加锁 + 消息泵」OS 线程；窗口的
// 创建/显隐/缩放全部排到这条线程执行（runOverlayOp）。
//
// 为什么必须这样做（实测踩过的坑，勿删）：
//   - Go goroutine 会在 OS 线程间迁移：不 LockOSThread 时，同一 goroutine
//     先后两次调 Win32 窗口 API 可能落在不同线程，对同一窗口的操作从
//     「同线程直调」退化成跨线程 SendMessage——曾经让 MoveWindow 卡死 30s+；
//   - 窗口 owner 线程若不泵消息，任何跨线程 SendMessage（Wails 线程的
//     resize、mpv 的同步消息）都会无限阻塞。泵线程持续 PeekMessage，
//     跨线程投递的同步消息（含 sent 消息）在取队列时被处理。
func startOverlayPump() {
	overlayPumpOnce.Do(func() {
		go func() {
			runtime.LockOSThread()
			for {
				select {
				case fn := <-overlayCmds:
					fn()
					continue
				default:
				}
				var msg w32.MSG
				for w32.PeekMessage(&msg, 0, 0, 0, w32.PM_REMOVE) {
					if msg.Message == w32.WM_QUIT {
						return
					}
					w32.TranslateMessage(&msg)
					w32.DispatchMessage(&msg)
				}
				time.Sleep(2 * time.Millisecond)
			}
		}()
	})
}

// runOverlayOp 把窗口操作排到泵线程执行并等待其完成。
//
// 等待期间持续泵调用方自己的消息队列：调用方若恰是覆盖窗口父窗口的
// owner 线程（测试线程、Wails UI 线程的 resize 事件），泵线程里的
// CreateWindowEx/SetWindowPos 会向父窗口同步回投消息——调用方干等不泵，
// 双方即互锁（实测 CreateWindowEx 卡死 30s+）。非 owner 的调用方多泵
// 一次本线程队列无害。跨线程同步消息（SendMessage/sent message）正是在
// PeekMessage/GetMessage 取队列时被系统派发的。
func runOverlayOp(fn func()) {
	startOverlayPump()
	done := make(chan struct{})
	overlayCmds <- func() {
		defer close(done)
		fn()
	}
	for {
		select {
		case <-done:
			return
		default:
		}
		var msg w32.MSG
		for w32.PeekMessage(&msg, 0, 0, 0, w32.PM_REMOVE) {
			w32.TranslateMessage(&msg)
			w32.DispatchMessage(&msg)
		}
		time.Sleep(time.Millisecond)
	}
}

// show 显示（或首次创建）覆盖窗口并返回其句柄。重复 show 把窗口重新置顶
// 显示——WebView 之上的层级可能被窗口系统动过，每次播放都刷新一次最稳妥。
func (o *winOverlay) show(parent uintptr) uintptr {
	if parent == 0 {
		return 0
	}
	var h uintptr
	runOverlayOp(func() { h = o.showLocked(parent) })
	return h
}

// showLocked 是 show 的实体，须在泵线程上执行。
func (o *winOverlay) showLocked(parent uintptr) uintptr {
	o.mu.Lock()
	defer o.mu.Unlock()
	hp := w32.HWND(parent)
	if o.hwnd != 0 && o.parent == hp && w32.IsWindow(o.hwnd) {
		o.toTopLocked()
		return uintptr(o.hwnd)
	}
	if o.hwnd != 0 {
		// 宿主窗口已更换（重建窗口）：拆掉旧覆盖窗口再按新宿主重建。
		w32.DestroyWindow(o.hwnd)
		o.hwnd, o.parent = 0, 0
	}
	overlayClassOnce.Do(registerOverlayClass)
	rc := w32.GetClientRect(hp)
	w, h := int(rc.Right-rc.Left), int(rc.Bottom-rc.Top)
	if w <= 0 || h <= 0 {
		return 0
	}
	child := w32.CreateWindowEx(
		0,
		w32.MustStringToUTF16Ptr(embedWndClass),
		w32.MustStringToUTF16Ptr(""),
		w32.WS_CHILD,
		0, 0, w, h,
		hp, 0, w32.GetModuleHandle(""), nil)
	if child == 0 {
		return 0
	}
	o.hwnd, o.parent = child, hp
	o.toTopLocked()
	return uintptr(child)
}

// toTopLocked 置顶并显示覆盖窗口（调用方须持有 o.mu）。
func (o *winOverlay) toTopLocked() {
	w32.SetWindowPos(o.hwnd, w32.HWND_TOP, 0, 0, 0, 0,
		w32.SWP_NOSIZE|w32.SWP_NOMOVE|w32.SWP_NOACTIVATE|w32.SWP_SHOWWINDOW)
}

// resize 让覆盖窗口重新铺满父窗口客户区，并把其中的 mpv 视频子窗口同步到
// 同尺寸；只调尺寸，不改变 z 序与可见性（隐藏状态保持隐藏）。
func (o *winOverlay) resize() {
	runOverlayOp(o.resizeLocked)
}

// resizeLocked 是 resize 的实体，须在泵线程上执行。
func (o *winOverlay) resizeLocked() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.hwnd == 0 || o.parent == 0 || !w32.IsWindow(o.parent) {
		return
	}
	rc := w32.GetClientRect(o.parent)
	w, h := int(rc.Right-rc.Left), int(rc.Bottom-rc.Top)
	if w <= 0 || h <= 0 {
		return
	}
	w32.SetWindowPos(o.hwnd, 0, 0, 0, w, h, w32.SWP_NOACTIVATE|w32.SWP_NOZORDER)
	o.syncChildrenLocked(w, h)
}

// syncChildrenLocked 把覆盖窗口的所有子窗口（即 mpv 的视频窗口）同步到
// 同尺寸。Windows 不会自动缩放子窗口；即便 mpv 自己处理了父窗口缩放，
// 重复设置到同一矩形也只是无害的幂等操作。
//
// 必须带 SWP_ASYNCWINDOWPOS：mpv 子窗口属于另一个进程的线程，同步
// SetWindowPos 会 SendMessage 等它应答——若 mpv 此刻正反过来给我们发
// 同步消息，双方互等即死锁（实测卡死过 30s+）；异步投递只改队列不等待。
func (o *winOverlay) syncChildrenLocked(w, h int) {
	w32.EnumChildWindows(o.hwnd, func(child w32.HWND, _ w32.LPARAM) w32.LRESULT {
		w32.SetWindowPos(child, 0, 0, 0, w, h,
			w32.SWP_NOZORDER|w32.SWP_NOACTIVATE|w32.SWP_ASYNCWINDOWPOS)
		return 1 // 继续枚举
	})
}

// hide 隐藏覆盖窗口（连同其中的 mpv 视频子窗口）；未创建时是空操作。
func (o *winOverlay) hide() {
	runOverlayOp(o.hideLocked)
}

// hideLocked 是 hide 的实体，须在泵线程上执行。
func (o *winOverlay) hideLocked() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.hwnd != 0 {
		w32.ShowWindow(o.hwnd, w32.SW_HIDE)
	}
}

// registerOverlayClass 注册覆盖窗口类：黑底（视频留边不刺眼）、箭头光标
// （mpv 子窗口未覆盖时也不至于光标消失）。返回 0 表示类已注册或注册失败，
// 均容忍——后续 CreateWindowEx 会给出真实结果。
func registerOverlayClass() {
	w32.RegisterClassEx(&w32.WNDCLASSEX{
		Size:       uint32(unsafe.Sizeof(w32.WNDCLASSEX{})),
		WndProc:    overlayWndProc,
		Cursor:     w32.LoadCursorWithResourceID(0, uint16(w32.IDC_ARROW)),
		Background: w32.HBRUSH(w32.GetStockObject(w32.BLACK_BRUSH)),
		Instance:   w32.GetModuleHandle(""),
		ClassName:  w32.MustStringToUTF16Ptr(embedWndClass),
	})
}
