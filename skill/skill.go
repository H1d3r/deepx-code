// Package skill 加载 SKILL.md 文件(兼容 Anthropic Agent Skills 极简子集)。
//
// SKILL.md 格式:
//
//	---
//	name: <短名,跟目录名一致即可>
//	description: <一句话说明做什么 + 何时用,LLM 据此决策是否加载>
//	---
//	<markdown 正文,加载后塞进 LLM 上下文>
//
// 发现路径(同名时 workspace 覆盖 global,组内按 slice 顺序后者覆盖前者):
//
//	workspace 级:<wd>/.deepx/skills
//	global 级:  ~/.agents/skills,~/.claude/skills,~/.deepx/skills
//
// global 多来源是为了兼容生态:已有 ~/.claude/skills / ~/.agents/skills 的用户能直接复用。
// 项目级只认 <wd>/.deepx/skills,避免扫到项目自家叫 skills/ 的目录或其他工具的配置。
package skill

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Skill 加载完成的 skill 实例。
type Skill struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`

	// 以下字段不来自 yaml,加载时由 loader 注入。
	Scope string `yaml:"-"` // "global" / "project"
	Path  string `yaml:"-"` // SKILL.md 绝对路径
	Body  string `yaml:"-"` // frontmatter 之后的 markdown 正文
}

// Metadata List() 返回的轻量信息(不读 Body,扫目录性能更好)。
type Metadata struct {
	Name        string
	Description string
	Scope       string
	Path        string
}

// Loader 多目录扫描器。
//
// WorkspaceDirs / GlobalDirs 都是有序 slice,扫描顺序 = slice 顺序,**同名后者覆盖前者**。
// 跨组 workspace 优先于 global(workspace 后扫,自然覆盖 global)。任一目录不存在不报错,
// Loader.List 静默跳过。
type Loader struct {
	WorkspaceDirs []string // 项目级,workspace 内的多个候选目录
	GlobalDirs    []string // 用户级,$HOME 下的多个候选目录

	// builtins 是从 embed 直接加载的内置 skill(不落盘),由 New 注入。
	// 优先级最低:磁盘上的同名 skill(用户自定义)会覆盖它。这样内置 skill 与全局
	// 用户 skill 分离、互不污染。
	builtins []Skill
}

// New 构造 loader。传 nil 表示该层无目录(只用单层也合法)。
// 内置 skill 通过 BuiltinSkills() 从 embed 加载并入,不依赖磁盘。
func New(workspaceDirs, globalDirs []string) *Loader {
	return &Loader{WorkspaceDirs: workspaceDirs, GlobalDirs: globalDirs, builtins: BuiltinSkills()}
}

// AllDirs 返回 Loader 持有的所有目录(按扫描顺序),给 /skills 空目录提示用。
func (l *Loader) AllDirs() []string {
	out := make([]string, 0, len(l.GlobalDirs)+len(l.WorkspaceDirs))
	out = append(out, l.GlobalDirs...)
	out = append(out, l.WorkspaceDirs...)
	return out
}

// List 扫所有目录,返回 skill 元数据(name 字典序)。
// 同名:workspace 覆盖 global;组内后扫覆盖先扫。
func (l *Loader) List() []Metadata {
	seen := make(map[string]Metadata)
	// 内置最低优先级,先放(会被磁盘同名覆盖)
	for _, s := range l.builtins {
		seen[s.Name] = Metadata{Name: s.Name, Description: s.Description, Scope: "builtin", Path: s.Path}
	}
	// 先 global(后被 workspace 覆盖)
	for _, dir := range l.GlobalDirs {
		scanDir(dir, "global", seen)
	}
	for _, dir := range l.WorkspaceDirs {
		scanDir(dir, "workspace", seen)
	}
	out := make([]Metadata, 0, len(seen))
	for _, m := range seen {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// scanDir 扫单个目录,匹配的 skill 写入 seen(覆盖式)。dir 不存在 / 不可读静默跳过。
func scanDir(dir, scope string, seen map[string]Metadata) {
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name(), "SKILL.md")
		meta, err := readMeta(p)
		if err != nil {
			continue // SKILL.md 不存在 / yaml 坏 → 静默跳过
		}
		if meta.Name == "" {
			meta.Name = e.Name()
		}
		meta.Scope = scope
		meta.Path = p
		seen[meta.Name] = meta
	}
}

// Load 按名加载完整 skill。workspace 优先,找不到再查 global,都没有则 error。
// 同层内按 slice 顺序找,先命中即返回(不再继续覆盖)。
func (l *Loader) Load(name string) (*Skill, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("skill name 不能为空")
	}
	var triedPaths []string
	for _, dir := range l.WorkspaceDirs {
		p := filepath.Join(dir, name, "SKILL.md")
		triedPaths = append(triedPaths, p)
		if _, err := os.Stat(p); err == nil {
			return readFull(p, "workspace")
		}
	}
	for _, dir := range l.GlobalDirs {
		p := filepath.Join(dir, name, "SKILL.md")
		triedPaths = append(triedPaths, p)
		if _, err := os.Stat(p); err == nil {
			return readFull(p, "global")
		}
	}
	// 磁盘没有 → 回退内置(从 embed 加载,优先级最低)
	for i := range l.builtins {
		if l.builtins[i].Name == name {
			s := l.builtins[i]
			return &s, nil
		}
	}
	return nil, fmt.Errorf("skill %q 未找到 (tried: %s, builtins)", name, strings.Join(triedPaths, ", "))
}

// readMeta 只读 frontmatter,不读 body —— List 场景下省 I/O。
func readMeta(path string) (Metadata, error) {
	f, err := os.Open(path)
	if err != nil {
		return Metadata{}, err
	}
	defer f.Close()
	fm, _, err := splitFrontmatter(f)
	if err != nil {
		return Metadata{}, err
	}
	var m Metadata
	if fm != "" {
		if err := yaml.Unmarshal([]byte(fm), &m); err != nil {
			return Metadata{}, fmt.Errorf("解析 frontmatter: %w", err)
		}
	}
	return m, nil
}

// readFull 读完整 SKILL.md(frontmatter + body)。
func readFull(path, scope string) (*Skill, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := parseSkillContent(data)
	if err != nil {
		return nil, err
	}
	if s.Name == "" {
		s.Name = filepath.Base(filepath.Dir(path))
	}
	s.Scope = scope
	s.Path = path
	return s, nil
}

// parseSkillContent 解析一段 SKILL.md 内容(frontmatter + body)成 Skill。
// 不设 Name 兜底 / Scope / Path —— 由调用方按来源(磁盘 / embed)补齐。
// 供磁盘读取(readFull)和内置加载(BuiltinSkills)共用,统一解析口径。
func parseSkillContent(data []byte) (*Skill, error) {
	fm, body, err := splitFrontmatter(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	var s Skill
	if fm != "" {
		if err := yaml.Unmarshal([]byte(fm), &s); err != nil {
			return nil, fmt.Errorf("解析 frontmatter: %w", err)
		}
	}
	s.Body = body
	return &s, nil
}

// splitFrontmatter 从文件中分出 yaml frontmatter 和 markdown body。
// 约定格式: "---\n<yaml>\n---\n<body>"。
// 没有 frontmatter(文件首行非 "---") → 全文当 body,fm 返回空串。
func splitFrontmatter(r io.Reader) (fm, body string, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // 1MB 单行上限,够任何合理 SKILL.md
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return "", "", err
	}
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", strings.Join(lines, "\n"), nil
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		// 没有闭合 "---" → 当作没有 frontmatter
		return "", strings.Join(lines, "\n"), nil
	}
	fm = strings.Join(lines[1:end], "\n")
	body = strings.Join(lines[end+1:], "\n")
	return fm, body, nil
}
