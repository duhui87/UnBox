//go:build windows

package shell

import (
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/wailsapp/wails/v3/pkg/w32"
)

var testParentClassOnce sync.Once

// newTestParentWindow 创建一个可见的测试顶层窗口，充当主窗口宿主。
// LockOSThread 必须放在最前：Win32 窗口句柄绑定创建它的 OS 线程，goroutine
// 迁移会让后续 MoveWindow/DestroyWindow 变成跨线程 SendMessage，而测试线程
// 不泵消息——发送方无限阻塞（曾把本测试卡死 30s+）。
func newTestParentWindow(t *testing.T) w32.HWND {
	t.Helper()
	runtime.LockOSThread()
	testParentClassOnce.Do(func() {
		// 与 Wails 启动时的 setupDPIAwareness 一致：进程感知 DPI 后，
		// 窗口矩形与 mpv（本身即 DPI 感知）才在同一物理坐标系里可比。
		// 每进程只能设置一次，已设置时返回错误可忽略。
		_ = w32.SetProcessDpiAwarenessContext(w32.DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2)
		w32.RegisterClassEx(&w32.WNDCLASSEX{
			Size:       uint32(unsafe.Sizeof(w32.WNDCLASSEX{})),
			WndProc:    overlayWndProc,
			Cursor:     w32.LoadCursorWithResourceID(0, uint16(w32.IDC_ARROW)),
			Background: w32.HBRUSH(w32.GetStockObject(w32.BLACK_BRUSH)),
			Instance:   w32.GetModuleHandle(""),
			ClassName:  w32.MustStringToUTF16Ptr("UnBoxTestParent"),
		})
	})
	hwnd := w32.CreateWindowEx(
		0,
		w32.MustStringToUTF16Ptr("UnBoxTestParent"),
		w32.MustStringToUTF16Ptr("unbox overlay test"),
		w32.WS_OVERLAPPEDWINDOW,
		80, 80, 640, 480,
		0, 0, w32.GetModuleHandle(""), nil)
	if hwnd == 0 {
		t.Fatal("创建测试父窗口失败")
	}
	w32.ShowWindow(hwnd, w32.SW_SHOW)
	t.Cleanup(func() { w32.DestroyWindow(hwnd) })
	return hwnd
}

// childWindows 枚举 parent 的全部子窗口句柄。
func childWindows(parent w32.HWND) []w32.HWND {
	var out []w32.HWND
	w32.EnumChildWindows(parent, func(h w32.HWND, _ w32.LPARAM) w32.LRESULT {
		out = append(out, h)
		return 1
	})
	return out
}

// waitForCond 轮询等待条件成立；超时即失败。
func waitForCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

// sameRect 比较两个屏幕坐标矩形是否一致。
func sameRect(a, b *w32.RECT) bool {
	return a.Left == b.Left && a.Top == b.Top && a.Right == b.Right && a.Bottom == b.Bottom
}

// TestWinOverlayShowCreatesChildFillingParent 保证 show 创建 WS_CHILD 覆盖
// 窗口：可见、是宿主的子窗口、尺寸恰为宿主客户区。
func TestWinOverlayShowCreatesChildFillingParent(t *testing.T) {
	parent := newTestParentWindow(t)
	ov := &winOverlay{}

	h := ov.show(uintptr(parent))
	if h == 0 {
		t.Fatal("show 返回 0，覆盖窗口创建失败")
	}
	child := w32.HWND(h)
	if !w32.IsWindow(child) {
		t.Fatal("返回的句柄不是有效窗口")
	}
	if !w32.IsWindowVisible(child) {
		t.Fatal("覆盖窗口创建后应可见")
	}
	found := false
	for _, c := range childWindows(parent) {
		if c == child {
			found = true
		}
	}
	if !found {
		t.Fatal("覆盖窗口不是宿主的子窗口")
	}
	wrc := w32.GetClientRect(parent)
	crc := w32.GetWindowRect(child)
	if int(crc.Right-crc.Left) != int(wrc.Right-wrc.Left) ||
		int(crc.Bottom-crc.Top) != int(wrc.Bottom-wrc.Top) {
		t.Fatalf("覆盖窗口尺寸 = %dx%d, want 客户区 %dx%d",
			crc.Right-crc.Left, crc.Bottom-crc.Top,
			wrc.Right-wrc.Left, wrc.Bottom-wrc.Top)
	}
}

// TestWinOverlayShowIsIdempotentAndReveals 保证 hide 后再次 show 原句柄
// 重新置顶显示（第二次播放复用同一窗口，不重复创建）。
func TestWinOverlayShowIsIdempotentAndReveals(t *testing.T) {
	parent := newTestParentWindow(t)
	ov := &winOverlay{}

	h1 := ov.show(uintptr(parent))
	ov.hide()
	if w32.IsWindowVisible(w32.HWND(h1)) {
		t.Fatal("hide 后覆盖窗口仍可见")
	}
	h2 := ov.show(uintptr(parent))
	if h1 != h2 {
		t.Fatalf("重复 show 创建了新窗口: %d != %d（应幂等复用）", h2, h1)
	}
	if !w32.IsWindowVisible(w32.HWND(h2)) {
		t.Fatal("再次 show 后应可见")
	}
}

// TestWinOverlayResizeFollowsParent 保证宿主窗口尺寸变化后 resize 让覆盖
// 窗口重新铺满客户区；隐藏状态下 resize 不改变可见性。
func TestWinOverlayResizeFollowsParent(t *testing.T) {
	parent := newTestParentWindow(t)
	ov := &winOverlay{}
	h := ov.show(uintptr(parent))
	if h == 0 {
		t.Fatal("show 失败")
	}

	w32.MoveWindow(parent, 80, 80, 900, 540, true)
	ov.resize()
	wrc := w32.GetClientRect(parent)
	crc := w32.GetWindowRect(w32.HWND(h))
	if int(crc.Right-crc.Left) != int(wrc.Right-wrc.Left) ||
		int(crc.Bottom-crc.Top) != int(wrc.Bottom-wrc.Top) {
		t.Fatalf("resize 后覆盖窗口尺寸 = %dx%d, want %dx%d",
			crc.Right-crc.Left, crc.Bottom-crc.Top,
			wrc.Right-wrc.Left, wrc.Bottom-wrc.Top)
	}

	// 隐藏状态下的 resize 不得把它重新显示出来。
	ov.hide()
	w32.MoveWindow(parent, 80, 80, 700, 500, true)
	ov.resize()
	if w32.IsWindowVisible(w32.HWND(h)) {
		t.Fatal("隐藏状态下 resize 不应改变可见性")
	}
}

// TestWinOverlayShowZeroParentFallsBack 保证拿不到宿主窗口时返回 0，
// Load 据此回退独立窗口而不是把 mpv 塞进无效句柄。
func TestWinOverlayShowZeroParentFallsBack(t *testing.T) {
	ov := &winOverlay{}
	if h := ov.show(0); h != 0 {
		t.Fatalf("show(0) = %d, want 0", h)
	}
}

// TestWinOverlayResizeBeforeShowIsNoop 保证未创建时 resize/hide 是空操作，
// 不 panic、不创建窗口。
func TestWinOverlayResizeBeforeShowIsNoop(t *testing.T) {
	ov := &winOverlay{}
	ov.resize()
	ov.hide()
	if ov.hwnd != 0 {
		t.Fatal("空操作不应创建窗口")
	}
}

// lookMPVExe 查找 mpv.exe；找不到时跳过（嵌入冒烟依赖真 mpv）。
func lookMPVExe(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("mpv.exe")
	if err != nil {
		t.Skipf("本机无 mpv.exe，跳过嵌入集成测试: %v", err)
	}
	return p
}

// TestWinOverlayHostsMPVChildWindow 是嵌入链路的真 mpv 冒烟：mpv 以覆盖
// 窗口为 --wid 启动后应创建自己的视频子窗口并铺满覆盖窗口；宿主缩放后
// resize 让子窗口跟着走。这是「整窗接管」可行性的最终判据。
func TestWinOverlayHostsMPVChildWindow(t *testing.T) {
	exe := lookMPVExe(t)
	parent := newTestParentWindow(t)
	ov := &winOverlay{}
	h := ov.show(uintptr(parent))
	if h == 0 {
		t.Fatal("show 失败")
	}

	cmd := exec.Command(exe,
		"--no-config", "--idle=yes", "--force-window=yes",
		"--wid="+strconv.FormatUint(uint64(h), 10))
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 mpv 失败: %v", err)
	}
	killed := false
	t.Cleanup(func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	// mpv 应在数秒内创建视频子窗口。
	var mpvChild w32.HWND
	waitForCond(t, "mpv 创建视频子窗口", func() bool {
		for _, c := range childWindows(w32.HWND(h)) {
			if c != 0 {
				mpvChild = c
				return true
			}
		}
		return false
	})

	// 子窗口应与覆盖窗口（=宿主客户区）同矩形；打印双方矩形便于诊断。
	ocr0 := w32.GetWindowRect(w32.HWND(h))
	mcr0 := w32.GetWindowRect(mpvChild)
	t.Logf("overlay rect = %+v, mpv child rect = %+v", ocr0, mcr0)
	waitForCond(t, "mpv 子窗口铺满覆盖窗口", func() bool {
		ocr := w32.GetWindowRect(w32.HWND(h))
		mcr := w32.GetWindowRect(mpvChild)
		return sameRect(ocr, mcr)
	})

	// 宿主缩放 → resize → mpv 子窗口跟着走。
	w32.MoveWindow(parent, 80, 80, 980, 600, true)
	ov.resize()
	waitForCond(t, "mpv 子窗口跟随缩放", func() bool {
		ocr := w32.GetWindowRect(w32.HWND(h))
		mcr := w32.GetWindowRect(mpvChild)
		return sameRect(ocr, mcr) &&
			int(ocr.Right-ocr.Left) == int(w32.GetClientRect(parent).Right-w32.GetClientRect(parent).Left)
	})

	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	killed = true
}
