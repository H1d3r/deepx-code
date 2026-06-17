package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Script 是一个加载完成的 workflow 脚本。
type Script struct {
	Name        string         // = 文件名去掉 .js(kebab-case)
	Description string         // 从 meta.description 轻量扫描得到(可空)
	Path        string         // .js 绝对路径
	Scope       string         // "project" / "global"
	Source      string         // 脚本源码
	Phases      []string       // meta.phases 的 title 列表(声明的阶段骨架;UI 用它在运行前就展示全部阶段)
	Steps       []ExpectedStep // 静态解析出的预期步骤(各阶段下的 agent;UI 运行前预先列出,尽力而为)
}

func (s *Script) fileName() string {
	if s == nil || s.Name == "" {
		return "workflow.mjs"
	}
	return s.Name + ".mjs"
}

// workflowExts 是 workflow 脚本的合法后缀。对齐 Claude Code(用 .mjs,ES module),同时兼容 .js。
// 顺序即查找优先级(Load 时先试 .mjs)。
var workflowExts = []string{".mjs", ".js"}

// workflowBaseName 若 fname 是 workflow 文件,返回去后缀的名字 + true。
func workflowBaseName(fname string) (string, bool) {
	for _, ext := range workflowExts {
		if base, ok := strings.CutSuffix(fname, ext); ok {
			return base, true
		}
	}
	return "", false
}

// Metadata 是 List() 返回的轻量信息(不读完整源码语义,只扫文件)。
type Metadata struct {
	Name        string
	Description string
	Scope       string
	Path        string
}

// Loader 多目录扫描器,语义对齐 skill 包:project 覆盖 global,组内后者覆盖前者。
// 约定每个 .js 文件就是一个 workflow,文件名(去 .js)即 workflow 名。
type Loader struct {
	ProjectDirs []string // 项目级,如 <wd>/.deepx/workflows
	GlobalDirs  []string // 用户级,如 ~/.deepx/workflows
}

// New 构造 loader;传 nil 表示该层无目录。
func New(projectDirs, globalDirs []string) *Loader {
	return &Loader{ProjectDirs: projectDirs, GlobalDirs: globalDirs}
}

// DefaultDirs 给定 workspace 和 home,返回 workflow 发现目录。
// 同时扫 .claude/workflows(兼容 Claude Code 的 workflow,拷进来即可跑)和 .deepx/workflows
// (deepx 自己 create/save 的去处)。组内后者覆盖前者,故 .deepx 同名覆盖 .claude。
func DefaultDirs(workspace, home string) (projectDirs, globalDirs []string) {
	if workspace != "" {
		projectDirs = []string{
			filepath.Join(workspace, ".claude", "workflows"),
			filepath.Join(workspace, ".deepx", "workflows"),
		}
	}
	if home != "" {
		globalDirs = []string{
			filepath.Join(home, ".claude", "workflows"),
			filepath.Join(home, ".deepx", "workflows"),
		}
	}
	return projectDirs, globalDirs
}

// AllDirs 返回所有目录(扫描顺序),给「空目录」提示用。
func (l *Loader) AllDirs() []string {
	out := make([]string, 0, len(l.GlobalDirs)+len(l.ProjectDirs))
	out = append(out, l.GlobalDirs...)
	out = append(out, l.ProjectDirs...)
	return out
}

// List 扫所有目录,返回 workflow 元数据(name 字典序);同名 project 覆盖 global。
func (l *Loader) List() []Metadata {
	seen := map[string]Metadata{}
	for _, d := range l.GlobalDirs {
		scanDir(d, "global", seen)
	}
	for _, d := range l.ProjectDirs {
		scanDir(d, "project", seen)
	}
	out := make([]Metadata, 0, len(seen))
	for _, m := range seen {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Load 按名加载完整脚本:project 优先,再 global,都没有则 error。
func (l *Loader) Load(name string) (*Script, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("workflow 名不能为空")
	}
	if !safeName.MatchString(name) {
		return nil, fmt.Errorf("非法 workflow 名 %q(仅允许字母数字 . _ -)", name)
	}
	var tried []string
	for _, group := range [][2]any{{l.ProjectDirs, "project"}, {l.GlobalDirs, "global"}} {
		dirs, _ := group[0].([]string)
		scope, _ := group[1].(string)
		for _, d := range dirs {
			for _, ext := range workflowExts {
				p := filepath.Join(d, name+ext)
				tried = append(tried, p)
				if data, err := os.ReadFile(p); err == nil {
					return &Script{
						Name:        name,
						Description: scanDescription(string(data)),
						Path:        p,
						Scope:       scope,
						Source:      string(data),
						Phases:      parsePhaseTitles(string(data)),
						Steps:       parseExpectedSteps(string(data)),
					}, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("workflow %q 未找到 (tried: %s)", name, strings.Join(tried, ", "))
}

// safeName 校验 workflow 名:字母数字 + . _ -,1~64 长(防越界、防奇怪文件名)。
var safeName = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)

// ValidName 校验一个 workflow 名是否合法(创建时校验 saveAs 用)。
func ValidName(name string) bool { return safeName.MatchString(name) }

// Save 把脚本源码写到 dir/<name>.mjs(创建 dir;对齐 Claude Code 的 .mjs)。
// name 必须合法;overwrite=false 时已存在(任一后缀)则报错。
func Save(dir, name, source string, overwrite bool) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("非法 workflow 名 %q(仅允许字母数字 . _ -,kebab-case)", name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建目录失败: %w", err)
	}
	if !overwrite {
		for _, ext := range workflowExts { // 任一后缀已存在都算冲突
			if _, err := os.Stat(filepath.Join(dir, name+ext)); err == nil {
				return "", fmt.Errorf("workflow %q 已存在(换个名字或先删)", name)
			}
		}
	}
	p := filepath.Join(dir, name+".mjs")
	if err := os.WriteFile(p, []byte(source), 0o644); err != nil {
		return "", fmt.Errorf("写入失败: %w", err)
	}
	return p, nil
}

// scanDir 扫单个目录,把每个 *.mjs / *.js 当一个 workflow 收进 seen(覆盖式)。
func scanDir(dir, scope string, seen map[string]Metadata) {
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name, ok := workflowBaseName(e.Name())
		if !ok || !safeName.MatchString(name) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		desc := ""
		if data, err := os.ReadFile(p); err == nil {
			desc = scanDescription(string(data))
		}
		seen[name] = Metadata{Name: name, Description: desc, Scope: scope, Path: p}
	}
}

// descRe 从脚本里轻量抓 meta.description 的字符串字面量,纯展示用。
// 不跑 JS、不求严谨:抓不到就空,列表照样能用文件名。
var descRe = regexp.MustCompile(`description\s*:\s*["'` + "`" + `]([^"'` + "`" + `]*)`)

func scanDescription(src string) string {
	if m := descRe.FindStringSubmatch(src); len(m) == 2 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// phasesBlockRe 抓 meta 里 `phases: [ ... ]` 块;phaseTitleRe 从块内按序抓每个 `title: "..."`。
// 轻量扫描、不跑 JS(meta 要求是顶部纯字面量,抓得到就用,抓不到就空)。仅取 title 作阶段骨架。
var (
	phasesBlockRe = regexp.MustCompile(`(?s)phases\s*:\s*\[(.*?)\]`)
	phaseTitleRe  = regexp.MustCompile("title\\s*:\\s*[\"'`]([^\"'`]+)")
)

// parsePhaseTitles 从脚本源码里提取 meta.phases 的 title 列表(声明顺序)。
func parsePhaseTitles(src string) []string {
	block := phasesBlockRe.FindStringSubmatch(src)
	if len(block) < 2 {
		return nil
	}
	var out []string
	for _, m := range phaseTitleRe.FindAllStringSubmatch(block[1], -1) {
		if t := strings.TrimSpace(m[1]); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// ExpectedStep 是静态解析出的一个预期步骤:一个带 label 的 agent() 调用及其所属阶段。
// 仅尽力而为(声明式脚本可解析;循环/动态 label/带 schema 的会漏,漏了就运行时再出现,不影响正确性)。
type ExpectedStep struct {
	Phase string
	Label string
	Model string
}

var (
	phaseCallRe = regexp.MustCompile("phase\\s*\\(\\s*[\"'`]([^\"'`]+)")
	// 含 label 的 opts 对象(无嵌套 `{}`,故带 schema 的 opts 会漏——可接受)。
	agentLabelOptsRe = regexp.MustCompile("\\{[^{}]*\\blabel\\s*:\\s*[\"'`]([^\"'`]+)[\"'`][^{}]*\\}")
	modelKeyRe       = regexp.MustCompile("\\bmodel\\s*:\\s*[\"'`]([^\"'`]+)")
)

// parseExpectedSteps 按源码位置把 phase() 调用与含 label 的 agent opts 归并排序,
// 每个 opts 归到它前面最近的 phase()。供 UI 在运行前把各阶段下的步骤也预先列出。
func parseExpectedSteps(src string) []ExpectedStep {
	type mark struct {
		pos          int
		phase        string
		label, model string
		isPhase      bool
	}
	var marks []mark
	for _, m := range phaseCallRe.FindAllStringSubmatchIndex(src, -1) {
		marks = append(marks, mark{pos: m[0], phase: src[m[2]:m[3]], isPhase: true})
	}
	for _, m := range agentLabelOptsRe.FindAllStringSubmatchIndex(src, -1) {
		model := ""
		if mm := modelKeyRe.FindStringSubmatch(src[m[0]:m[1]]); len(mm) == 2 {
			model = mm[1]
		}
		marks = append(marks, mark{pos: m[0], label: src[m[2]:m[3]], model: model})
	}
	sort.Slice(marks, func(i, j int) bool { return marks[i].pos < marks[j].pos })
	var out []ExpectedStep
	cur := ""
	for _, mk := range marks {
		if mk.isPhase {
			cur = mk.phase
			continue
		}
		out = append(out, ExpectedStep{Phase: cur, Label: mk.label, Model: mk.model})
	}
	return out
}
