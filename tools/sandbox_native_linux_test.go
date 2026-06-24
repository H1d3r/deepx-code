//go:build linux

package tools

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestMain 复刻 deepx main() 的最早一步:带跳板标记的子进程走 Landlock 跳板,施加后 exec 真命令、永不返回。
// 没有这一步,下面端到端测试 re-exec 出来的子测试进程就不会进跳板(issue #138 的修复点正是这条路径)。
func TestMain(m *testing.M) {
	RunSandboxTrampolineIfRequested()
	os.Exit(m.Run())
}

// landlockHandledFS 的掩码必须与内核 ABI 逐级对齐(对照 go-landlock 的 abiInfos:
// V1=(1<<13)-1、V2=(1<<14)-1、V3=(1<<15)-1、V5=(1<<16)-1)。写错一位 = 要么 EINVAL 要么沙箱漏写。
func TestLandlockHandledFSMask(t *testing.T) {
	cases := []struct {
		abi  int
		want uint64
	}{
		{1, 0x1fff}, // 13 个基础位
		{2, 0x3fff}, // + REFER
		{3, 0x7fff}, // + TRUNCATE
		{4, 0x7fff}, // V4 只加了 net,FS 位不变
		{5, 0xffff}, // + IOCTL_DEV
		{8, 0xffff}, // 更高版本 FS 位不再增加
	}
	for _, c := range cases {
		if got := landlockHandledFS(c.abi); got != c.want {
			t.Errorf("landlockHandledFS(%d) = %#x, want %#x", c.abi, got, c.want)
		}
	}
	// 只读根的访问位必须是"读/执行",且不含任何写 / 改 / refer / truncate 位。
	writeBits := landlockHandledFS(8) &^ (unix.LANDLOCK_ACCESS_FS_EXECUTE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR)
	if landlockFSReadExec&writeBits != 0 {
		t.Errorf("只读根访问位 %#x 不应含任何写位", landlockFSReadExec)
	}
}

// TestLandlockTrampolineConfinesWrites 端到端验证 issue #138 的修复:
// 通过 Landlock 跳板跑命令,workspace 内可写、workspace 外被内核拒绝,且全程不 panic。
// 仅在内核支持 Landlock 时跑;否则跳过(CI / 老内核 / 容器未开)。
func TestLandlockTrampolineConfinesWrites(t *testing.T) {
	if !landlockAvailable() {
		t.Skip("内核无 Landlock 支持,跳过端到端测试")
	}

	ws := t.TempDir() // workspace = 可写根(也作 cwd)

	// "workspace 外"目录:放在 $HOME 根下(我们的策略里 ~ 只读、只有 ~/.cache 等子目录可写),
	// 而非 /tmp(/tmp 是可写根,放那测不出禁闭)。
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("拿不到 HOME,跳过: %v", err)
	}
	outside, err := os.MkdirTemp(home, ".deepx-ll-test-")
	if err != nil {
		t.Skipf("无法在 HOME 下建测试目录,跳过: %v", err)
	}
	defer os.RemoveAll(outside)

	run := func(command string) error {
		c := landlockShellCmd(command, ws) // 直接走 Landlock 跳板,绕过 bwrap 优先级
		if c == nil {
			t.Fatal("landlockShellCmd 返回 nil")
		}
		return c.Run()
	}

	// 1) workspace 内写:应成功。
	if err := run("echo hi > inside.txt"); err != nil {
		t.Fatalf("workspace 内写应成功: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "inside.txt")); err != nil {
		t.Fatalf("inside.txt 应已创建: %v", err)
	}

	// 2) workspace 外写:应被 Landlock 拒绝(命令非零退出 + 文件不存在)。
	escape := filepath.Join(outside, "escape.txt")
	if err := run("echo hi > " + escape); err == nil {
		t.Fatalf("workspace 外写应被拒,却成功了: %s", escape)
	}
	if _, err := os.Stat(escape); err == nil {
		t.Fatalf("escape.txt 不该被创建 —— 越界写未被拦住")
	}
}
