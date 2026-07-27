package tui

import (
	"errors"

	"github.com/atotto/clipboard"
)

// readClipboardText 读系统剪贴板里的**文本**。跨平台走 atotto/clipboard —— bubbles textarea
// 内部的 Paste 命令用的也是它,保持同一来源。
//
// Linux 上它依赖外部工具(wl-paste / xclip / xsel),缺工具时静默返错;那种情况由启动时的
// clipboardTextHint 提示用户安装,这里不重复报错。图片走各平台的 readClipboardImage。
func readClipboardText() (string, error) { return clipboard.ReadAll() }

// errNoClipboardImage 表示系统剪贴板当前没有图片数据 (可能是空,也可能只有文本)。
// 各平台的 readClipboardImage 实现应当把"没图"统一映射到这个值,
// 调用方据此区分"没图"与"读取出错"。
var errNoClipboardImage = errors.New("clipboard does not contain image data")
