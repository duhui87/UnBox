package mpvproc

import (
	"errors"
	"strings"
	"testing"
)

// 加载器级失败的三个真实退出码必须翻译成可操作的原因；其余退出码（普通参数
// 错误之类）只报码不猜原因。
func TestExitHintExplainsWindowsLoaderFailures(t *testing.T) {
	cases := map[int]string{
		0xC0000135: "缺少依赖 DLL",
		0xC0000139: "版本过旧",
		0xC0000142: "初始化失败",
		3:          "",
		0:          "",
	}
	for code, want := range cases {
		hint := exitHint(code)
		if want == "" {
			if hint != "" {
				t.Fatalf("exitHint(%#X) = %q, 不认识的码不该猜原因", code, hint)
			}
			continue
		}
		if !strings.Contains(hint, want) {
			t.Fatalf("exitHint(%#X) = %q, want 含 %q", code, hint, want)
		}
	}
}

// 没有 ProcessState（收尸失败）或错误不是「提前退出」时，原样返回，不得凭空
// 拼一个退出码。
func TestWithExitCodeKeepsErrorWhenUnavailable(t *testing.T) {
	sentinel := errMPVExitedEarly
	if got := withExitCode(sentinel, nil); got != sentinel {
		t.Fatalf("state 为 nil 应原样返回，实际 %v", got)
	}
	other := errors.New("连接 mpv IPC 失败")
	if got := withExitCode(other, nil); got != other {
		t.Fatalf("非提前退出的错误应原样返回，实际 %v", got)
	}
}
