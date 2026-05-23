package codegraph

import (
	"os"
	"path/filepath"
	"testing"
)

// 验证 tree-sitter 兜底:同一 workspace 里 .py / .ts 文件能抽出符号、调用边、import 边,
// 且故意带 f-string / 泛型这些 gpython 会挂的语法。
func TestTreeSitterPyTs(t *testing.T) {
	dir := t.TempDir()
	py := `import os

class UserService:
    def get_user(self, id):
        msg = f"fetch {id}"
        return self._load(id)
    def _load(self, id):
        return id

def make():
    return UserService()
`
	ts := `import { foo } from "./bar"
export class Repo {
  find<T>(id: string): T { return this.cache.get(id) }
}
`
	must(t, filepath.Join(dir, "svc.py"), py)
	must(t, filepath.Join(dir, "repo.ts"), ts)

	g, err := NewIndex(dir).Graph()
	if err != nil {
		t.Fatal(err)
	}

	// Python:类 + 方法 + 顶层函数都在
	if d := g.Def("UserService"); len(d) != 1 || d[0].Kind != KindType {
		t.Fatalf("UserService = %+v, 期望 1 个 type", d)
	}
	if d := g.Def("get_user"); len(d) != 1 {
		t.Fatalf("get_user 应被抽到, got %+v", d)
	}
	// 调用边:get_user 调了 _load(self._load)
	if cs, _ := g.Callees("get_user", 50); !hasEdgeTo(cs, "_load") {
		t.Fatalf("get_user 的 callees 应含 _load, got %+v", cs)
	}
	// callers:_load 被 get_user 调
	if cs, total := g.Callers("_load", 50); total < 1 {
		t.Fatalf("_load 应至少被调 1 次, got %+v", cs)
	}
	// import 边
	if imps := g.Imports("svc.py"); !hasEdgeTo(imps, "os") {
		t.Fatalf("svc.py 应 import os, got %+v", imps)
	}
	// TypeScript:泛型方法的类
	if d := g.Def("Repo"); len(d) != 1 || d[0].Kind != KindType {
		t.Fatalf("Repo = %+v, 期望 1 个 type", d)
	}
	t.Logf("tree-sitter py/ts ok:符号/调用/import 全抽到")
}

func must(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hasEdgeTo(edges []Edge, to string) bool {
	for _, e := range edges {
		if e.To == to {
			return true
		}
	}
	return false
}
