package tui

import (
	"deepx/agent"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// padLinesToWidth 把每行强制对齐到精确 w 列宽:
//   - 短行 (实际 < w): 末尾补空格
//   - 长行 (实际 > w, 通常是 emoji 在 wrap 时被低估): 用 ansi.Cut 切到 w 列
//
// 都用 ansi.StringWidth / ansi.Cut 同一套测量,跟 lipgloss 后续 JoinHorizontal 对齐口径一致,
// 避免 emoji 行把滚动条 / 右栏分隔线推偏。
func padLinesToWidth(content string, w int) string {
	if w <= 0 {
		return content
	}
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		cur := lineDisplayWidth(line)
		switch {
		case cur < w:
			lines[i] = line + strings.Repeat(" ", w-cur)
		case cur > w:
			// 切到 [0, w),ansi.Cut 不会破坏 ANSI 转义,但有可能裁掉 emoji 的尾部 byte。
			// 这是 "emoji 宽度估算 vs 终端实际渲染" 不一致的不得已选择 — 留尾巴比错位强。
			lines[i] = ansi.Cut(line, 0, w)
		}
	}
	return strings.Join(lines, "\n")
}

// widthFunc 是运行时选定的宽度测量函数。关键差异是终端是否 honor VS16(U+FE0F):
//   - 不认 VS16 的终端把 `🌤️` 这类"基础符号 + VS16" emoji 渲染成 1 cell → 用 WcWidth
//   - 认 VS16 的终端把它渲染成 2 cell → 用 GraphemeWidth
//
// 归属:
//   - macOS Terminal.app:不认 VS16 → WcWidth
//   - VSCode 集成终端(xterm.js, 默认 unicodeVersion=6):同样不认 VS16,`🌤️` = 1 cell → WcWidth
//   - iTerm2 / WezTerm / Kitty / Ghostty / Alacritty:认 VS16 → GraphemeWidth
//   - Windows Terminal / 现代 conhost(Win10 1903+):认 VS16,emoji/符号渲染 2 cell → GraphemeWidth
//
// 启动时按 TERM_PROGRAM / 终端 env 变量选,匹配实际渲染行为,避免 divider 在含 emoji 的
// 行被推偏。未知非 Windows 终端默认 WcWidth(更保守 — 多数终端不严格 honors VS16)。
var widthFunc func(string) int = ansi.StringWidthWc

func init() {
	widthFunc = detectWidthFunc()
}

func detectWidthFunc() func(string) int {
	switch os.Getenv("TERM_PROGRAM") {
	case "Apple_Terminal", "vscode":
		// VSCode 内置终端在 Windows 上也是 xterm.js(unicode v6),1 cell → WcWidth。
		// 这个判断必须在 GOOS=windows 兜底之前,否则 Windows 上的 VSCode 终端会被误判。
		return ansi.StringWidthWc
	case "iTerm.app", "WezTerm", "ghostty":
		return ansi.StringWidth // GraphemeWidth 口径,认 VS16
	}
	if os.Getenv("KITTY_WINDOW_ID") != "" {
		return ansi.StringWidth
	}
	if os.Getenv("ALACRITTY_LOG") != "" || os.Getenv("ALACRITTY_WINDOW_ID") != "" {
		return ansi.StringWidth
	}
	// Windows Terminal 设 WT_SESSION;它和现代 conhost(PowerShell / cmd 默认宿主,
	// Win10 1903+)都 honor VS16,把插了 VS16 的图标(🔍 🐚 ⚙ ✅ 等)渲染成 2 cell。
	// 默认 WcWidth 会把这些算 1 cell → 那行被低估 1 列 → 右栏 divider 被顶偏。
	if runtime.GOOS == "windows" || os.Getenv("WT_SESSION") != "" {
		return ansi.StringWidth
	}
	return ansi.StringWidthWc
}

// lineDisplayWidth 测一行的显示宽度,口径在启动时按终端选定(见 widthFunc / detectWidthFunc)。
// 所有需要"跟终端实际渲染对齐"的地方(padLinesToWidth、normalizeFrame、scrollbar)都走这个。
func lineDisplayWidth(s string) int {
	return widthFunc(s)
}

// === Emoji presentation 修正 ===
//
// 问题:Unicode 里很多 emoji(如 🗡 U+1F5E1)默认是 text presentation(单 cell 文字字形),
// `ansi.StringWidth` 算 1 cell。但 macOS Terminal.app / 多数终端会把它图形化渲染成 2 cell
// emoji。程序按 1 cell pad,终端实际渲染 2 cell → 行被推宽 1 → scrollbar 那行向右浮动。
//
// 修法:在 emoji 后插入 VS16 (U+FE0F, Variation Selector 16, emoji presentation selector)。
// VS16 让 emoji 强制按 emoji presentation 渲染,**并让 ansi.StringWidth 也算 2 cell**,
// 两边度量对齐 → scrollbar 不再抖。VS16 本身 0 cell,视觉无副作用。
//
// 跟"插空格"方案对比:
//   - 插空格改变 LLM 输出的视觉间距(紧凑 "📁拆文件" 变 "📁 拆文件")
//   - 插 VS16 视觉零侵入,但只对 emoji 起作用,纯 wide 字符(全角标点等)无效
//   - 此场景下问题源都是 emoji,VS16 够用

// isEmojiLike 判断 rune 是否属于"需要 emoji presentation"的码点范围。
// 用 Unicode emoji block 列表而非 width 检测 —— text-presentation 默认的 emoji 在 ansi
// 度量下 width=1,width-based 检测会漏掉它们(`🗡` 就是漏判的典型)。
//
// 覆盖最常见的 Unicode emoji block,不追求 100% Extended_Pictographic 精确(需 uniseg 依赖)。
// 漏判个别罕见 emoji 顶多那一行还抖一下,不会引入错误行为。
func isEmojiLike(r rune) bool {
	// 先剔除落在下面 emoji 块里、但其实没有 emoji 变体序列的纯文字 dingbat。给它们塞 VS16
	// 是错的:VS16-honoring 终端(Windows Terminal 等)可能仍按文字渲染 1 cell,而 GraphemeWidth
	// 会算 2 → 那行被高估、divider 反向偏。覆盖 LLM 高频输出的对勾/叉,以及装饰括号。
	// 注意别误伤真 emoji:✔️(2714) / ✖️(2716) / ✡️(2721) 有 emoji 变体序列,不在此列。
	switch {
	case r == 0x2713 || r == 0x2715 || r == 0x2717 || r == 0x2718:
		// ✓ ✕ ✗ ✘:CHECK MARK / 各式 X,均无 emoji presentation
		return false
	case r >= 0x2768 && r <= 0x2775:
		// ❨ … ❵:中号 / 装饰括号(含输入提示符 ❱ U+2771),非 emoji
		return false
	}
	switch {
	case r >= 0x1F000 && r <= 0x1FFFF:
		// misc symbols & pictographs / emoticons / transport / supplemental symbols 等
		// 含 📁 🗡 🔍 🎯 🐋 🧠 等所有常见 emoji
		return true
	case r >= 0x2600 && r <= 0x27BF:
		// misc symbols & dingbats (✅ U+2705 / ❌ U+274C / ✨ / ☀ / ☎ / ✂ 等)
		return true
	case r >= 0x2300 && r <= 0x23FF:
		// misc technical (⌚ ⌛ ⏰ ⏳ ⏸ ⏯ 等)
		return true
	case r >= 0x2B00 && r <= 0x2BFF:
		// misc symbols and arrows (⬆ ⬇ ⭐ ⭕ 等)
		return true
	}
	return false
}

// ensureEmojiSpacing 处理 emoji 的两件事:
//  1. 强制 emoji presentation(在 emoji 后插 VS16 U+FE0F)— 让 ansi.StringWidth /
//     lipgloss / terminal 三方都把这个 emoji 算 2 cell,绕过 text-presentation default
//     字符(如 ⛅ ☀ ⚙ ✂)在 Unicode 标准里算 1 cell 但 Terminal.app/iTerm2 实际渲染 2 cell
//     的口径分歧。否则 divider / scrollbar 会在这类字符的行上偏移 1 cell。
//  2. 视觉分隔(emoji 后紧跟非空白字符时补一个空格)。
//
// 跳过插 VS16 的情况:
//   - 后跟 ZWJ (U+200D):emoji 组合序列内部不动 (例: 👨‍👩‍👧‍👦)
//   - 已经有 VS16:保留,不再重复
//   - 已经有 VS15 (U+FE0E text presentation 选择符):尊重用户意图,不覆盖
func ensureEmojiSpacing(s string) string {
	if s == "" {
		return s
	}
	runes := []rune(s)
	var sb strings.Builder
	sb.Grow(len(s) + 32)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		sb.WriteRune(r)
		if !isEmojiLike(r) {
			continue
		}
		if i+1 >= len(runes) {
			// emoji 在串尾:补 VS16,无需空格
			sb.WriteRune(0xFE0F)
			continue
		}
		next := runes[i+1]
		// ZWJ:跨 ZWJ 的组合序列,不动
		if next == 0x200D {
			continue
		}
		// 已有 VS16 / VS15:保留,推进游标到 selector 之后
		if next == 0xFE0F || next == 0xFE0E {
			sb.WriteRune(next)
			i++
			if i+1 >= len(runes) {
				continue
			}
			after := runes[i+1]
			if after != 0x200D && !isWhitespaceLike(after) {
				sb.WriteRune(' ')
			}
			continue
		}
		// 关键修复:插 VS16 强制 emoji presentation,让宽度测量跨工具一致
		sb.WriteRune(0xFE0F)
		// 再处理视觉分隔
		if !isWhitespaceLike(next) {
			sb.WriteRune(' ')
		}
	}
	return sb.String()
}

// isWhitespaceLike 判断 rune 是否是已经能起字符边界作用的空白。
// 包括 ASCII 空白 + nbsp + ideographic space + 全角空格。
func isWhitespaceLike(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', 0x00A0, 0x3000:
		return true
	}
	return false
}

// ensureEmojiSpacingANSI 跟 ensureEmojiSpacing 同样目的(emoji 强制 emoji presentation
// 并补视觉分隔空格),但 ANSI-aware:用在 glamour 渲染**之后**兜底,会跳过 ANSI 转义序列。
func ensureEmojiSpacingANSI(s string) string {
	if s == "" {
		return s
	}
	runes := []rune(s)
	var sb strings.Builder
	sb.Grow(len(s) + 32)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		sb.WriteRune(r)

		// 遇 ESC:透传整段 ANSI 序列。覆盖最常见的 CSI(ESC [ ... final_byte)和
		// OSC(ESC ] ... BEL or ST)。final_byte 范围 0x40-0x7E 对 CSI。
		if r == 0x1B && i+1 < len(runes) {
			i++
			sb.WriteRune(runes[i])
			switch runes[i] {
			case '[':
				for i+1 < len(runes) {
					i++
					sb.WriteRune(runes[i])
					if runes[i] >= 0x40 && runes[i] <= 0x7E {
						break
					}
				}
			case ']':
				for i+1 < len(runes) {
					i++
					sb.WriteRune(runes[i])
					if runes[i] == 0x07 { // BEL
						break
					}
					if runes[i] == 0x1B && i+1 < len(runes) && runes[i+1] == '\\' {
						i++
						sb.WriteRune('\\')
						break
					}
				}
			}
			continue
		}

		if !isEmojiLike(r) {
			continue
		}
		if i+1 >= len(runes) {
			sb.WriteRune(0xFE0F)
			continue
		}
		next := runes[i+1]
		if next == 0x200D {
			continue
		}
		if next == 0xFE0F || next == 0xFE0E {
			// 已有 selector:保留,跳过后再判断是否要空格
			sb.WriteRune(next)
			i++
			if i+1 >= len(runes) {
				continue
			}
			after := runes[i+1]
			if after != 0x200D && !isWhitespaceLike(after) {
				sb.WriteRune(' ')
			}
			continue
		}
		// 关键修复:插 VS16 让宽度跨工具一致;再判断分隔空格
		sb.WriteRune(0xFE0F)
		if !isWhitespaceLike(next) {
			sb.WriteRune(' ')
		}
	}
	return sb.String()
}

// sumHistoryChars 把整段对话历史的 Content 字符数加起来,用作"已用上下文"的近似值。
// 不调 tokenizer 是为了零依赖 + 跨模型通用;按 ~3 chars/token 估算足够给用户一个量级感知。
func sumHistoryChars(h []agent.ChatMessage) int {
	total := 0
	for _, m := range h {
		total += len([]rune(m.Content))
		total += len([]rune(m.ReasoningContent))
		for _, p := range m.ContentParts {
			total += len([]rune(p.Text))
		}
	}
	return total
}

// formatTokenCount 把 token 计数格式化成紧凑字符串: 12 / 1.2K / 12.4K。
func formatTokenCount(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fK", float64(n)/1024.0)
}

// formatElapsed 把 duration 格式化成右栏能塞下的紧凑字符串。
// <60s: "4.2s"; 60-3600s: "2m13s"; ≥1h: "1h05m"。
func formatElapsed(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	if d < time.Hour {
		m := int(d / time.Minute)
		s := int(d/time.Second) % 60
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	h := int(d / time.Hour)
	m := int(d/time.Minute) % 60
	return fmt.Sprintf("%dh%02dm", h, m)
}

// abbreviatePath 把绝对路径压缩成 ~/... 形式以适配右栏窄宽。
// 超过 maxWidth 时从中间截断,保留头几段和最后一段。
func abbreviatePath(path string, maxWidth int) string {
	home := homeDir()
	if home != "" && strings.HasPrefix(path, home) {
		path = "~" + path[len(home):]
	}
	if maxWidth <= 0 || len(path) <= maxWidth {
		return path
	}
	// 从中间截断: 保留头部 + … + 尾部
	parts := strings.Split(path, "/")
	if len(parts) <= 2 {
		// 没法分段,从中间硬截
		half := (maxWidth - 1) / 2
		return path[:half] + "…" + path[len(path)-half:]
	}
	// 留最后一个目录名 + 尽量多的前段
	tail := "/" + parts[len(parts)-1]
	if len(tail) >= maxWidth-2 {
		return "…" + tail[len(tail)-(maxWidth-1):]
	}
	head := strings.Join(parts[:len(parts)-1], "/")
	budget := maxWidth - len(tail) - 1 // -1 给 "…"
	if budget < 1 {
		budget = 1
	}
	if len(head) > budget {
		head = head[:budget]
	}
	return head + "…" + tail
}

// homeDir 一次性查 $HOME,失败返回空串(走原路径)。
func homeDir() string {
	return os.Getenv("HOME")
}
