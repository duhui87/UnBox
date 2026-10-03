//go:build windows

package mpvproc

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestNewIPCPathIsolatesFromStalePipes 保证管道名跨进程不相撞：上一个 UnBox
// 进程崩溃/测试超时被杀后，遗留的 mpv 仍占着 unbox-mpv-1；新进程 seq 从 1
// 重开会「连上」那台僵尸 mpv——它的服务端早已不读管道，后续命令全部卡死在
// WriteFile 上（集成测试里曾把 send 拖到 ctx 超时）。
func TestNewIPCPathIsolatesFromStalePipes(t *testing.T) {
	p, err := newIPCPath()
	if err != nil {
		t.Fatalf("newIPCPath: %v", err)
	}
	if !strings.Contains(p, strconv.Itoa(os.Getpid())) {
		t.Fatalf("管道名 %q 未包含进程 PID，会与上一进程遗留的管道撞名", p)
	}
}

// TestDialIPCOpensPipeWithOverlappedIO 保证拨号端以 OVERLAPPED 方式打开管道。
// os.OpenFile 打开的非 OVERLAPPED 句柄上，readLoop 的并发读与命令写入会互相
// 卡死：写 IRP 永远不完成，mpv 根本收不到命令（压测中 send 卡死即此因）。
// OVERLAPPED 句柄可设 Deadline，也是 Go 轮询器正确调度并发读写的前提。
func TestDialIPCOpensPipeWithOverlappedIO(t *testing.T) {
	name := "unbox-test-" + strconv.Itoa(os.Getpid())
	h, err := windows.CreateNamedPipe(
		windows.StringToUTF16Ptr(`\\.\pipe\`+name),
		windows.PIPE_ACCESS_DUPLEX,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT,
		1, 4096, 4096, 0, nil)
	if err != nil {
		t.Fatalf("CreateNamedPipe: %v", err)
	}
	defer windows.CloseHandle(h)

	connected := make(chan error, 1)
	go func() {
		connected <- windows.ConnectNamedPipe(h, nil)
	}()

	conn, err := dialIPC(name)
	if err != nil {
		t.Fatalf("dialIPC: %v", err)
	}
	defer conn.Close()
	if err := <-connected; err != nil && err != windows.ERROR_PIPE_CONNECTED {
		t.Fatalf("ConnectNamedPipe: %v", err)
	}

	file, ok := conn.(*os.File)
	if !ok {
		t.Fatalf("dialIPC 返回 %T, want *os.File", conn)
	}
	if err := file.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline 不可用（管道句柄未按 OVERLAPPED/可轮询方式打开）: %v", err)
	}
	if _, err := file.Read(make([]byte, 1)); err == nil {
		t.Fatal("Read err = nil, want deadline 超时错误（无人写入）")
	}
}
