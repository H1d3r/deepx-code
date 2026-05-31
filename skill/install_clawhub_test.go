package skill

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildZip 构造一个内存 ZIP,files 是 name → content。
func buildZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeClawhubServer 起一个 httptest server,/api/v1/download?slug=<slug> 返回 zipByName[slug]。
// 找不到 slug 返回 404。
func fakeClawhubServer(t *testing.T, zipByName map[string][]byte) (*httptest.Server, SkillSource) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/download") {
			http.NotFound(w, r)
			return
		}
		slug := r.URL.Query().Get("slug")
		body, ok := zipByName[slug]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(body)
	}))
	src := SkillSource{
		ID:      "test-clawhub",
		Name:    "test",
		Type:    "clawhub",
		URL:     srv.URL,
		Enabled: true,
	}
	return srv, src
}

// 路径 1:SKILL.md 在 zip 根,直接安装成功。
func TestInstallFromClawhub_RootSKILL(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root, _ := InstalledDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	zipBody := buildZip(t, map[string]string{
		"SKILL.md":  "---\nname: alpha\ndescription: x\n---\nbody",
		"extra.txt": "hello",
	})
	srv, src := fakeClawhubServer(t, map[string][]byte{"alpha": zipBody})
	defer srv.Close()

	name, err := installFromClawhub(context.Background(), src, "alpha", root)
	if err != nil {
		t.Fatalf("装失败:%v", err)
	}
	if name != "alpha" {
		t.Errorf("name 应是 alpha,got %q", name)
	}
	if _, err := os.Stat(filepath.Join(root, "alpha", "SKILL.md")); err != nil {
		t.Errorf("SKILL.md 缺失:%v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "alpha", "extra.txt")); string(b) != "hello" {
		t.Errorf("extra.txt 内容错")
	}
}

// 路径 2:SKILL.md 在一层子目录,应自动剥层。
func TestInstallFromClawhub_NestedSKILL(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root, _ := InstalledDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	zipBody := buildZip(t, map[string]string{
		"beta-v1/SKILL.md": "---\nname: beta\ndescription: x\n---\nbody",
		"beta-v1/notes.md": "more",
	})
	srv, src := fakeClawhubServer(t, map[string][]byte{"beta": zipBody})
	defer srv.Close()

	if _, err := installFromClawhub(context.Background(), src, "beta", root); err != nil {
		t.Fatalf("装失败:%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "beta", "SKILL.md")); err != nil {
		t.Errorf("剥层后 SKILL.md 应在 ~/.deepx/skills/beta/:%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "beta", "notes.md")); err != nil {
		t.Errorf("剥层后 notes.md 应在:%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "beta", "beta-v1")); err == nil {
		t.Errorf("剥层失败,beta-v1/ 还在")
	}
}

// 路径 3:zip 里完全没 SKILL.md,应报错且不留半残目录。
func TestInstallFromClawhub_MissingSKILL(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root, _ := InstalledDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	zipBody := buildZip(t, map[string]string{
		"README.md": "hi",
		"dir/x.txt": "x",
	})
	srv, src := fakeClawhubServer(t, map[string][]byte{"gamma": zipBody})
	defer srv.Close()

	_, err := installFromClawhub(context.Background(), src, "gamma", root)
	if err == nil || !strings.Contains(err.Error(), "SKILL.md") {
		t.Errorf("无 SKILL.md 应报错,got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "gamma")); !os.IsNotExist(err) {
		t.Errorf("失败时不应留 ~/.deepx/skills/gamma/")
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") && strings.Contains(e.Name(), "staging") {
			t.Errorf("staging 目录残留:%s", e.Name())
		}
	}
}

// 路径 4:zip slip —— 文件名带 ../,应被拦截。
func TestInstallFromClawhub_ZipSlipRejected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root, _ := InstalledDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	zipBody := buildZip(t, map[string]string{
		"SKILL.md":         "---\nname: x\n---\nb",
		"../../evil.txt":   "pwned",
		"safe/inner/a.txt": "ok",
	})
	srv, src := fakeClawhubServer(t, map[string][]byte{"slip": zipBody})
	defer srv.Close()

	_, err := installFromClawhub(context.Background(), src, "slip", root)
	if err == nil || !strings.Contains(err.Error(), "可疑路径") {
		t.Errorf("含 ../ 的 zip 应被拦截,got: %v", err)
	}
}

// 路径 5:解压总大小超 10MB 上限。
func TestInstallFromClawhub_TotalSizeCap(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root, _ := InstalledDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("a", 9*1024*1024)
	big2 := strings.Repeat("b", 5*1024*1024)
	zipBody := buildZip(t, map[string]string{
		"SKILL.md": "---\nname: x\n---\nb",
		"big1.txt": big,
		"big2.txt": big2,
	})
	srv, src := fakeClawhubServer(t, map[string][]byte{"bomb": zipBody})
	defer srv.Close()

	_, err := installFromClawhub(context.Background(), src, "bomb", root)
	if err == nil {
		t.Errorf("超 10MB 应被拦截,但成功了")
		return
	}
	if !strings.Contains(err.Error(), "超") {
		t.Errorf("应报'超...上限',got: %v", err)
	}
}

// InstallFromSource:非法 remoteRef 直接被白名单拒,不到网络层。
func TestInstallFromSource_RefValidation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for _, bad := range []string{"foo/bar", "../etc", "/abs", "name with space", ""} {
		_, err := InstallFromSource(context.Background(), SourceIDClawhub, bad)
		if err == nil || !strings.Contains(err.Error(), "非法") {
			t.Errorf("非法 ref %q 应被拒,got: %v", bad, err)
		}
	}
}

// InstallFromSource:未知 source ID 应报错。
func TestInstallFromSource_UnknownSource(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	_, err := InstallFromSource(context.Background(), "not-exist", "alpha")
	if err == nil || !strings.Contains(err.Error(), "未找到源") {
		t.Errorf("未知 source 应报错,got: %v", err)
	}
}

// slug 原样传给 server query。
func TestInstallFromClawhub_SlugInQuery(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root, _ := InstalledDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	zipBody := buildZip(t, map[string]string{
		"SKILL.md": "---\nname: x\n---\nb",
	})
	var gotSlug string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSlug = r.URL.Query().Get("slug")
		_, _ = w.Write(zipBody)
	}))
	defer srv.Close()
	src := SkillSource{ID: "s", Type: "clawhub", URL: srv.URL, Enabled: true}

	want := "my-skill.v2_beta"
	if _, err := installFromClawhub(context.Background(), src, want, root); err != nil {
		t.Fatal(err)
	}
	if gotSlug != want {
		t.Errorf("slug 应原样传过去 (want=%q got=%q,URL-escaped=%q)", want, gotSlug, url.QueryEscape(want))
	}
}
