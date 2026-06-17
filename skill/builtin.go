package skill

import (
	"embed"
	"os"
	"path/filepath"
	"sort"
)

//go:embed skills/*
var builtinFS embed.FS

// builtinVersion 仅保留供 -ldflags -X 注入(.goreleaser.yaml / install.sh / gitee-release.sh
// 仍会设置它)。内置 skill 已改为「从 embed 直接加载、不落盘」(见 BuiltinSkills),
// 不再用它做解压版本门控。
var builtinVersion = "dev"

// BuiltinVersion 返回构建时注入的内置版本号(本地构建为 "dev")。
func BuiltinVersion() string { return builtinVersion }

// 旧「抽取模型」曾把内置 skill 解压到 ~/.deepx/skills 并留下这两个标记文件;
// 现在只在一次性清理(CleanupExtractedBuiltins)里用作「曾经抽取过」的识别信号。
const (
	manifestFile = ".builtin_manifest"
	versionFile  = ".builtin_version"
)

// builtinSkillNames 是「需要从 ~/.deepx/skills 清理掉的内置 skill 目录名」的显式清单。
// 应包含**当前**所有内置(= skill/skills/ 下的目录)以及**历史上曾内置、后被移除**的名字
// (这样旧版解压的废弃内置也能被清掉)。新增/重命名内置时同步更新这里。
//
// 用显式常量而非运行时扫 embed:删除范围一目了然、可审计,且不受当前 embed 内容变化影响。
var builtinSkillNames = []string{
	"brainstorming",
	"creating-workflows",
	"dispatching-parallel-agents",
	"executing-plans",
	"finishing-a-development-branch",
	"frontend-design",
	"karpathy-guidelines",
	"openspec",
	"receiving-code-review",
	"requesting-code-review",
	"subagent-driven-development",
	"superpowers",
	"systematic-debugging",
	"test-driven-development",
	"using-git-worktrees",
	"verification-before-completion",
	"writing-plans",
	// 历史内置(已从 embed 移除,仍清理其旧解压副本):
	"using-superpowers",
}

// BuiltinNames 返回随二进制 embed 的内置 skill 名字集合。
// 供 UI / web 判断某 skill 是否内置(内置不可删,只随升级更新)。
func BuiltinNames() map[string]bool {
	out := map[string]bool{}
	entries, err := builtinFS.ReadDir("skills")
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() {
			out[e.Name()] = true
		}
	}
	return out
}

// BuiltinSkills 是内置 skill 的「专门加载函数」:从 embed 直接把所有内置 skill 读进内存,
// 完全不落盘。Loader 通过它把内置 skill 并入发现结果,从而:
//   - 内置 skill 与 ~/.deepx/skills 下的用户 skill 分离,互不污染、各自好管理;
//   - 升级换二进制即换内置,无需解压 / 版本门控 / 清理废弃副本。
func BuiltinSkills() []Skill {
	entries, err := builtinFS.ReadDir("skills")
	if err != nil {
		return nil
	}
	out := make([]Skill, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := "skills/" + e.Name() + "/SKILL.md"
		data, err := builtinFS.ReadFile(p)
		if err != nil {
			continue // 该内置目录没有 SKILL.md,跳过
		}
		s, err := parseSkillContent(data)
		if err != nil {
			continue
		}
		if s.Name == "" {
			s.Name = e.Name()
		}
		s.Scope = "builtin"
		s.Path = "embed:" + p
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// CleanupExtractedBuiltins 一次性迁移:删掉旧版「抽取模型」解压到 ~/.deepx/skills 的内置 skill 副本,
// 让内置不再污染全局用户 skill。
//
//   - 识别信号:旧版留下的 .builtin_manifest / .builtin_version 标记文件。两者都没有 → 直接返回
//     (说明从没抽取过,或已清理过;避免误删用户后来同名建的 skill)。
//   - 删除目标:**显式常量清单 builtinSkillNames**(只删名字落在这个内置集合里的目录;
//     用户自定义 skill 不同名,绝不触碰)。
//   - 删完移除两个标记文件 → 本函数对同一目录只实际生效一次,之后是无副作用的 no-op。
//     这也保证清理后用户仍能在 ~/.deepx/skills/ 放同名 skill 覆盖内置(不会再被删)。
func CleanupExtractedBuiltins(home string) {
	dest := filepath.Join(home, ".deepx", "skills")
	manifestPath := filepath.Join(dest, manifestFile)
	versionPath := filepath.Join(dest, versionFile)

	_, mErr := os.Stat(manifestPath)
	_, vErr := os.Stat(versionPath)
	if mErr != nil && vErr != nil {
		return // 无旧抽取痕迹,无需清理
	}

	for _, name := range builtinSkillNames {
		if name == "" || !safeName.MatchString(name) {
			continue // 防越界:只动合法名字的目录
		}
		_ = os.RemoveAll(filepath.Join(dest, name))
	}
	_ = os.Remove(manifestPath)
	_ = os.Remove(versionPath)
}
