package shell

import (
	"errors"
	"os"
	"runtime"
	"testing"

	"github.com/unbox/unbox/internal/player/mpvplugin"
)

func TestPickPlayerHappyPath(t *testing.T) {
	// 本机已装 mpv，按平台名查找应命中；PickPlayer 返回非 nil 的播放器且无错误。
	// 保守起见先探测：若本机确实没有 mpv，跳过 happy path 断言，避免误报。
	if _, err := lookPath(mpvplugin.ExeForOS(runtime.GOOS)); err != nil {
		t.Skipf("本机未安装 mpv，跳过 happy path: %v", err)
	}

	p, err := PickPlayer()
	if err != nil {
		t.Fatalf("PickPlayer() err = %v, want nil", err)
	}
	if p == nil {
		t.Fatal("PickPlayer() = nil player, want non-nil")
	}
	_ = p.Close()
}

func TestPickPlayerMissingMpv(t *testing.T) {
	// 用桩替换 lookPath，覆盖「mpv 缺失」分支：应返回明确错误而非 panic。
	orig := lookPath
	lookPath = func(string) (string, error) {
		return "", errors.New("exec: mpv not found")
	}
	defer func() { lookPath = orig }()
	origStatus := pluginStatus
	pluginStatus = func() mpvplugin.Status { return mpvplugin.Status{} }
	defer func() { pluginStatus = origStatus }()

	p, err := PickPlayer()
	if err == nil {
		t.Fatal("PickPlayer() err = nil, want non-nil（mpv 缺失应报错）")
	}
	if p != nil {
		t.Fatalf("PickPlayer() = %v, want nil player", p)
	}
}

// TestPickPlayerLooksUpPlatformExecutableName 保证 PATH 查找用平台对应的可执行名。
// Windows 上 exec.LookPath("mpv") 按 PATHEXT 优先命中 mpv.com 启动器：它拉起的
// 真 mpv.exe 是孙进程，mpvproc.Close 的 Kill 只杀得到启动器，真 mpv 泄漏并把
// 收尸管道攥住，导致 Close 永久挂起。
func TestPickPlayerLooksUpPlatformExecutableName(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "fake-mpv")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	tmp.Close()

	orig := lookPath
	var got string
	lookPath = func(name string) (string, error) {
		got = name
		return tmp.Name(), nil
	}
	defer func() { lookPath = orig }()

	p, err := PickPlayer()
	if err != nil {
		t.Fatalf("PickPlayer() err = %v, want nil", err)
	}
	if p == nil {
		t.Fatal("PickPlayer() = nil player, want non-nil")
	}
	defer p.Close()

	want := "mpv"
	if runtime.GOOS == "windows" {
		want = "mpv.exe"
	}
	if got != want {
		t.Fatalf("lookPath 请求名 = %q, want %q", got, want)
	}
}
