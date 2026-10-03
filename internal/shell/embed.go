package shell

import (
	"context"
	"log"
	"sync"

	"github.com/unbox/unbox/internal/player"
)

// overlaySurface 是主窗口内的嵌入宿主子窗口：mpv 以它为 --wid 宿主，画面
// 整窗接管主窗口；隐藏它即可把主窗口还给 UI。平台实现：
//   - embed_windows.go：真实 WS_CHILD 覆盖窗口（Windows）；
//   - embed_other.go：恒返回 0 的占位（macOS 受 mpv 限制不支持 --wid，
//     Linux 嵌入待做），Load 据此回退 --force-window 独立窗口。
type overlaySurface interface {
	// show 显示 overlay 并返回其宿主句柄（mpv --wid 用）。parent 为 0 或
	// 创建失败返回 0，调用方回退独立窗口；重复 show 幂等（重新置顶显示）。
	show(parent uintptr) uintptr
	// resize 让 overlay 重新铺满父窗口客户区（窗口缩放 / DPI 变化时调用）；
	// 未创建时是空操作，隐藏状态保持隐藏。
	resize()
	// hide 隐藏 overlay（连同其中的 mpv 视频子窗口）；未创建时是空操作。
	hide()
}

// embedder 是「主窗口整窗接管」的播放器装饰器：
//   - Load 前显示 overlay 并把句柄喂给内层（player.Embedder → mpv --wid），
//     Load 失败立即隐藏，绝不把黑屏留给 UI；
//   - 内层终端事件（EOF/Error/Quit）与 Close 时隐藏 overlay，把主窗口还给 UI；
//   - 事件由本层单一循环消费并重新对外发布，转发策略与 failover 相同
//     （终端阻塞送达、其余满则丢弃），避免消费端反压到 mpv 读循环。
type embedder struct {
	ov       overlaySurface
	host     func() uintptr
	inner    player.Player
	events   chan player.Event
	done     chan struct{}
	loopDone chan struct{}
	stopOnce sync.Once
}

// newEmbedder 包装 inner 并启动事件循环。host 返回宿主窗口句柄，0 表示
// 不可嵌入（独立窗口回退）。
func newEmbedder(inner player.Player, ov overlaySurface, host func() uintptr) *embedder {
	e := &embedder{
		ov:       ov,
		host:     host,
		inner:    inner,
		events:   make(chan player.Event, 64),
		done:     make(chan struct{}),
		loopDone: make(chan struct{}),
	}
	go e.loop()
	return e
}

// Load 显示 overlay、把宿主句柄交给内层再加载流；失败时隐藏 overlay。
func (e *embedder) Load(ctx context.Context, s player.Stream) error {
	hwnd := uintptr(0)
	if parent := e.host(); parent != 0 {
		hwnd = e.ov.show(parent)
	}
	if em, ok := e.inner.(player.Embedder); ok {
		em.SetEmbedWindow(hwnd)
	}
	if hwnd == 0 {
		log.Printf("嵌入宿主不可用（hwnd=0），mpv 以独立窗口播放")
	} else {
		log.Printf("mpv 整窗接管已启用（overlay hwnd=%d）", hwnd)
	}
	if err := e.inner.Load(ctx, s); err != nil {
		e.ov.hide()
		return err
	}
	return nil
}

// Close 拆掉内层播放器后隐藏 overlay，把主窗口还给 UI。
func (e *embedder) Close() error {
	err := e.inner.Close()
	e.ov.hide()
	return err
}

// resize 转发给 overlay 重新铺满父窗口客户区。
func (e *embedder) resize() { e.ov.resize() }

// loop 是内层事件的唯一消费者：终端事件先隐藏 overlay 再转发；内层事件
// 通道关闭时同步关闭本层通道，让上层桥接照常退出（与不包嵌入层时一致）。
func (e *embedder) loop() {
	defer close(e.loopDone)
	for {
		select {
		case <-e.done:
			return
		case ev, ok := <-e.inner.Events():
			if !ok {
				close(e.events)
				return
			}
			switch ev.Kind {
			case player.EventEOF, player.EventError, player.EventQuit:
				e.ov.hide()
			}
			if !e.forward(ev) {
				return
			}
		}
	}
}

// forward 把事件转发给上层。终端事件（EOF/Error/Quit）阻塞送达——嵌入层
// 收场与点播自动切集都依赖它们；其余事件上层跟不上时丢弃，绝不反压回内层。
func (e *embedder) forward(ev player.Event) bool {
	if ev.Kind == player.EventEOF || ev.Kind == player.EventError || ev.Kind == player.EventQuit {
		select {
		case e.events <- ev:
			return true
		case <-e.done:
			return false
		}
	}
	select {
	case e.events <- ev:
		return true
	case <-e.done:
		return false
	default:
		return true
	}
}

// stop 终止事件循环并等待其退出；幂等。停止后本层 events 不再关闭——
// 消费方（事件桥接）应已换到新包装器的通道上，关闭旧通道反而会把桥接
// 打成「通道关闭退出」。
func (e *embedder) stop() {
	e.stopOnce.Do(func() { close(e.done) })
	<-e.loopDone
}

func (e *embedder) Events() <-chan player.Event { return e.events }
func (e *embedder) State() player.State         { return e.inner.State() }
func (e *embedder) Play() error                 { return e.inner.Play() }
func (e *embedder) Pause() error                { return e.inner.Pause() }
func (e *embedder) Seek(sec float64) error      { return e.inner.Seek(sec) }
func (e *embedder) SetVolume(v int) error       { return e.inner.SetVolume(v) }
func (e *embedder) SelectTrack(k player.TrackKind, id int) error {
	return e.inner.SelectTrack(k, id)
}

// 全局嵌入状态：Embed 在播放器链上挂 embedder；AttachEmbedWindow（由
// OpenWindow 调用）注入主窗口宿主。host 未注入或返回 0 时回退独立窗口。
var (
	embedMu      sync.Mutex
	embedHostFn  func() uintptr
	embedOverlay overlaySurface
	currentEmbed *embedder
)

// Embed 把播放器接入主窗口嵌入装饰器并返回包装后的播放器；p 为 nil 原样
// 返回。重复调用会先收掉上一个 embedder 的事件循环（RefreshMPV 重包装用）。
func Embed(p player.Player) player.Player {
	if p == nil {
		return nil
	}
	embedMu.Lock()
	defer embedMu.Unlock()
	if currentEmbed != nil {
		currentEmbed.stop()
	}
	if embedOverlay == nil {
		embedOverlay = newOverlaySurface()
	}
	host := func() uintptr {
		embedMu.Lock()
		fn := embedHostFn
		embedMu.Unlock()
		if fn == nil {
			return 0
		}
		return fn()
	}
	e := newEmbedder(p, embedOverlay, host)
	currentEmbed = e
	return e
}

// AttachEmbedWindow 注入宿主窗口句柄来源，供 overlay 创建与 --wid 使用。
func AttachEmbedWindow(host func() uintptr) {
	embedMu.Lock()
	embedHostFn = host
	embedMu.Unlock()
}

// resizeCurrentEmbed 在宿主窗口尺寸/DPI 变化时让当前 overlay 重新铺满客户区。
func resizeCurrentEmbed() {
	embedMu.Lock()
	e := currentEmbed
	embedMu.Unlock()
	if e != nil {
		e.resize()
	}
}

// shutdownEmbed 收掉当前嵌入装饰器并隐藏 overlay（服务退出时调用）。
func shutdownEmbed() {
	embedMu.Lock()
	e := currentEmbed
	currentEmbed = nil
	ov := embedOverlay
	embedMu.Unlock()
	if e != nil {
		e.stop()
	}
	if ov != nil {
		ov.hide()
	}
}
