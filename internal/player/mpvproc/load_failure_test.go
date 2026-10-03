package mpvproc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/unbox/unbox/internal/player"
)

// fakeMPVEnv 让测试二进制把自己当成「坏掉的 mpv」：打印一行 stderr 后立刻退出，
// 复现缺 DLL / 被杀软拦截时的真实表现——进程建得起来，但 IPC 管道永远不出现。
const fakeMPVEnv = "UNBOX_FAKE_MPV_STDERR"

// fakeMPVExitEnv 控制假 mpv 的退出码，用于复现 Windows 加载器级失败
// （0xC0000135 缺 DLL 等）——这类失败一个字都不往 stderr 写。
const fakeMPVExitEnv = "UNBOX_FAKE_MPV_EXIT"

func TestMain(m *testing.M) {
	msg, hasMsg := os.LookupEnv(fakeMPVEnv)
	exitStr, hasExit := os.LookupEnv(fakeMPVExitEnv)
	if hasMsg || hasExit {
		if msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
		code := 3
		if v, err := strconv.Atoi(exitStr); err == nil {
			code = v
		}
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// 这是本次修复的核心回归：mpv 启动即崩时，错误必须说明「启动后立即退出」，
// 并把 mpv 自己打印的原因原样带出来。此前 stderr 被丢进 io.Discard，
// 用户和我们能看到的只有一句「连接 mpv IPC 失败: 找不到管道」。
func TestLoadSurfacesMPVStderrWhenProcessDies(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("取不到测试二进制路径: %v", err)
	}
	t.Setenv(fakeMPVEnv, "Failed to load libmpv-2.dll")

	p, err := New(exe)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	err = p.Load(ctx, player.Stream{URL: "https://example.com/a.m3u8"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("mpv 起来即崩，Load 应报错")
	}
	if !errors.Is(err, errMPVExitedEarly) {
		t.Fatalf("错误应说明进程提前退出，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "libmpv-2.dll") {
		t.Fatalf("错误应带上 mpv 自己的输出，实际 %q", err.Error())
	}
	// 进程在毫秒级退出，不该再空等满 ipcConnectTimeout。
	if elapsed > ipcConnectTimeout {
		t.Fatalf("耗时 %v，应在进程退出后立即返回而不是等满超时", elapsed)
	}
}

// 加载器级失败（缺 DLL / 被杀软拦截）不会往 stderr 写任何东西，此前错误只剩
// 一句「mpv 启动后立即退出」，无从判断是参数问题还是环境问题。退出码是这类
// 失败唯一剩下的线索，必须带进错误里。
func TestLoadReportsExitCodeWhenMPVDiesEarly(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("取不到测试二进制路径: %v", err)
	}
	t.Setenv(fakeMPVEnv, "")
	t.Setenv(fakeMPVExitEnv, "42")

	p, err := New(exe)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err = p.Load(ctx, player.Stream{URL: "https://example.com/a.m3u8"})
	if !errors.Is(err, errMPVExitedEarly) {
		t.Fatalf("错误应说明进程提前退出，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "退出码 0x2A") {
		t.Fatalf("错误应带上十六进制退出码（want 退出码 0x2A），实际 %q", err.Error())
	}
}

// Windows 加载器级失败的两个真实退出码：0xC0000135（vulkan-1.dll 之类根本不在）
// 与 0xC0000139（DLL 在但版本过旧、缺入口点）。二者都无 stderr，退出码是唯一
// 的区分依据，必须翻译成可操作的原因提示。
func TestLoadExplainsWindowsLoaderExitCode(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("加载器级退出码是 Windows 特有，其他平台按低 8 位截断")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("取不到测试二进制路径: %v", err)
	}
	for _, tc := range []struct {
		exit int
		want string
	}{
		{0xC0000135, "缺少依赖 DLL"},
		{0xC0000139, "版本过旧"},
	} {
		t.Run(fmt.Sprintf("0x%X", tc.exit), func(t *testing.T) {
			t.Setenv(fakeMPVEnv, "")
			t.Setenv(fakeMPVExitEnv, strconv.Itoa(tc.exit))

			p, err := New(exe)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer p.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			err = p.Load(ctx, player.Stream{URL: "https://example.com/a.m3u8"})
			if !errors.Is(err, errMPVExitedEarly) {
				t.Fatalf("错误应说明进程提前退出，实际 %v", err)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("0x%X", tc.exit)) {
				t.Fatalf("错误应带退出码 %#X，实际 %q", tc.exit, err.Error())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误应提示 %q，实际 %q", tc.want, err.Error())
			}
		})
	}
}
