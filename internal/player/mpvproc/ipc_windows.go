//go:build windows

package mpvproc

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
)

// pipeSeq 保证同进程内命名管道名唯一。
var pipeSeq atomic.Uint64

// newIPCPath 返回一个唯一的命名管道基础名（不带 \\.\pipe\ 前缀）。mpv 的
// --input-ipc-server 在 Windows 上创建命名管道，并自行加 \\.\pipe\ 前缀。
// 名字里带上进程 PID：崩溃/被杀的上一进程可能留下还占着 unbox-mpv-N 的僵尸
// mpv，新进程若从 seq=1 重开会连上它——僵尸服务端不再读命令，后续 WriteFile
// 全部永久阻塞。
func newIPCPath() (string, error) {
	return fmt.Sprintf("unbox-mpv-%d-%d", os.Getpid(), pipeSeq.Add(1)), nil
}

// dialIPC 单次尝试连接 mpv 的命名管道。必须以 FILE_FLAG_OVERLAPPED 打开：
// os.OpenFile 得到的非 OVERLAPPED 句柄不进 Go 轮询器，readLoop 的并发读与
// send 的命令写入会互相卡死——写 IRP 永远不完成，mpv 根本收不到命令
// （表现为 send 无限阻塞在 WriteFile 上）。重试与超时由 waitForIPC 统一
// 处理——它还要在重试期间监视 mpv 是否已经退出。
func dialIPC(path string) (io.ReadWriteCloser, error) {
	h, err := syscall.CreateFile(
		syscall.StringToUTF16Ptr(`\\.\pipe\`+path),
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), `\\.\pipe\`+path), nil
}

// cleanupIPC 命名管道随 mpv 退出自动销毁，无需显式删除。
func cleanupIPC(path string) {}

// setupProcAttr 隐藏 mpv 子进程的控制台窗口（mpv.exe 是控制台程序，否则从
// GUI 应用启动会弹出一个黑色终端）。CREATE_NO_WINDOW 禁止创建控制台。
func setupProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}
