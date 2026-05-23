package codegraph

import (
	"path/filepath"
	"testing"
	"time"
)

// 验证 go/types 精确解析消除同名合并:User.Save 和 Order.Save 同名,
// 按限定名查 callers 应各自精确,按短名查才合并。
func TestGoPreciseCallers(t *testing.T) {
	dir := t.TempDir()
	must(t, filepath.Join(dir, "go.mod"), "module foo\n\ngo 1.21\n")
	must(t, filepath.Join(dir, "svc.go"), `package foo

type User struct{}
func (u *User) Save() {}

type Order struct{}
func (o *Order) Save() {}

func handleUser(u *User)   { u.Save() }
func handleOrder(o *Order) { o.Save() }
`)

	ix := NewIndex(dir)
	if _, err := ix.Graph(); err != nil { // 快图(语法近似),精确解析走后台
		t.Fatal(err)
	}
	// 等后台精确解析换图(轮询 Callers 直到 User.Save 精确到 1 处)
	var g *Graph
	for i := 0; i < 200; i++ {
		g, _ = ix.Graph()
		if uc, _ := g.Callers("User.Save", 50); len(uc) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 精确:User.Save 只被 handleUser 调
	uc, _ := g.Callers("User.Save", 50)
	if len(uc) != 1 || uc[0].From != "handleUser" {
		t.Fatalf("callers(User.Save) = %+v, 期望仅 handleUser", uc)
	}
	// 精确:Order.Save 只被 handleOrder 调
	oc, _ := g.Callers("Order.Save", 50)
	if len(oc) != 1 || oc[0].From != "handleOrder" {
		t.Fatalf("callers(Order.Save) = %+v, 期望仅 handleOrder", oc)
	}
	// 短名:合并视图,两处都算
	all, total := g.Callers("Save", 50)
	if total != 2 {
		t.Fatalf("callers(Save) 短名应合并为 2, got %d (%+v)", total, all)
	}
	t.Log("go/types 精确:User.Save / Order.Save 不再合并 ✓")
}
