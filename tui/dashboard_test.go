package tui

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// glyphVS16 是一个会被 ensureEmojiSpacing 补上 VS16 的图标(⚙ U+2699 + U+FE0F)。
// VS16-honoring 终端渲染 2 cell,不认的渲染 1 cell —— 正好区分两套宽度口径。
const glyphVS16 = "⚙️"

func TestDetectWidthFunc_VS16Terminals(t *testing.T) {
	// 清掉可能干扰判定的终端 env,逐个用例显式设置。
	for _, k := range []string{"TERM_PROGRAM", "KITTY_WINDOW_ID", "ALACRITTY_LOG", "ALACRITTY_WINDOW_ID", "WT_SESSION"} {
		t.Setenv(k, "")
	}

	cases := []struct {
		name  string
		env   map[string]string
		wantW int // 期望该口径下 glyphVS16 的宽度
	}{
		{"vscode 不认 VS16", map[string]string{"TERM_PROGRAM": "vscode"}, 1},
		{"AppleTerminal 不认 VS16", map[string]string{"TERM_PROGRAM": "Apple_Terminal"}, 1},
		{"WezTerm 认 VS16", map[string]string{"TERM_PROGRAM": "WezTerm"}, 2},
		{"WindowsTerminal 认 VS16", map[string]string{"WT_SESSION": "some-guid"}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"TERM_PROGRAM", "WT_SESSION"} {
				t.Setenv(k, tc.env[k])
			}
			if got := detectWidthFunc()(glyphVS16); got != tc.wantW {
				t.Fatalf("detectWidthFunc()(%q) = %d, want %d", glyphVS16, got, tc.wantW)
			}
		})
	}
}

// isEmojiLike 必须放过纯文字 dingbat(不塞 VS16),否则它们在 VS16-honoring 终端会被高估 1 cell。
// 同时必须保留真 emoji —— ✔️/✖️/✡️ 有 emoji 变体序列,不能误伤。
func TestIsEmojiLike_NonEmojiExcluded(t *testing.T) {
	nonEmoji := []rune{0x2713, 0x2715, 0x2717, 0x2718, 0x2771} // ✓ ✕ ✗ ✘ ❱
	for _, r := range nonEmoji {
		if isEmojiLike(r) {
			t.Errorf("isEmojiLike(U+%04X %q) = true, want false (无 emoji 变体序列,不该塞 VS16)", r, string(r))
		}
	}
	realEmoji := []rune{0x2714, 0x2716, 0x2721, 0x2600, 0x2699} // ✔ ✖ ✡ ☀ ⚙
	for _, r := range realEmoji {
		if !isEmojiLike(r) {
			t.Errorf("isEmojiLike(U+%04X %q) = false, want true (真 emoji,需 VS16 对齐宽度)", r, string(r))
		}
	}
}

// 被排除的 dingbat 经 ensureEmojiSpacing 后不应被塞 VS16:输出宽度在两套口径下都得是 1,
// 这样无论终端认不认 VS16,divider 都不会因这些字符偏移。
func TestEnsureEmojiSpacing_NonEmojiStaysNarrow(t *testing.T) {
	for _, g := range []string{"✓", "✗", "❱"} {
		out := ensureEmojiSpacing(g)
		wc, gr := ansi.StringWidthWc(out), ansi.StringWidth(out)
		if wc != 1 || gr != 1 {
			t.Errorf("ensureEmojiSpacing(%q) = %q, 宽度 Wc=%d Graph=%d, 期望两者都为 1", g, out, wc, gr)
		}
	}
}
