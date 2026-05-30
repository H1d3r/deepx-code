//go:build linux

package tui

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
)

// clipboardTextHint 检测 Linux 上文本剪贴板工具是否齐全。返回非空字符串 =
// 用户需要装工具才能用 Ctrl+V 粘贴文本(默认 Ubuntu / Fedora 等大多不装这些)。
// 返回 "" = 工具就绪。
//
// 跟图片走的是同一套外部二进制(Linux 没法纯 Go 直读剪贴板),但**文本剪贴板
// 缺工具的症状是"Ctrl+V 完全无声音"** —— bubbles textarea 的 Paste 命令底下
// 调 atotto/clipboard.ReadAll(),它找不到 wl-paste/xclip/xsel 就静默返错。
// 启动时探一下,把这条隐形坑直接告诉用户。
func clipboardTextHint() string {
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		if _, err := exec.LookPath("wl-paste"); err == nil {
			return ""
		}
	}
	if _, err := exec.LookPath("xclip"); err == nil {
		return ""
	}
	if _, err := exec.LookPath("xsel"); err == nil {
		return ""
	}
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		return "⚠️ 未检测到剪贴板工具,Ctrl+V 粘贴文本会失效。Wayland 下装一下:`sudo apt install wl-clipboard`"
	}
	return "⚠️ 未检测到剪贴板工具,Ctrl+V 粘贴文本会失效。X11 下装一下:`sudo apt install xclip`"
}

// readClipboardImage 在 Linux 上调用 wl-paste (Wayland) 或 xclip (X11) 读取 image/png。
// 没有纯 Go 不带 cgo 的剪贴板二进制读取方案,只能依赖外部工具。
func readClipboardImage() ([]byte, error) {
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		if _, err := exec.LookPath("wl-paste"); err == nil {
			return runClipboardCmd("wl-paste", "--type", "image/png")
		}
	}
	if _, err := exec.LookPath("xclip"); err == nil {
		return runClipboardCmd("xclip", "-selection", "clipboard", "-t", "image/png", "-o")
	}
	return nil, errors.New("clipboard image requires wl-paste (Wayland) or xclip (X11) installed")
}

func runClipboardCmd(name string, args ...string) ([]byte, error) {
	var out bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		// 没图、剪贴板为空、目标类型不存在 都会让命令退出码非零
		return nil, errNoClipboardImage
	}
	if out.Len() == 0 {
		return nil, errNoClipboardImage
	}
	return out.Bytes(), nil
}
