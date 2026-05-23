package codegraph

import (
	"path/filepath"
	"strings"
)

// Parser 是单语言解析器抽象。多语言扩展点:新增一门语言 = 实现这个接口 + 在 init() 里 Register。
// Parse 拿到相对路径与源码字节,吐出该文件里的定义(符号)与引用。解析失败应返回 error,
// 由 index 决定跳过该文件(不让一个坏文件拖垮整次索引)。
type Parser interface {
	Lang() string   // 语言名,如 "go" / "typescript" / "python"
	Exts() []string // 负责的文件扩展名(含点,小写),如 [".go"]
	Parse(relPath string, src []byte) (ParseResult, error)
}

// parsers 是扩展名 → Parser 的注册表。运行时只在 init() 阶段写入,之后只读,无竞态。
var parsers = map[string]Parser{}

// Register 把 parser 按其负责的扩展名登记。各语言文件在 init() 里调用。
func Register(p Parser) {
	for _, ext := range p.Exts() {
		parsers[strings.ToLower(ext)] = p
	}
}

// parserFor 按文件扩展名取对应 parser,没有则返回 nil(该文件被跳过)。
func parserFor(path string) Parser {
	return parsers[strings.ToLower(filepath.Ext(path))]
}

// SupportedExts 返回当前已注册的全部扩展名(给诊断 / 工具描述用)。
func SupportedExts() []string {
	out := make([]string, 0, len(parsers))
	for ext := range parsers {
		out = append(out, ext)
	}
	return out
}
