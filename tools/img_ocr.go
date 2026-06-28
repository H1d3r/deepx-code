package tools

import (
	"deepx/ocr"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ImgOCR 工具入口:对单张图片路径做 OCR,把识别出的文本返回给模型。
// 用于不支持视觉输入的 LLM (例如 DeepSeek),由模型显式调用获取图片文字。
// 内部走 PP-OCRv5_mobile det+rec ONNX,首次调用会下载 ~37MB 资产到 ~/.deepx/ocr/。
//
// 注意:**OCR 不再删图**。粘贴的图片在整个会话期间都可能被反复使用(支持视觉的模型直接发
// base64、不支持的走 OCR;模型还可能中途切换),删早了会让后续轮次 / 模型切换 / 重发历史读不到图。
// 粘贴缓存改由启动时的 SweepPasteCache 按时效统一清理(见下)。
func ImgOCR(args map[string]any) ToolResult {
	path, _ := args["path"].(string)
	if path == "" {
		return ToolResult{Output: "错误: path 参数为空", Success: false}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ToolResult{Output: fmt.Sprintf("路径错误: %v", err), Success: false}
	}

	engine := ocr.Global()
	// 第一次调用会触发下载;若已下载好就是个轻量 stat。
	if !engine.IsReady() {
		if err := engine.EnsureAssets(nil); err != nil {
			return ToolResult{
				Output:  fmt.Sprintf("OCR 资产下载失败: %v\n缓存目录: %s", err, engine.CacheDir()),
				Success: false,
			}
		}
	}

	text, err := engine.RecognizeText(abs)
	if err != nil {
		return ToolResult{Output: fmt.Sprintf("OCR 失败: %v", err), Success: false}
	}
	if strings.TrimSpace(text) == "" {
		// 没识别到文字必须算「失败」:否则非视觉模型对一张读不出字的图反复 OCR,每轮都 Success=true
		// 会把 maxNoProgressRounds 卡死断路器一直归零 → 无限重试(issue #146)。返回失败让断路器能介入,
		// 并明确叫模型别再对同一张图 OCR。
		return ToolResult{
			Output:  "(图片中未识别到文字)。请勿对同一张图重复 OCR;如需图中内容,请让用户改用支持视觉的模型,或直接询问用户图里写了什么。",
			Success: false,
		}
	}
	return ToolResult{Output: text, Success: true}
}

// PasteCacheDir 返回粘贴图片的缓存目录 ~/.deepx/ocr/cache;home 取不到则返回空串。
// 单一来源:落盘(saveAttachedImage)、清理(SweepPasteCache)、视觉模型 OCR 拦截判定都用它。
func PasteCacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".deepx", "ocr", "cache")
}

// SweepPasteCache 清理 ~/.deepx/ocr/cache 下修改时间超过 maxAge 的粘贴图片。
// 在程序启动时调用(非阻塞):把图片生命周期跟 OCR 调用解耦——会话期间随用随有,
// 旧的统一按时效清掉,不堆积。删除失败静默(文件可能正被占用/已被清)。
func SweepPasteCache(maxAge time.Duration) {
	cacheDir := PasteCacheDir()
	if cacheDir == "" {
		return
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return // 目录不存在(从没粘过图)或读不了 → 无事可清
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(cacheDir, e.Name()))
		}
	}
}
