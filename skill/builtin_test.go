package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 内置 skill 必须能从 embed 直接加载、被 Loader 发现,且 frontmatter / 正文完整(无需落盘)。
func TestBuiltinSkillsFromEmbed(t *testing.T) {
	// 不给任何磁盘目录:内置应仅靠 embed 出现。
	loader := New(nil, nil)
	got := map[string]Metadata{}
	for _, m := range loader.List() {
		got[m.Name] = m
	}

	want := []string{
		"brainstorming",
		"verification-before-completion",
		"creating-workflows",
	}
	for _, name := range want {
		m, ok := got[name]
		if !ok {
			t.Errorf("内置 skill %q 未被发现", name)
			continue
		}
		if m.Scope != "builtin" {
			t.Errorf("skill %q scope = %q, 应为 builtin", name, m.Scope)
		}
		if strings.TrimSpace(m.Description) == "" {
			t.Errorf("skill %q 的 description 为空", name)
		}
		s, err := loader.Load(name)
		if err != nil {
			t.Errorf("加载内置 skill %q: %v", name, err)
			continue
		}
		if strings.TrimSpace(s.Body) == "" {
			t.Errorf("skill %q 正文为空", name)
		}
	}
}

// 磁盘同名 skill 应覆盖内置(用户可自定义)。
func TestDiskOverridesBuiltin(t *testing.T) {
	dir := t.TempDir()
	bn := BuiltinNames()
	if len(bn) == 0 {
		t.Skip("无内置 skill")
	}
	var name string
	for n := range bn {
		name = n
		break
	}
	sub := filepath.Join(dir, name)
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: user override\n---\nUSER BODY"
	if err := os.WriteFile(filepath.Join(sub, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	loader := New(nil, []string{dir})
	s, err := loader.Load(name)
	if err != nil {
		t.Fatalf("Load %q: %v", name, err)
	}
	if s.Scope != "global" || !strings.Contains(s.Body, "USER BODY") {
		t.Fatalf("磁盘未覆盖内置:scope=%q body=%q", s.Scope, s.Body)
	}
}

// 一次性清理:有旧抽取标记时,删掉内置名目录(含清单里已移除的内置),不碰用户自定义 skill。
func TestCleanupExtractedBuiltins(t *testing.T) {
	home := t.TempDir()
	dest := filepath.Join(home, ".deepx", "skills")
	mk := func(name, content string) {
		if err := os.MkdirAll(filepath.Join(dest, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dest, name, "SKILL.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 取一个真实内置名(当前 embed 里有的)+ 一个已被移除的旧内置(只在清单里)+ 一个用户 skill。
	var builtinName string
	for n := range BuiltinNames() {
		builtinName = n
		break
	}
	mk(builtinName, "x")         // 当前内置的旧解压副本 → 应删(在常量清单里)
	mk("using-superpowers", "x") // 历史内置(已移除,但在常量清单里)→ 应删
	mk("user-custom", "x")       // 用户自定义 → 绝不能删
	// 旧抽取标记(仅作「曾抽取过」的信号;删除集来自常量 builtinSkillNames,不再读清单内容)
	if err := os.WriteFile(filepath.Join(dest, manifestFile), []byte(builtinName+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, versionFile), []byte("1.0.0"), 0o644); err != nil {
		t.Fatal(err)
	}

	CleanupExtractedBuiltins(home)

	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(dest, name))
		return err == nil
	}
	if exists(builtinName) {
		t.Errorf("内置副本 %q 应被清理", builtinName)
	}
	if exists("using-superpowers") {
		t.Error("历史内置 using-superpowers(在常量清单里)应被清理")
	}
	if !exists("user-custom") {
		t.Error("用户自定义 user-custom 被误删 —— 清理越界")
	}
	if exists(manifestFile) || exists(versionFile) {
		t.Error("清理后标记文件应被移除")
	}
}

// 守护:skill/skills/ 下每个内置目录都必须出现在常量清单 builtinSkillNames 里。
// 防止「新增/重命名了内置 skill 却忘了同步清单」——那会导致旧解压副本清理不到。
func TestBuiltinSkillNamesCoversEmbed(t *testing.T) {
	listed := map[string]bool{}
	for _, n := range builtinSkillNames {
		listed[n] = true
	}
	for name := range BuiltinNames() {
		if !listed[name] {
			t.Errorf("内置 skill %q 在 skill/skills/ 里但不在 builtinSkillNames 常量清单中——请同步", name)
		}
	}
}

// 没有旧抽取标记时,清理必须是 no-op(不动任何东西)。
func TestCleanupNoMarkersIsNoop(t *testing.T) {
	home := t.TempDir()
	dest := filepath.Join(home, ".deepx", "skills")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	// 没有标记文件,但有个跟内置同名的用户目录 → 不应被删(没迁移信号)。
	var builtinName string
	for n := range BuiltinNames() {
		builtinName = n
		break
	}
	if err := os.MkdirAll(filepath.Join(dest, builtinName), 0o755); err != nil {
		t.Fatal(err)
	}
	CleanupExtractedBuiltins(home)
	if _, err := os.Stat(filepath.Join(dest, builtinName)); err != nil {
		t.Error("无迁移标记时不应删除任何目录")
	}
}
