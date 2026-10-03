// Package mpvproc 通过 mpv 子进程 + JSON IPC 实现 player.Player。
//
// --input-ipc-server 在 Linux/macOS 用 Unix socket、Windows 用命名管道
// （见 ipc_unix.go / ipc_windows.go）。所有平台统一使用该外部进程后端。
package mpvproc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/unbox/unbox/internal/player"
)

// ipcConnectTimeout 是连接 mpv IPC socket 的最大等待时间（mpv 启动后创建
// socket 有微小延迟，需重试）。
const ipcConnectTimeout = 5 * time.Second

// cmdResponseTimeout 是单条命令等待 mpv 应答的超时。
const cmdResponseTimeout = 5 * time.Second

// response 是 readLoop 路由给 send 的一条命令应答，携带会话代际，
// 供 send 丢弃跨会话串味的迟到应答。
type response struct {
	session int64
	data    []byte
}

type mpvProc struct {
	exePath string
	ipcPath string // --input-ipc-server 暴露的 IPC 路径（Unix socket 或 Windows 命名管道）

	// lifecycleMu 守护 cmd/conn/ipcPath/session：只在锁内做快照/赋值，
	// 绝不在持锁时阻塞（不做 IO、不等应答），且不与 sendMu 嵌套。
	lifecycleMu sync.Mutex
	session     int64 // 会话代际：Load 每次自增，用于丢弃跨会话串味的迟到应答
	cmd         *exec.Cmd
	wait        *waiter // 当前会话的收尸器，Close/超时/失败清理共用，避免 Wait 被调用两次
	conn        io.ReadWriteCloser
	wid         uintptr // 历史嵌入句柄，M4 默认不设置

	sendMu    sync.Mutex    // 串行化命令：保证任一时刻只有一条命令在飞
	responses chan response // readLoop 路由来的命令应答（带会话代际）
	events    chan player.Event
	// done 在当前会话被拆除（Close / 应答超时清理 / 被新 Load 顶掉）时关闭，
	// 让在飞的 send 立即返回，而不是干等 cmdResponseTimeout。与 conn 一样
	// 按「锁内认领、锁外使用」保证只被关闭一次。
	done chan struct{}

	stateMu sync.Mutex
	state   player.State
	// paused 记录用户是否主动暂停：暂停期间屏蔽缓存状态事件，
	// 避免把缓存回填/见底误报成恢复播放或缓冲。
	paused bool
}

// New 以指定 mpv 可执行文件启动一个播放器实例。
//
// 本层负责 mpv 进程生命周期与 JSON IPC 对话。窗口形态由 SetEmbedWindow
// 决定：shell 层嵌入装饰器喂入宿主句柄时 mpv 渲染进主窗口（--wid），
// 句柄为 0 时独立开窗（--force-window）。
func New(exePath string) (player.Player, error) {
	if _, err := os.Stat(exePath); err != nil {
		return nil, fmt.Errorf("mpv 可执行文件不可用: %w", err)
	}
	return &mpvProc{
		exePath:   exePath,
		responses: make(chan response, 16),
		events:    make(chan player.Event, 64),
		state:     player.State{Playing: player.StateStopped, Duration: -1},
	}, nil
}

// session 是一整套会话资源的认领快照。拆解一律「锁内认领、锁外执行」，
// Close、应答超时清理、Load 失败清理、并发 Load 顶替四条路径共用。
type session struct {
	conn    io.ReadWriteCloser
	cmd     *exec.Cmd
	wait    *waiter
	ipcPath string
	done    chan struct{}
}

// claimSessionLocked 快照并置空当前会话（调用方须持有 lifecycleMu）。
// 无可认领资源时返回 nil。
func (p *mpvProc) claimSessionLocked() *session {
	s := &session{
		conn: p.conn, cmd: p.cmd, wait: p.wait,
		ipcPath: p.ipcPath, done: p.done,
	}
	p.conn, p.cmd, p.wait, p.ipcPath, p.done = nil, nil, nil, "", nil
	if s.conn == nil && s.cmd == nil && s.done == nil && s.ipcPath == "" {
		return nil
	}
	return s
}

// teardown 拆解一个被认领的会话：关连接、放行在飞命令、杀进程收尸、清理
// IPC 端点。s 为 nil（本就无会话）时是空操作。
func (s *session) teardown() {
	if s == nil {
		return
	}
	if s.conn != nil {
		_ = s.conn.Close()
	}
	if s.done != nil {
		close(s.done) // 放行在飞的 send，避免它干等 5s 应答超时
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	// Kill 之后 Wait 立即返回；wait 为 nil（本就无进程）时是空操作。
	_ = s.wait.wait()
	if s.ipcPath != "" {
		cleanupIPC(s.ipcPath)
	}
}

// registerSession 登记新会话并返回其代际。并发 Load 下可能顶掉刚登记的
// 旧会话（Load 开头的 Close 在它出生之前跑过）——被顶掉的会话必须就地拆解，
// 否则它的 mpv 无人认领，进程与窗口永久泄漏。
func (p *mpvProc) registerSession(cmd *exec.Cmd, w *waiter, conn io.ReadWriteCloser, ipcPath string) int64 {
	p.lifecycleMu.Lock()
	displaced := p.claimSessionLocked()
	p.session++
	sess := p.session
	p.cmd, p.wait, p.conn, p.ipcPath = cmd, w, conn, ipcPath
	p.done = make(chan struct{})
	p.lifecycleMu.Unlock()
	displaced.teardown()
	return sess
}

func (p *mpvProc) Load(ctx context.Context, s player.Stream) error {
	// 关闭旧会话（Close 幂等：无会话时直接返回 nil）。
	_ = p.Close()
	ipcPath, err := newIPCPath()
	if err != nil {
		return err
	}

	p.lifecycleMu.Lock()
	wid := p.wid
	p.lifecycleMu.Unlock()
	args := buildArgs(s, ipcPath, wid)

	// mpv 起不来时（缺 DLL、被杀软拦截、参数过旧）唯一的原因只出现在它的
	// stderr 上，丢弃它会让这类故障只剩一句「连接 IPC 失败」，无从定位。
	stderr := &stderrTail{max: mpvStderrLimit}

	cmd := exec.CommandContext(ctx, p.exePath, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	setupProcAttr(cmd) // Windows：隐藏 mpv 子进程的控制台窗口
	if err := cmd.Start(); err != nil {
		cleanupIPC(ipcPath)
		return mpvLoadError(fmt.Errorf("启动 mpv 失败: %w", err), stderr.String())
	}

	w := startWaiter(cmd)

	conn, err := waitForIPC(dialIPC, ipcPath, ipcConnectTimeout, w)
	if err != nil {
		// 认领并拆掉刚启动的半成品会话：杀进程、收尸（此后 stderr 不再有
		// 写入、ProcessState 可读）、清理 IPC 端点。
		(&session{cmd: cmd, wait: w, ipcPath: ipcPath}).teardown()
		return mpvLoadError(withExitCode(err, cmd.ProcessState), stderr.String())
	}

	sess := p.registerSession(cmd, w, conn, ipcPath)
	p.stateMu.Lock()
	p.state = player.State{Playing: player.StatePlaying, Duration: -1, Volume: 80}
	p.paused = false
	p.stateMu.Unlock()

	go p.readLoop(conn, sess)

	// 观察位置、缓存暂停状态与用户暂停。观察失败不影响播放，故忽略错误。
	// pause 只用来屏蔽暂停期间的缓存事件，不会自己映射成缓冲信号。
	_ = p.send("observe_property", 0, "time-pos")
	_ = p.send("observe_property", 1, "paused-for-cache")
	_ = p.send("observe_property", 2, "pause")
	return nil
}

// SetEmbedWindow 设置 mpv 嵌入的宿主窗口句柄（X11 XID / Windows HWND）。
// 为 0 表示不嵌入，Load 时回退为 --force-window 独立窗口。
func (p *mpvProc) SetEmbedWindow(id uintptr) {
	p.lifecycleMu.Lock()
	p.wid = id
	p.lifecycleMu.Unlock()
}

// buildArgs 构造 mpv 启动参数。wid != 0 时嵌入宿主窗口并开 OSC；
// wid == 0 时独立开窗，但仍开启 OSC，让 mpv 窗口提供原生播放控件。
func buildArgs(s player.Stream, ipcPath string, wid uintptr) []string {
	args := []string{
		"--idle=yes",
		"--input-ipc-server=" + ipcPath,
		"--keep-open=yes",
		"--volume=80",
	}
	if wid != 0 {
		args = append(args, "--wid="+strconv.FormatUint(uint64(wid), 10), "--osc=yes")
	} else {
		args = append(args, "--force-window=yes", "--osc=yes")
	}
	for k, v := range s.Headers {
		args = append(args, "--http-header-fields="+k+": "+v)
	}
	args = append(args, s.URL)
	return args
}

func (p *mpvProc) Play() error  { return p.send("set_property", "pause", false) }
func (p *mpvProc) Pause() error { return p.send("set_property", "pause", true) }
func (p *mpvProc) Seek(sec float64) error {
	return p.send("seek", sec, "absolute")
}
func (p *mpvProc) SetVolume(v int) error {
	if err := p.send("set_property", "volume", v); err != nil {
		return err
	}
	p.stateMu.Lock()
	p.state.Volume = v
	p.stateMu.Unlock()
	return nil
}
func (p *mpvProc) SelectTrack(kind player.TrackKind, id int) error {
	return errors.New("mpvproc: SelectTrack 未实现")
}
func (p *mpvProc) State() player.State {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	return p.state
}
func (p *mpvProc) Events() <-chan player.Event { return p.events }

func (p *mpvProc) Close() error {
	// 锁内认领（幂等：无会话时认领到 nil），锁外拆解，保证不持锁做 IO。
	p.lifecycleMu.Lock()
	s := p.claimSessionLocked()
	p.lifecycleMu.Unlock()
	s.teardown()
	return nil
}

// send 串行发送一条命令并等待应答。readLoop 负责把命令应答路由到
// p.responses，异步事件路由到 p.events——两端互不干扰。
func (p *mpvProc) send(args ...any) error {
	p.sendMu.Lock()
	defer p.sendMu.Unlock()

	// 快照当前 conn、会话代际与拆除信号；并发 Close 只关掉旧 conn，此处
	// Write 返回 error 而非 nil-deref。不持锁等应答。
	p.lifecycleMu.Lock()
	c := p.conn
	sess := p.session
	done := p.done
	p.lifecycleMu.Unlock()
	if c == nil {
		return errors.New("mpvproc: 尚未 Load")
	}
	if _, err := c.Write([]byte(encodeCommand(args))); err != nil {
		return err
	}

	timer := time.NewTimer(cmdResponseTimeout)
	defer timer.Stop()
	for {
		select {
		case <-done:
			// 会话已被拆除（Close/应答超时清理/新 Load 顶替）：应答不会再来了。
			return errors.New("mpvproc: 会话已关闭")
		case r := <-p.responses:
			if r.session != sess {
				continue // 跨会话串味的迟到应答，丢弃继续等
			}
			var resp struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(r.data, &resp); err != nil {
				return fmt.Errorf("解析 mpv 应答失败: %w", err)
			}
			if resp.Error != "" && resp.Error != "success" {
				return errors.New(resp.Error)
			}
			return nil
		case <-timer.C:
			// 超时视为本会话连接已坏。仅在 p.conn 仍是自己写入的那条（指针同一）
			// 时才拆解，否则说明 Load 已重入建立新会话，不得误杀后继。
			p.lifecycleMu.Lock()
			same := p.conn == c
			var s *session
			if same {
				s = p.claimSessionLocked()
			}
			p.lifecycleMu.Unlock()

			if same {
				s.teardown()
				p.stateMu.Lock()
				p.state.Playing = player.StateStopped
				p.stateMu.Unlock()
			}
			return errors.New("mpvproc: 命令应答超时")
		}
	}
}

// readLoop 是连接上唯一的读取者。mpv 连接建立后不会主动发握手行，命令
// 应答与异步事件在同一流上交错，必须由单一 reader 分路，否则多个带预读
// 的 reader（bufio.Reader/Scanner）会互相抢字节。
// conn/sess 由 Load 传入并固定，保证每条命令应答都标上本会话代际。
func (p *mpvProc) readLoop(conn io.ReadWriteCloser, sess int64) {
	sc := bufio.NewScanner(conn)
	// 单行可能较长（如音轨/字幕列表），给足缓冲：起始 64KB、上限 1MB，
	// 避免 ReadBytes 默认 4096B 缓冲触顶误杀读循环。
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		var probe struct {
			Event string `json:"event"`
		}
		_ = json.Unmarshal(line, &probe)
		if probe.Event != "" {
			if paused, ok := parsePauseProperty(line); ok {
				// 用户主动暂停：只更新状态，不产生任何播放信号。
				p.stateMu.Lock()
				p.paused = paused
				if paused {
					p.state.Playing = player.StatePaused
				} else if p.state.Playing == player.StatePaused {
					p.state.Playing = player.StatePlaying
				}
				p.stateMu.Unlock()
				continue
			}
			if evt, ok := parseEvent(line); ok {
				p.stateMu.Lock()
				emit := true
				switch evt.Kind {
				case player.EventPosition:
					p.state.Position = evt.Position
				case player.EventBuffering:
					// 暂停期间的缓存状态变化不对外播报，否则会被前端当成
					// 「未起播/持续缓冲」而触发自动换源。
					if p.paused {
						emit = false
					} else {
						p.state.Playing = player.StateBuffering
					}
				case player.EventPlaying:
					if p.paused {
						emit = false
					} else {
						p.state.Playing = player.StatePlaying
					}
				case player.EventEOF, player.EventError:
					p.state.Playing = player.StateStopped
				}
				p.stateMu.Unlock()
				if emit {
					sendEvent(p.events, evt)
				}
			}
			continue
		}
		// 命令应答。Scanner 复用内部缓冲，必须先拷贝再入 channel，否则下一次
		// Scan 覆盖底层数组后，等待应答的 send 会读到脏数据。
		reply := append([]byte(nil), line...)
		select {
		case p.responses <- response{session: sess, data: reply}:
		default: // 无命令在等（理论上不应发生）
		}
	}
	// 连接断开：要么是本方主动拆除（已认领会话），要么是 mpv 进程自然退出。
	p.onConnectionEnd(conn, sess)
}

// onConnectionEnd 在读循环终止后判断会话归属。连接断开有两种来源：
//  1. Close / 应答超时 / 新 Load 顶替等主动拆除——它们已把 p.conn 置空或换成
//     新连接，这里直接返回；事件与状态由拆除方负责，绝不重复收尸；
//  2. mpv 进程自然退出（OSC 关闭按钮 exit 0、崩溃或被外部杀掉，IPC 随之
//     EOF）——会话仍登记在本连接上，此时认领并拆解、复位状态，并按退出码
//     上报 EventQuit（0，用户主动关闭，不触发故障切换）或 EventError
//     （非 0，异常退出，故障切换与错误提示依赖它）。
func (p *mpvProc) onConnectionEnd(conn io.ReadWriteCloser, sess int64) {
	p.lifecycleMu.Lock()
	if p.conn != conn || p.session != sess {
		p.lifecycleMu.Unlock()
		return
	}
	s := p.claimSessionLocked()
	p.lifecycleMu.Unlock()

	// 进程已死：Kill 幂等，wait 立即返回，顺带完成 IPC 端点清理。
	s.teardown()

	code := -1
	if s.cmd != nil && s.cmd.ProcessState != nil {
		code = s.cmd.ProcessState.ExitCode()
	}
	p.stateMu.Lock()
	p.state.Playing = player.StateStopped
	p.paused = false
	p.stateMu.Unlock()

	if code == 0 {
		sendEvent(p.events, player.Event{Kind: player.EventQuit})
		return
	}
	if hint := exitHint(code); hint != "" {
		sendEvent(p.events, player.Event{Kind: player.EventError,
			Err: fmt.Errorf("mpv 进程异常退出（退出码 %d：%s）", code, hint)})
		return
	}
	sendEvent(p.events, player.Event{Kind: player.EventError,
		Err: fmt.Errorf("mpv 进程异常退出（退出码 %d）", code)})
}

// sendEvent 把事件发往通道：终端事件（EOF/Error/Quit）阻塞发送保证送达
// （失败自动切换与嵌入层收场依赖它们），其余事件通道满则丢弃以避免阻塞读循环。
func sendEvent(ch chan<- player.Event, evt player.Event) {
	switch evt.Kind {
	case player.EventEOF, player.EventError, player.EventQuit:
		ch <- evt
	default:
		select {
		case ch <- evt:
		default:
		}
	}
}
