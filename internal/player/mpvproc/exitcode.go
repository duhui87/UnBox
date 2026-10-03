package mpvproc

import (
	"errors"
	"fmt"
	"os"
)

// withExitCode 在 mpv 提前退出时把退出码拼进错误。
//
// 加载器级失败（缺 DLL、DLL 版本过旧、被杀软拦截）发生在 mpv 自身代码运行
// 之前，它不会往 stderr 写任何东西，退出码是唯一还能定位原因的信息——
// stderr 那一层（mpvLoadError）对这类故障完全无话可说。非「提前退出」的错误
// （如 IPC 连接超时后由调用方自己 Kill）其退出码没有诊断价值，原样返回。
func withExitCode(err error, state *os.ProcessState) error {
	if state == nil || !errors.Is(err, errMPVExitedEarly) {
		return err
	}
	code := state.ExitCode()
	if code < 0 {
		// Unix 被信号终止等无法解读的情形，不拼一个误导性的码。
		return err
	}
	if hint := exitHint(code); hint != "" {
		return fmt.Errorf("%w（退出码 0x%X：%s）", err, code, hint)
	}
	return fmt.Errorf("%w（退出码 0x%X）", err, code)
}

// exitHint 把 Windows 加载器级退出码翻译成可操作的原因；认不出的返回空串。
// 这些高位码是 Windows NTSTATUS，其他平台的正常退出码（0–255）不会撞上。
func exitHint(code int) string {
	switch uint32(code) {
	case 0xC0000135: // STATUS_DLL_NOT_FOUND：依赖 DLL 根本不存在
		return "缺少依赖 DLL，如 vulkan-1.dll 未安装"
	case 0xC0000139: // STATUS_ENTRYPOINT_NOT_FOUND：DLL 在但版本过旧
		return "依赖 DLL 版本过旧、缺少所需入口点，如 vulkan-1.dll 版本过低"
	case 0xC0000142: // STATUS_DLL_INIT_FAILED：DLL 存在但初始化失败
		return "依赖 DLL 初始化失败"
	}
	return ""
}
