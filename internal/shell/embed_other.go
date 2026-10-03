//go:build !windows

package shell

// noopOverlay 是不支持窗口嵌入的平台占位：show 恒返回 0，Load 据此回退
// mpv --force-window 独立窗口。macOS 受 mpv 自身限制不支持 --wid 嵌入；
// Linux 的 X11 XID 嵌入待做（需 cgo 取 XID，且需真机验证）。
type noopOverlay struct{}

func (noopOverlay) show(uintptr) uintptr { return 0 }
func (noopOverlay) resize()              {}
func (noopOverlay) hide()                {}

func newOverlaySurface() overlaySurface { return noopOverlay{} }
