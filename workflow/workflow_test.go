package workflow

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeWF(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".js"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoader_ListAndLoad(t *testing.T) {
	proj := filepath.Join(t.TempDir(), "proj")
	glob := filepath.Join(t.TempDir(), "glob")

	writeWF(t, glob, "shared", `export const meta = { name: "shared", description: "global one" };
export default async function main(){ return "G"; }`)
	writeWF(t, glob, "only-global", `export const meta = { name: "only-global", description: "g" };
export default async function main(){ return "g"; }`)
	// project 同名覆盖 global
	writeWF(t, proj, "shared", `export const meta = { name: "shared", description: "project one" };
export default async function main(){ return "P"; }`)

	l := New([]string{proj}, []string{glob})

	list := l.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 workflows, got %d: %+v", len(list), list)
	}
	// 字典序:only-global, shared
	if list[0].Name != "only-global" || list[1].Name != "shared" {
		t.Fatalf("unexpected order: %+v", list)
	}
	// shared 应被 project 覆盖
	if list[1].Scope != "project" || list[1].Description != "project one" {
		t.Fatalf("shared not overridden by project: %+v", list[1])
	}

	// Load shared → 取 project 版,跑出来是 "P"
	s, err := l.Load("shared")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Scope != "project" {
		t.Fatalf("Load scope = %s, want project", s.Scope)
	}
	got, err := Run(context.Background(), s, RunOptions{Executor: &mockExec{}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != "P" {
		t.Fatalf("ran %q, want P", got)
	}
}

func TestLoader_LoadNotFound(t *testing.T) {
	l := New(nil, []string{t.TempDir()})
	if _, err := l.Load("nope"); err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestLoader_RejectsBadName(t *testing.T) {
	l := New(nil, []string{t.TempDir()})
	if _, err := l.Load("../etc/passwd"); err == nil {
		t.Fatal("expected rejection of unsafe name")
	}
}

func TestDefaultDirs(t *testing.T) {
	proj, glob := DefaultDirs("/ws", "/home/u")
	// 同时扫 .claude(兼容 Claude Code)和 .deepx(自家),.deepx 在后(同名覆盖)。
	eq := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	wantProj := []string{filepath.Join("/ws", ".claude", "workflows"), filepath.Join("/ws", ".deepx", "workflows")}
	wantGlob := []string{filepath.Join("/home/u", ".claude", "workflows"), filepath.Join("/home/u", ".deepx", "workflows")}
	if !eq(proj, wantProj) {
		t.Fatalf("proj dirs = %v, want %v", proj, wantProj)
	}
	if !eq(glob, wantGlob) {
		t.Fatalf("glob dirs = %v, want %v", glob, wantGlob)
	}
}

func TestLoader_MjsAndClaudeCompat(t *testing.T) {
	proj := t.TempDir()
	// 用 .mjs 写一个 workflow(对齐 Claude Code),Loader 应能发现 + 加载
	writeWFExt(t, proj, "flow-a", "mjs", `export const meta = { name: "flow-a", description: "mjs one" };
export default async function main(){ return "A"; }`)
	// .js 仍兼容
	writeWFExt(t, proj, "flow-b", "js", `export const meta = { name: "flow-b", description: "js one" };
export default async function main(){ return "B"; }`)

	l := New([]string{proj}, nil)
	names := map[string]string{}
	for _, m := range l.List() {
		names[m.Name] = m.Description
	}
	if names["flow-a"] != "mjs one" || names["flow-b"] != "js one" {
		t.Fatalf("List 未同时发现 .mjs / .js: %v", names)
	}
	s, err := l.Load("flow-a")
	if err != nil || s.Scope != "project" {
		t.Fatalf("Load flow-a(.mjs): %v scope=%v", err, s)
	}

	// Save 默认写 .mjs
	p, err := Save(proj, "flow-c", "export const meta={name:\"flow-c\"};", false)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if filepath.Ext(p) != ".mjs" {
		t.Fatalf("Save 应写 .mjs, got %s", p)
	}
}

func writeWFExt(t *testing.T, dir, name, ext, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+"."+ext), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
