//go:build darwin

package tui

import (
	"fmt"
	"os"
	"os/exec"
)

// clipboardTextHint 在 macOS 上恒为空 —— pbcopy/pbpaste 是系统自带,不可能缺。
func clipboardTextHint() string { return "" }

// readClipboardImage 通过 osascript 把剪贴板中的 «class PNGf» 写到临时文件再读回。
// macOS 没有 cgo-free 的官方剪贴板二进制 API,osascript 是最干净的内置方案。
// 剪贴板没有图片时 osascript 退出码非零,统一映射为 errNoClipboardImage。
func readClipboardImage() ([]byte, error) {
	tmp, err := os.CreateTemp("", "deepx-clip-*.png")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	path := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(path)

	cmd := exec.Command("osascript",
		"-e", `set png_data to the clipboard as «class PNGf»`,
		"-e", fmt.Sprintf(`set fh to open for access POSIX file "%s" with write permission`, path),
		"-e", `write png_data to fh`,
		"-e", `close access fh`,
	)
	if err := cmd.Run(); err != nil {
		return nil, errNoClipboardImage
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read temp file: %w", err)
	}
	if len(data) == 0 {
		return nil, errNoClipboardImage
	}
	return data, nil
}
