package codegraph

import (
	"bytes"
	"sort"
	"strings"
	"sync"
	"time"

	gts "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// HeritageLangs 返回支持继承/实现边抽取的语言(Go 精确 + tsHeritage 覆盖的语法级)。
// 供工具空结果提示用,避免模型把"未抽取"误判成"无继承关系"。
func HeritageLangs() []string {
	out := []string{"go"}
	for k := range tsHeritage {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// parseTimeout 是单文件 tree-sitter 解析的硬上限。GLR 对个别病态输入会爆炸且软超时不可靠,
// 用子 goroutine + select 到点直接放弃该文件,保证索引不被一个文件拖死。
const parseTimeout = 1 * time.Second

func init() { Register(newTreeSitterParser()) }

// tsAllowed 是交给 tree-sitter 的语言白名单(gotreesitter 语言名)。只激活"主流编程语言 +
// 解析靠谱 + 有符号价值"的那批(对齐 codegraph 清单);Go 不在内(stdlib 精确),shell/css/
// json/md 等不在内(grammar 病态或无代码符号),它们走 Grep。
//
// 注:必须用 grammars 包的 Language()(它接好了各语言的外部扫描器,如 Python 缩进);裸
// LoadLanguage(blob) 接不上扫描器,会让缩进类语言的节点范围截断、调用/容器推断失效。
var tsAllowed = map[string]bool{
	"typescript": true, "tsx": true, "javascript": true,
	"python": true, "java": true, "rust": true,
	"c": true, "cpp": true, "c_sharp": true,
	"ruby": true, "php": true, "kotlin": true, "swift": true,
	"scala": true, "dart": true, "vue": true, "svelte": true,
	// 对齐 GitHub codegraph 清单补充:
	"liquid": true, "pascal": true, // Pascal/Delphi(pascal grammar 覆盖 Delphi)
	"lua": true, "luau": true,
}

// tsHeritage 是各语言的"继承/实现"tree-sitter 查询(captures:@type 类名、@super 父类/extends、
// @impl 实现的接口)。只为节点结构已验证的主流 OOP 语言提供;没有的语言不产出继承边。
// 这是语法级(名字解析,跟 Go 之外其它边一致),不做跨文件类型解析。
var tsHeritage = map[string]string{
	"typescript": tsTSHeritage,
	"tsx":        tsTSHeritage,
	"javascript": `(class_declaration name: (identifier) @type (class_heritage (identifier) @super))`,
	"java": `(class_declaration name: (identifier) @type superclass: (superclass (type_identifier) @super)? interfaces: (super_interfaces (type_list (type_identifier) @impl))?)
(interface_declaration name: (identifier) @type (extends_interfaces (type_list (type_identifier) @super)))`,
	"python": `(class_definition name: (identifier) @type superclasses: (argument_list [(identifier) @super (attribute) @super]))`,
	// C++ 无接口概念,基类(public/private/protected)全当 extends;class 和 struct 都可继承。
	"cpp": `(class_specifier name: (type_identifier) @type (base_class_clause (type_identifier) @super))
(struct_specifier name: (type_identifier) @type (base_class_clause (type_identifier) @super))`,
	// C#:base_list 混了基类和接口,语法上分不开,全当 extends。
	"c_sharp": `(class_declaration name: (identifier) @type (base_list (identifier) @super))`,
	// Ruby:class X < Super(单继承);include 模块是方法调用,不在此抽。
	"ruby": `(class name: (constant) @type (superclass (constant) @super))`,
	// PHP / Dart:能分 extends 与 implements。
	"php":  `(class_declaration name: (name) @type (base_clause (name) @super)? (class_interface_clause (name) @impl)?)`,
	"dart": `(class_definition name: (identifier) @type (superclass (type_identifier) @super)? (interfaces (type_identifier) @impl)?)`,
	// Kotlin / Swift / Scala:语法不区分继承与实现,全当 extends。
	"kotlin": `(class_declaration (type_identifier) @type (delegation_specifier (user_type (type_identifier) @super)))
(class_declaration (type_identifier) @type (delegation_specifier (constructor_invocation (user_type (type_identifier) @super))))`,
	"swift": `(class_declaration (type_identifier) @type (inheritance_specifier (user_type (type_identifier) @super)))`,
	"scala": `(class_definition (identifier) @type (extends_clause (type_identifier) @super))`,
	// Rust:无类继承,只有 impl Trait for Type → Type 实现 Trait(inherent impl 无 trait 字段,不产边)。
	"rust": `(impl_item trait: (type_identifier) @impl type: (type_identifier) @type)`,
}

const tsTSHeritage = `(class_declaration name: (type_identifier) @type (class_heritage (extends_clause (identifier) @super)? (implements_clause (type_identifier) @impl)?))
(interface_declaration name: (type_identifier) @type (extends_type_clause (type_identifier) @super))`

// tsParser 用 gotreesitter 兜底白名单内的语言(Go 以外的主流编程语言)。
// 精度说明(跟 Go 语法层一致):符号/调用是名字级,方法/容器靠 AST range 包含关系推断,
// 同名会合并;refs 只覆盖 tags 捕获的引用,不是每个标识符。
type tsParser struct {
	exts    []string
	mu      sync.Mutex
	taggers map[string]*gts.Tagger // 按语言名缓存(query 编译一次;构建顺序调用)
	queries map[string]*gts.Query  // 继承查询缓存(nil 表示编译失败,不再重试)
}

func newTreeSitterParser() *tsParser {
	p := &tsParser{taggers: map[string]*gts.Tagger{}, queries: map[string]*gts.Query{}}
	seen := map[string]bool{}
	for _, e := range grammars.AllLanguages() {
		if !tsAllowed[e.Name] {
			continue
		}
		for _, ext := range e.Extensions {
			ext = strings.ToLower(ext)
			if ext == "" || ext == ".go" || seen[ext] {
				continue
			}
			seen[ext] = true
			p.exts = append(p.exts, ext)
		}
	}
	return p
}

func (p *tsParser) Lang() string   { return "tree-sitter" }
func (p *tsParser) Exts() []string { return p.exts }

func (p *tsParser) tagger(name string, lang *gts.Language, entry *grammars.LangEntry) (*gts.Tagger, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t, ok := p.taggers[name]; ok {
		return t, nil
	}
	t, err := gts.NewTagger(lang, grammars.ResolveTagsQuery(*entry))
	if err != nil {
		return nil, err
	}
	p.taggers[name] = t
	return t, nil
}

func (p *tsParser) Parse(relPath string, src []byte) (ParseResult, error) {
	entry := grammars.DetectLanguage(relPath)
	if entry == nil || !tsAllowed[entry.Name] {
		return ParseResult{}, nil // 不识别 / 不在白名单 → 跳过(走 Grep)
	}
	lang := entry.Language()
	if lang == nil {
		return ParseResult{}, nil
	}
	tg, err := p.tagger(entry.Name, lang, entry)
	if err != nil {
		return ParseResult{}, nil
	}

	// 解析硬超时:子 goroutine + select,前台绝不被单文件阻塞超过 parseTimeout。
	parser := gts.NewParser(lang)
	parser.SetTimeoutMicros(uint64(parseTimeout.Microseconds()))
	type parseOut struct {
		tree *gts.Tree
		err  error
	}
	ch := make(chan parseOut, 1)
	go func() { tr, e := parser.Parse(src); ch <- parseOut{tr, e} }()
	var tree *gts.Tree
	select {
	case r := <-ch:
		if r.err != nil || r.tree == nil {
			return ParseResult{}, nil
		}
		tree = r.tree
	case <-time.After(parseTimeout):
		return ParseResult{}, nil // 硬超时 → 跳过该文件
	}

	tags := tg.TagTree(tree)

	// 先收集定义的 range,用于推断容器(嵌套)和调用方(enclosing)。
	type defSpan struct {
		name               string
		nameStart          uint32
		fullStart, fullEnd uint32
	}
	var defs []defSpan
	for _, t := range tags {
		if strings.HasPrefix(t.Kind, "definition.") {
			defs = append(defs, defSpan{t.Name, t.NameRange.StartByte, t.Range.StartByte, t.Range.EndByte})
		}
	}
	enclosing := func(pos uint32, skipStart uint32) string {
		best := ""
		var bestSpan uint32 = ^uint32(0)
		for _, d := range defs {
			if d.nameStart == skipStart {
				continue
			}
			if d.fullStart <= pos && pos < d.fullEnd {
				if span := d.fullEnd - d.fullStart; span < bestSpan {
					bestSpan = span
					best = d.name
				}
			}
		}
		return best
	}

	var res ParseResult
	for _, t := range tags {
		row := int(t.NameRange.StartPoint.Row) + 1
		switch {
		case strings.HasPrefix(t.Kind, "definition."):
			res.Symbols = append(res.Symbols, Symbol{
				Name:      t.Name,
				Kind:      tsKind(t.Kind),
				File:      relPath,
				Line:      row,
				EndLine:   int(t.Range.EndPoint.Row) + 1,
				Container: enclosing(t.NameRange.StartByte, t.NameRange.StartByte),
				Lang:      entry.Name,
				Exported:  isExportedName(t.Name),
			})
		case strings.HasPrefix(t.Kind, "reference."):
			res.Refs = append(res.Refs, Ref{Name: t.Name, File: relPath, Line: row})
			if t.Kind == "reference.call" {
				if from := enclosing(t.NameRange.StartByte, 0); from != "" {
					res.Edges = append(res.Edges, Edge{From: from, To: t.Name, Kind: EdgeCall, File: relPath, Line: row})
				}
			}
		}
	}

	// 继承/实现边(语法级,仅 tsHeritage 覆盖的语言)。
	res.Edges = append(res.Edges, p.heritageEdges(entry.Name, relPath, lang, tree, src)...)

	// import 边(语言中立):From=文件,To=import path。
	for _, im := range gts.ExtractImports(tree) {
		path := im.Path
		if path == "" {
			path = im.Name
		}
		if path == "" {
			continue
		}
		res.Edges = append(res.Edges, Edge{From: relPath, To: path, Kind: EdgeImport, File: relPath, Line: byteLine(src, im.StartByte)})
	}

	return res, nil
}

// query 按语言名缓存编译好的继承查询(NewQuery 构造后并发读安全)。编译失败缓存 nil。
func (p *tsParser) query(name string, lang *gts.Language) *gts.Query {
	p.mu.Lock()
	defer p.mu.Unlock()
	if q, ok := p.queries[name]; ok {
		return q
	}
	q, err := gts.NewQuery(tsHeritage[name], lang)
	if err != nil {
		q = nil
	}
	p.queries[name] = q
	return q
}

// heritageEdges 跑继承查询,产出 extends(@super)/ implements(@impl)边,From=类名(@type)。
func (p *tsParser) heritageEdges(name, relPath string, lang *gts.Language, tree *gts.Tree, src []byte) []Edge {
	if _, ok := tsHeritage[name]; !ok {
		return nil
	}
	q := p.query(name, lang)
	if q == nil {
		return nil
	}
	var out []Edge
	seen := map[string]bool{} // 去重:多接口会产生多个 match,每个都带同一个 @super,避免重复发边
	for _, m := range q.Execute(tree) {
		var typ string
		var line int
		for _, c := range m.Captures {
			if c.Name == "type" {
				typ = c.Text(src)
				line = int(c.Node.StartPoint().Row) + 1
			}
		}
		if typ == "" {
			continue
		}
		for _, c := range m.Captures {
			var kind EdgeKind
			switch c.Name {
			case "super":
				kind = EdgeExtends
			case "impl":
				kind = EdgeImplements
			default:
				continue
			}
			to := c.Text(src)
			key := typ + "\x00" + to + "\x00" + string(kind)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, Edge{From: typ, To: to, Kind: kind, File: relPath, Line: line})
		}
	}
	return out
}

// tsKind 把 tree-sitter tags 的 "definition.X" 映射到我们的 Kind。
func tsKind(tagKind string) Kind {
	switch strings.TrimPrefix(tagKind, "definition.") {
	case "function":
		return KindFunc
	case "method":
		return KindMethod
	case "constant":
		return KindConst
	case "field", "property", "member":
		return KindField
	case "variable":
		return KindVar
	case "class", "interface", "struct", "enum", "type", "trait", "module", "object", "protocol", "union", "macro":
		return KindType
	}
	return KindType // 其它 definition.* 兜底当类型/声明
}

// isExportedName 跨语言粗判"是否对外可见":不以下划线开头。仅供展示近似。
func isExportedName(name string) bool {
	return name != "" && !strings.HasPrefix(name, "_")
}

// byteLine 把字节偏移换成 1-based 行号。
func byteLine(src []byte, off uint32) int {
	if int(off) > len(src) {
		off = uint32(len(src))
	}
	return bytes.Count(src[:off], []byte("\n")) + 1
}
