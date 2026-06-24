//go:build linux

package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Linux native 隔离,三级择优:
//   1. bubblewrap(bwrap):根只读 + 可写目录叠加 + PID/UTS/IPC namespace 隔进程。最强。
//   2. Landlock(内核 ≥5.13):纯文件写禁闭(无进程隔离),按路径授权、不改文件标签。无需装任何东西。
//   3. 都没有 → 退软黑名单(由 SandboxCheck 调 nativePolicyCheck)。
// 网络始终开(否则 go mod / npm / git fetch 全断)。读不限,只禁"写到 workspace 外"。
//
// Landlock 的限制一旦施加于某进程便不可逆,所以不能加在长驻的 deepx 上,只能加在"将要执行命令的那个
// 进程"里。做法是 re-exec 跳板:nativeShellCmd 让命令以「deepx 自身 + 一组 env 标记」启动,启动后的
// deepx 在 main() 最早处(RunSandboxTrampolineIfRequested)识别标记 → 施加 Landlock → exec 真正的
// sh -c <命令>。Landlock 限制随 execve 保留,从而约束到命令本身及其子进程。

const (
	sbxTrampolineEnv = "DEEPX_SBX_LANDLOCK" // =1 标记本进程是 Landlock 跳板
	sbxWritableEnv   = "DEEPX_SBX_WRITABLE" // 可写根列表(PathListSeparator 分隔)
	sbxCmdEnv        = "DEEPX_SBX_CMD"       // 要执行的 shell 命令
	sbxCwdEnv        = "DEEPX_SBX_CWD"       // 工作目录
)

var (
	bwrapProbeOnce sync.Once
	bwrapProbeOK   bool
	llProbeOnce    sync.Once
	llProbeOK      bool
)

// bwrapAvailable 实跑一个极简 bwrap 沙箱确认真能用(很多发行版禁用非特权 userns,装了也运行时报错)。
func bwrapAvailable() bool {
	bwrapProbeOnce.Do(func() {
		if _, err := exec.LookPath("bwrap"); err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		err := exec.CommandContext(ctx, "bwrap",
			"--ro-bind", "/", "/", "--proc", "/proc", "--unshare-pid",
			"sh", "-c", ":").Run()
		bwrapProbeOK = err == nil
	})
	return bwrapProbeOK
}

// landlockAvailable 仅查询内核 Landlock ABI 版本(纯探测,不施加任何限制)。≥1 即支持。
func landlockAvailable() bool {
	llProbeOnce.Do(func() {
		llProbeOK = landlockABIVersion() >= 1
	})
	return llProbeOK
}

// landlockABIVersion 用裸 syscall 查询内核 Landlock ABI 版本,返回 0 表示不支持
// (内核 <5.13 / 未编进 / 未在 cmdline 启用)。landlock_create_ruleset 在 attr=NULL、size=0、
// flags=LANDLOCK_CREATE_RULESET_VERSION 时直接返回 ABI 版本号(正整数)而非 fd。
// 走标准库 syscall(仅当前线程),不经 go-landlock/libpsx 的全线程同步,故 cgo/iscgo 下也安全。
func landlockABIVersion() int {
	r, _, e := syscall.Syscall(uintptr(unix.SYS_LANDLOCK_CREATE_RULESET), 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if e != 0 {
		return 0
	}
	return int(r)
}

// nativeIsolationAvailable 报告本机能否做 native OS 隔离(bwrap 或 Landlock 任一)。
// 都没有 → false,SandboxCheck 退软黑名单。探测各缓存一次。
func nativeIsolationAvailable() bool {
	return bwrapAvailable() || landlockAvailable()
}

// nativeShellCmd 按优先级构造隔离命令:bwrap > Landlock > 裸 shell。
func nativeShellCmd(command, cwd string) *exec.Cmd {
	if bwrapAvailable() {
		return bwrapShellCmd(command, cwd)
	}
	if landlockAvailable() {
		if c := landlockShellCmd(command, cwd); c != nil {
			return c
		}
	}
	return plainShellCmd(command, cwd)
}

// bwrapShellCmd 构造在 bwrap 沙箱里跑命令的 *exec.Cmd。
func bwrapShellCmd(command, cwd string) *exec.Cmd {
	args := []string{
		"--ro-bind", "/", "/", // 整个根只读
		"--dev", "/dev", // 干净的 /dev
		"--proc", "/proc", // 配合 PID namespace 的新 /proc
		"--unshare-pid", "--unshare-uts", "--unshare-ipc", // 进程隔离
		"--die-with-parent", // deepx 退出则沙箱进程一起死
	}
	// 可写目录:在只读根之上叠加可写绑定。用 --bind-try 而非 --bind:候选含 macOS 专属路径
	// (/private/tmp、~/Library/Caches),Linux 上不存在,普通 --bind 绑不存在的 source 会致命报错。
	for _, p := range nativeWritableRoots(cwd) {
		args = append(args, "--bind-try", p, p)
	}
	if cwd != "" {
		args = append(args, "--chdir", cwd)
	}
	args = append(args, "sh", "-c", command)
	return exec.Command("bwrap", args...)
}

// landlockShellCmd 以 deepx 自身作 re-exec 跳板,带 env 标记启动;跳板进程负责施加 Landlock 再 exec 真命令。
func landlockShellCmd(command, cwd string) *exec.Cmd {
	exe, err := os.Executable()
	if err != nil {
		return nil // 拿不到自身路径就放弃 Landlock,退裸 shell
	}
	c := exec.Command(exe)
	c.Env = append(os.Environ(),
		sbxTrampolineEnv+"=1",
		sbxWritableEnv+"="+strings.Join(nativeWritableRoots(cwd), string(os.PathListSeparator)),
		sbxCmdEnv+"="+command,
		sbxCwdEnv+"="+cwd,
	)
	return c
}

// RunSandboxTrampolineIfRequested 必须在 main() 最早处调用。
// 若本进程带 Landlock 跳板标记:施加"读全局 / 只写可写根"的 Landlock 写禁闭,然后 exec sh -c <命令>,
// 永不返回。否则立即返回,deepx 正常启动。
func RunSandboxTrampolineIfRequested() {
	if os.Getenv(sbxTrampolineEnv) != "1" {
		return
	}
	cwd := os.Getenv(sbxCwdEnv)
	command := os.Getenv(sbxCmdEnv)
	var roots []string
	if w := os.Getenv(sbxWritableEnv); w != "" {
		roots = filepath.SplitList(w)
	}
	if cwd != "" {
		_ = os.Chdir(cwd)
	}

	// 关键:把"prctl(NO_NEW_PRIVS) → landlock_restrict_self → execve"钉在同一 OS 线程上。
	// Landlock 域与 no_new_privs 都是线程属性、随 execve 继承;execve 会终止其余线程并以本线程映像
	// 替换整个进程,故只需限制本线程即可约束到真正的命令及其后代 —— 无需 go-landlock 的全线程同步,
	// 从而彻底避开 cgo/iscgo 下 runtime 拒绝 doAllThreadsSyscall 的 panic(issue #138)。不解锁(execve 永不返回)。
	runtime.LockOSThread()

	// BestEffort:Landlock 不可用 / 施加失败一律吞掉,绝不因此拒跑命令(最坏退化为不隔离)。
	_ = applyLandlockRestrict(roots)

	// 清掉跳板自用的 env,避免泄漏给子命令(否则子命令里再起 deepx 会被误判为跳板)。
	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, sbxTrampolineEnv+"=") || strings.HasPrefix(kv, sbxWritableEnv+"=") ||
			strings.HasPrefix(kv, sbxCmdEnv+"=") || strings.HasPrefix(kv, sbxCwdEnv+"=") {
			continue
		}
		env = append(env, kv)
	}

	sh, err := exec.LookPath("sh")
	if err != nil {
		sh = "/bin/sh"
	}
	// exec 替换当前进程映像;Landlock 域随 execve 保留 → 真正约束 sh 及其后代。
	_ = syscall.Exec(sh, []string{"sh", "-c", command}, env)
	os.Exit(127) // 只有 exec 失败才会走到这
}

// === Landlock 裸 syscall 施加 ===
//
// 不用 go-landlock 库:它在内核 ABI <V8 时必须把 PR_SET_NO_NEW_PRIVS 经 libpsx 同步到所有 OS 线程,
// 而 Go runtime 在 cgo/iscgo=true(deepx 因 purego 动态链接正是如此)下拒绝该全线程 syscall 并 panic
// (issue #138)。这里改用标准库 syscall 在当前(已 LockOSThread 的)线程上直接施加 —— 因跳板施加完
// 立刻 execve、Landlock 域随 execve 继承,单线程足矣,且彻底不碰 libpsx。
//
// 参数结构体 / 常量取自 golang.org/x/sys/unix(内核 <linux/landlock.h> 的官方映射)。

// landlockFSReadExec 是只读根 / 授予的访问位:执行 + 读文件 + 列目录,不含任何写 / 改位。
const landlockFSReadExec = unix.LANDLOCK_ACCESS_FS_EXECUTE |
	unix.LANDLOCK_ACCESS_FS_READ_FILE |
	unix.LANDLOCK_ACCESS_FS_READ_DIR

// landlockHandledFS 返回该 ABI 下要"接管"(handle)的全部 FS 访问位。
// 只请求内核认识的位:V1 的 13 个基础位,Refer(≥V2)/Truncate(≥V3)/IoctlDev(≥V5)按版本叠加。
// 请求子集永不 EINVAL;授给可写根时用这整套(含写),授给只读根时只给其中的读 / 执行子集。
func landlockHandledFS(abi int) uint64 {
	var m uint64 = unix.LANDLOCK_ACCESS_FS_EXECUTE |
		unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
		unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM
	if abi >= 2 {
		m |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		m |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= 5 {
		m |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	return m
}

// applyLandlockRestrict 在当前 OS 线程施加 Landlock 文件写禁闭:读 / 执行整个根可,只写 writable 根。
// 必须在 runtime.LockOSThread() 之后、execve 之前调用。任何一步失败都返回 error(调用方按 BestEffort 吞掉)。
func applyLandlockRestrict(writable []string) error {
	abi := landlockABIVersion()
	if abi < 1 {
		return fmt.Errorf("landlock 不可用")
	}
	handled := landlockHandledFS(abi)

	attr := unix.LandlockRulesetAttr{Access_fs: handled}
	fd, _, e := syscall.Syscall(uintptr(unix.SYS_LANDLOCK_CREATE_RULESET),
		uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if e != 0 {
		return fmt.Errorf("landlock_create_ruleset: %v", e)
	}
	defer syscall.Close(int(fd))

	// 读 / 执行整个根(不含写位),保证能跑二进制、读任意文件。
	if err := landlockAddPathRule(int(fd), "/", landlockFSReadExec); err != nil {
		return err
	}
	// 可写根:授全部已接管的位(含写)。打不开的目录(不存在等)跳过 —— 等价 IgnoreIfMissing。
	for _, dir := range writable {
		if dir == "" {
			continue
		}
		_ = landlockAddPathRule(int(fd), dir, handled)
	}

	// 非 root 下 landlock_restrict_self 要求先置 no_new_privs;单线程 prctl 即可(不经 libpsx)。
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("prctl(NO_NEW_PRIVS): %v", err)
	}
	if _, _, e := syscall.Syscall(uintptr(unix.SYS_LANDLOCK_RESTRICT_SELF), fd, 0, 0); e != 0 {
		return fmt.Errorf("landlock_restrict_self: %v", e)
	}
	return nil
}

// landlockAddPathRule 给 ruleset 加一条 path_beneath 规则:dir 子树允许 access 指定的访问位。
// 用 O_PATH 打开目录(轻量、不需读权限),fd 仅供本次 add_rule 引用。
func landlockAddPathRule(rulesetFd int, dir string, access uint64) error {
	dirFd, err := unix.Open(dir, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %q: %v", dir, err)
	}
	defer unix.Close(dirFd)
	attr := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(dirFd)}
	if _, _, e := syscall.Syscall6(uintptr(unix.SYS_LANDLOCK_ADD_RULE),
		uintptr(rulesetFd), unix.LANDLOCK_RULE_PATH_BENEATH,
		uintptr(unsafe.Pointer(&attr)), 0, 0, 0); e != 0 {
		return fmt.Errorf("landlock_add_rule %q: %v", dir, e)
	}
	return nil
}
