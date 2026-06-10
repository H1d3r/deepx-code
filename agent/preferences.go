package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// prefsCache:会话内冻结的偏好快照。BuildSystemPrompt 每轮读它(不再每轮读盘),
// 只有 RefreshPreferences 才更新 —— 由 TUI 在「启动」和「触发压缩」时调用。
// 这样会话中途写入的 AGENTS.md 不会立刻改变前缀(保持缓存稳定),下次压缩/重启才生效。
var prefsCache atomic.Value // 存 string

// RefreshPreferences 重新读盘并刷新缓存。在启动、新建/切换会话、压缩时调用。
func RefreshPreferences(workspace string) {
	prefsCache.Store(loadPreferences(workspace))
}

// currentPrefs 返回当前冻结的偏好快照(未刷新过则为空)。
func currentPrefs() string {
	if v := prefsCache.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// 持久偏好 / 项目约定(类似 CLAUDE.md):两级 AGENTS.md
//   - 全局:~/.deepx/AGENTS.md       —— 跨项目的个人工作习惯
//   - 项目:<workspace>/AGENTS.md    —— 仅本仓库的约定
// 由 Remember 工具写入,BuildSystemPrompt 每次构建 system prompt 时读取并注入。

// loadPreferences 读取全局 + 项目两级偏好,拼成注入用的文本(空则返回空串)。
func loadPreferences(workspace string) string {
	var b strings.Builder
	if home, err := os.UserHomeDir(); err == nil {
		if c := readTrim(filepath.Join(home, ".deepx", "AGENTS.md")); c != "" {
			b.WriteString("## 全局偏好(适用于所有项目)\n")
			b.WriteString(c)
			b.WriteString("\n\n")
		}
	}
	if workspace != "" {
		if c := readTrim(filepath.Join(workspace, "AGENTS.md")); c != "" {
			b.WriteString("## 本项目约定\n")
			b.WriteString(c)
			b.WriteString("\n")
		}
	}
	return strings.TrimSpace(b.String())
}

// saveMemory 把一条偏好写入对应级别的 AGENTS.md(去重:同内容已存在则跳过)。返回写入的文件路径。
func saveMemory(scope, content, workspace string) (string, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return "", fmt.Errorf("content 为空")
	}
	path, header, err := prefPath(scope, workspace)
	if err != nil {
		return "", err
	}
	existing := readTrim(path)
	// 去重:逐行比对(剥掉 "- " 前缀)
	for _, ln := range strings.Split(existing, "\n") {
		if strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ln), "- ")) == content {
			return path, nil // 已记过,幂等
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	line := "- " + content + "\n"
	if existing == "" {
		return path, os.WriteFile(path, []byte(header+line), 0o644)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	_, err = f.WriteString(line)
	return path, err
}

// prefPath 按 scope 返回目标文件路径 + 新建时的文件头。
func prefPath(scope, workspace string) (path, header string, err error) {
	if scope == "project" {
		if strings.TrimSpace(workspace) == "" {
			return "", "", fmt.Errorf("当前无工作区,无法记项目偏好")
		}
		return filepath.Join(workspace, "AGENTS.md"), "# 项目约定(deepx 自动维护,启动时注入)\n\n", nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(home, ".deepx", "AGENTS.md"), "# 用户偏好(deepx 全局,自动维护,启动时注入)\n\n", nil
}

func readTrim(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// parseRememberArgs 解析 Remember 工具参数。scope 非法时兜底为 project(影响面小、好回收)。
func parseRememberArgs(raw string) (scope, content string, err error) {
	var w struct {
		Scope   string `json:"scope"`
		Content string `json:"content"`
	}
	if e := json.Unmarshal([]byte(raw), &w); e != nil {
		return "", "", fmt.Errorf("Remember 参数解析失败: %w", e)
	}
	scope = strings.TrimSpace(w.Scope)
	if scope != "global" && scope != "project" {
		scope = "project"
	}
	content = strings.TrimSpace(w.Content)
	if content == "" {
		return "", "", fmt.Errorf("content 不能为空")
	}
	return scope, content, nil
}
