package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkflowJournal_PersistReloadResume(t *testing.T) {
	home := t.TempDir()
	src := `export const meta={name:"j"}; export default async function main(){}`
	args := []byte(`{"v":"1"}`)

	// 第一次:新建,写两条结果。
	j1, cached, _ := loadWorkflowJournal(home, "j", src, args, "")
	if cached != 0 {
		t.Fatalf("全新应 cached=0, got %d", cached)
	}
	j1.Put(0, "r0")
	j1.Put(1, "r1")

	// 重载(模拟下次重跑):应 resume 出 2 条。
	j2, cached, _ := loadWorkflowJournal(home, "j", src, args, "")
	if cached != 2 {
		t.Fatalf("重载应 cached=2, got %d", cached)
	}
	if r, ok := j2.Get(1); !ok || r != "r1" {
		t.Fatalf("Get(1) = %q,%v", r, ok)
	}

	// 脚本变了 → 旧 journal 作废,从头来。
	j3, cached, _ := loadWorkflowJournal(home, "j", src+"// changed", args, "")
	if cached != 0 {
		t.Fatalf("脚本变更应 cached=0(作废), got %d", cached)
	}
	if _, ok := j3.Get(0); ok {
		t.Fatal("脚本变更后不应命中旧缓存")
	}
}

func TestWorkflowJournal_DiscardAndArgsIsolation(t *testing.T) {
	home := t.TempDir()
	src := `export const meta={name:"j"};`

	jA, _, _ := loadWorkflowJournal(home, "j", src, []byte(`{"a":1}`), "")
	jA.Put(0, "ra")
	jB, cached, _ := loadWorkflowJournal(home, "j", src, []byte(`{"a":2}`), "") // 不同 args → 独立 journal
	if cached != 0 {
		t.Fatalf("不同 args 应独立(cached=0), got %d", cached)
	}
	_ = jB

	// Discard 后文件应消失,重载 cached=0。
	jA.Discard()
	if _, err := os.Stat(jA.path); !os.IsNotExist(err) {
		t.Fatalf("Discard 后 journal 文件应被删:%v", err)
	}
	jA2, cached, _ := loadWorkflowJournal(home, "j", src, []byte(`{"a":1}`), "")
	if cached != 0 {
		t.Fatalf("Discard 后重载应 cached=0, got %d", cached)
	}
	// journal 应落在 ~/.deepx/workflows/.journal/ 下
	if filepath.Dir(jA2.path) != filepath.Join(home, ".deepx", "workflows", ".journal") {
		t.Fatalf("journal 路径不对: %s", jA2.path)
	}
}
