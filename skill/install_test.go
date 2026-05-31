package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 安全护栏:Delete 只许动 ~/.deepx/skills/<name>/,name 必须通过 safeName 白名单,
// 路径 Clean 后还要再次校验落在 root 下 —— 杜绝 `../../foo` 钻出去 rm。
func TestDelete_SecurityGuards(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root, _ := InstalledDir()

	// 不存在的 skill:不报"非法路径",而是报"不属于 deepx 管理"
	if err := Delete("never-existed"); err == nil || !strings.Contains(err.Error(), "不在") {
		t.Errorf("不存在的 skill 应报'不在 ~/.deepx/skills/ 下',got: %v", err)
	}

	// 非法名(含 / 或 空格):必须被 safeName 白名单一刀拒,根本不到 rm 阶段
	for _, bad := range []string{"../etc", "foo/bar", "/abs/path", "name with space"} {
		err := Delete(bad)
		if err == nil || !strings.Contains(err.Error(), "非法") {
			t.Errorf("非法名 %q 必须被拒,实际: %v", bad, err)
		}
	}
	// `..` 单独的边角:regex 允许(全是 . 字符),但 filepath.Clean 后越出 root,
	// 第二道防御"非法路径(疑似越界)"接住。
	if err := Delete(".."); err == nil || !strings.Contains(err.Error(), "越界") {
		t.Errorf(".. 应被越界检查拒,实际: %v", err)
	}
	// `.` 单独的边角:regex 允许,Clean 后是 root 本身,SKILL.md 检查不通过 → 报"不在"。
	if err := Delete("."); err == nil || !strings.Contains(err.Error(), "不在") {
		t.Errorf(". 应被 SKILL.md 检查拒,实际: %v", err)
	}

	// 合法但不存在
	if err := Delete("legit-name"); err == nil || !strings.Contains(err.Error(), "不在") {
		t.Errorf("合法名但目录不存在 → 应报'不在',got: %v", err)
	}

	// 真造一个 fake skill,Delete 应该成功
	fake := filepath.Join(root, "fake-skill")
	if err := os.MkdirAll(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fake, "SKILL.md"), []byte("---\nname: fake\ndescription: x\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Delete("fake-skill"); err != nil {
		t.Errorf("已装 skill 删除失败: %v", err)
	}
	if _, err := os.Stat(fake); !os.IsNotExist(err) {
		t.Errorf("删除后目录应消失,实际仍在")
	}
}

// Install 路径分诊:HTTPS GitHub URL / 本地路径要走对应分支,
// 不识别的 source 直接报错,不去网络/磁盘乱试。
func TestInstall_SourceDispatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// 空 source
	if _, err := Install(""); err == nil || !strings.Contains(err.Error(), "不能为空") {
		t.Errorf("空 source 应报错,got: %v", err)
	}

	// 不识别协议(非 github URL / 非本地路径)
	for _, bad := range []string{"foo/bar", "git@github.com:foo/bar.git", "ftp://x.com/y"} {
		if _, err := Install(bad); err == nil || !strings.Contains(err.Error(), "无法识别") {
			t.Errorf("无法识别的 source %q 应报错,got: %v", bad, err)
		}
	}

	// 本地路径但目录不存在
	if _, err := Install("/this/path/should/not/exist"); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Errorf("不存在的本地路径应报错,got: %v", err)
	}

	// 本地目录存在但没 SKILL.md
	noSkill := t.TempDir()
	if _, err := Install(noSkill); err == nil || !strings.Contains(err.Error(), "没有 SKILL.md") {
		t.Errorf("目录无 SKILL.md 应报错,got: %v", err)
	}

	// 本地目录有 SKILL.md → 安装成功
	good := t.TempDir()
	if err := os.WriteFile(filepath.Join(good, "SKILL.md"), []byte("---\nname: test\ndescription: x\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	name, err := Install(good)
	if err != nil {
		t.Fatalf("合法本地 skill 安装失败: %v", err)
	}
	if name != filepath.Base(good) {
		t.Errorf("装入名应是源目录名 %q,got %q", filepath.Base(good), name)
	}
	// 装到对应位置
	root, _ := InstalledDir()
	if _, err := os.Stat(filepath.Join(root, name, "SKILL.md")); err != nil {
		t.Errorf("装好的 SKILL.md 不见了: %v", err)
	}

	// 再装一次同源:同名应被拒(不静默覆盖)
	if _, err := Install(good); err == nil || !strings.Contains(err.Error(), "已存在") {
		t.Errorf("同名 skill 已装应报'已存在',got: %v", err)
	}
}

// parseGitHubInstallURL 必须正确区分单 repo / tree-subpath / 异常形式。
func TestParseGitHubInstallURL(t *testing.T) {
	cases := []struct {
		raw                              string
		clone, branch, subpath, baseName string
		wantErr                          bool
	}{
		// 单 skill 仓库
		{"https://github.com/anthropics/claude-skill-x",
			"https://github.com/anthropics/claude-skill-x", "", "", "claude-skill-x", false},
		{"https://github.com/o/r.git",
			"https://github.com/o/r", "", "", "r", false},
		{"https://github.com/o/r/",
			"https://github.com/o/r", "", "", "r", false},
		// tree 但无子路径(只是限定分支)
		{"https://github.com/o/r/tree/dev",
			"https://github.com/o/r", "dev", "", "r", false},
		// 多 skill 仓库 + 子路径(anthropics/skills 的典型形式)
		{"https://github.com/anthropics/skills/tree/main/skills/docx",
			"https://github.com/anthropics/skills", "main", "skills/docx", "docx", false},
		{"https://github.com/o/r/tree/dev/sub/deeper",
			"https://github.com/o/r", "dev", "sub/deeper", "deeper", false},
		// blob → 必须明确报错(防止用户复制了文件 URL)
		{"https://github.com/o/r/blob/main/SKILL.md", "", "", "", "", true},
		// 非 GitHub
		{"https://gitlab.com/o/r", "", "", "", "", true},
		{"random-string", "", "", "", "", true},
		{"https://github.com/", "", "", "", "", true},
		{"https://github.com/onlyowner", "", "", "", "", true},
	}
	for _, c := range cases {
		cl, br, sp, bn, err := parseGitHubInstallURL(c.raw)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q 应报错", c.raw)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.raw, err)
			continue
		}
		if cl != c.clone || br != c.branch || sp != c.subpath || bn != c.baseName {
			t.Errorf("%q: got clone=%q branch=%q subpath=%q baseName=%q",
				c.raw, cl, br, sp, bn)
		}
	}
}

// InstalledList 只看 ~/.deepx/skills/ 下的,不掺 ~/.claude / ~/.agents 的(防误删别人工具的)。
func TestInstalledList_OnlyDeepxOwned(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root, _ := InstalledDir()

	// 空目录返回空列表,不报错
	got, err := InstalledList()
	if err != nil {
		t.Fatalf("空目录 List 不应报错: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("空目录 List 应返回 0 条,got %d", len(got))
	}

	// 装两个 fake skill
	for _, n := range []string{"alpha", "bravo"} {
		dir := filepath.Join(root, n)
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "SKILL.md"),
			[]byte("---\nname: "+n+"\ndescription: x\n---\nbody"), 0o644)
	}
	got, err = InstalledList()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "bravo" {
		t.Errorf("应返回字典序 alpha, bravo,got %+v", got)
	}
}
