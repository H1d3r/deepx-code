package codegraph

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
)

func init() { Register(&goParser{}) }

// goParser 用 stdlib go/parser 做语法级解析,零外部依赖、不要求代码能编译。
// 抽取:顶层函数 / 方法(带接收者类型)/ 类型 / 结构体字段 / 顶层 var-const。
// 引用:文件内所有标识符出现点(只命中真正的 ident,不会撞注释 / 字符串)。
//
// 注:这是语法级,跨包的精确调用 / 引用解析(go/types)留作后续增强 —— 当前足够做
// "找定义 / 找引用 / 文件结构"这类导航,且对不能编译的代码也照常工作。
type goParser struct{}

func (p *goParser) Lang() string   { return "go" }
func (p *goParser) Exts() []string { return []string{".go"} }

func (p *goParser) Parse(relPath string, src []byte) (ParseResult, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, relPath, src, parser.SkipObjectResolution)
	if err != nil {
		return ParseResult{}, err
	}

	var syms []Symbol
	var edges []Edge
	line := func(pos token.Pos) int { return fset.Position(pos).Line }

	// import 边:每个 import path 一条,From=文件,To=路径。
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if path != "" {
			edges = append(edges, Edge{From: relPath, To: path, Kind: EdgeImport, File: relPath, Line: line(imp.Pos())})
		}
	}

	add := func(name string, kind Kind, container string, sig string, start, end token.Pos) {
		if name == "" || name == "_" {
			return
		}
		syms = append(syms, Symbol{
			Name:      name,
			Kind:      kind,
			File:      relPath,
			Line:      line(start),
			EndLine:   line(end),
			Container: container,
			Signature: oneline(sig),
			Lang:      "go",
			Exported:  ast.IsExported(name),
		})
	}

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			kind, container := KindFunc, ""
			if d.Recv != nil && len(d.Recv.List) > 0 {
				kind, container = KindMethod, receiverType(d.Recv.List[0].Type)
			}
			// 只打印签名(Body 置 nil),得到 "func (r R) Name(args) ret"。
			sig := printNode(fset, &ast.FuncDecl{Recv: d.Recv, Name: d.Name, Type: d.Type})
			add(d.Name.Name, kind, container, sig, d.Pos(), d.End())

			// call 边:扫这个函数体里的调用,From=调用方限定名,To=被调用短名。
			if d.Body != nil {
				from := d.Name.Name
				if container != "" {
					from = container + "." + d.Name.Name
				}
				ast.Inspect(d.Body, func(n ast.Node) bool {
					ce, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					if callee := calleeName(ce.Fun); callee != "" {
						edges = append(edges, Edge{From: from, To: callee, Kind: EdgeCall, File: relPath, Line: line(ce.Pos())})
					}
					return true
				})
			}

		case *ast.GenDecl:
			switch d.Tok {
			case token.TYPE:
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					add(ts.Name.Name, KindType, "", "type "+ts.Name.Name+" "+typeKindHint(ts.Type), ts.Pos(), ts.End())
					// 结构体字段作为 field 符号,container = 类型名。
					if st, ok := ts.Type.(*ast.StructType); ok && st.Fields != nil {
						for _, fld := range st.Fields.List {
							for _, fn := range fld.Names {
								add(fn.Name, KindField, ts.Name.Name, fn.Name+" "+printNode(fset, fld.Type), fn.Pos(), fld.End())
							}
						}
					}
				}
			case token.VAR, token.CONST:
				kind := KindVar
				if d.Tok == token.CONST {
					kind = KindConst
				}
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, n := range vs.Names {
						add(n.Name, kind, "", d.Tok.String()+" "+n.Name, n.Pos(), n.End())
					}
				}
			}
		}
	}

	// 引用:遍历全部标识符出现点。
	var refs []Ref
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name != "_" {
			refs = append(refs, Ref{Name: id.Name, File: relPath, Line: line(id.Pos())})
		}
		return true
	})

	return ParseResult{Symbols: syms, Refs: refs, Edges: edges}, nil
}

// calleeName 从调用表达式的 Fun 取被调用的短名:
//
//	foo()      → "foo"        (Ident)
//	x.Bar()    → "Bar"        (SelectorExpr,拿 .Sel)
//	pkg.Foo()  → "Foo"
//
// 调用的是表达式结果(如 getFn()())等情况返回空,跳过。
func calleeName(fun ast.Expr) string {
	switch t := fun.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.IndexExpr: // 泛型函数实例化 f[T]()
		return calleeName(t.X)
	case *ast.IndexListExpr:
		return calleeName(t.X)
	}
	return ""
}

// receiverType 把方法接收者的类型表达式还原成裸类型名(剥指针 / 泛型参数)。
func receiverType(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return receiverType(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr: // 泛型接收者 T[P]
		return receiverType(t.X)
	case *ast.IndexListExpr: // 泛型接收者 T[P, Q]
		return receiverType(t.X)
	}
	return ""
}

// typeKindHint 给类型一个简短的底层种类提示,避免把整个结构体定义塞进签名。
func typeKindHint(expr ast.Expr) string {
	switch expr.(type) {
	case *ast.StructType:
		return "struct"
	case *ast.InterfaceType:
		return "interface"
	case *ast.FuncType:
		return "func"
	case *ast.MapType:
		return "map"
	case *ast.ArrayType:
		return "array/slice"
	}
	return ""
}

func printNode(fset *token.FileSet, node any) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, node); err != nil {
		return ""
	}
	return buf.String()
}

// oneline 把多行 / 带制表的签名折成单行并截断,保证输出紧凑、行宽稳定。
func oneline(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	s = strings.TrimSpace(s)
	const max = 120
	if len(s) > max {
		s = s[:max-1] + "…"
	}
	return s
}
