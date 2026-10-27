package codegen

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/types"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/pkg/servicecatalog"
)

func catalogServiceName(s routesServiceView, services []ServiceDef) string {
	for _, d := range services {
		if d.Name == s.ProtoServiceName {
			return d.Package + "." + d.Name
		}
	}
	return s.ProtoServiceName
}

// graphCatalogClients recognizes the actual typed constructor marker in owned
// providers. No scan for New*Client names or endpoint string values is used.
func graphCatalogClients(in GraphGenInput, m *graphModel, diagnostics *[]servicecatalog.Diagnostic) []servicecatalog.Client {
	out := []servicecatalog.Client{}
	// Validate against the existing descriptor's config fields; never create
	// another endpoint registry or export resolved values/defaults.
	configFields := map[string]bool{}
	if desc, err := loadDescriptor(in.ProjectDir); err == nil && desc != nil {
		for _, cfg := range desc.Configs {
			for _, field := range cfg.Fields {
				if cfg.FullName != "" && field.ProtoType == "string" && !field.Sensitive {
					configFields[cfg.FullName+"."+field.Name] = true
				}
			}
		}
	}
	p := m.u.app
	if p == nil || p.TypesInfo == nil {
		return out
	}
	for _, file := range p.Syntax {
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			provider := strings.TrimPrefix(fd.Name.Name, "New")
			if fd.Recv != nil {
				provider = strings.TrimPrefix(fd.Name.Name, "new")
			}
			if m.byName[provider] == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				fun := call.Fun
				if idx, ok := fun.(*ast.IndexExpr); ok {
					fun = idx.X
				}
				if idx, ok := fun.(*ast.IndexListExpr); ok {
					fun = idx.X
				}
				sel, ok := fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				fn, ok := p.TypesInfo.Uses[sel.Sel].(*types.Func)
				if !ok || fn.Pkg() == nil || fn.Pkg().Path() != "github.com/reliant-labs/forge/pkg/servicecatalog" || fn.Name() != "NewClient" {
					return true
				}
				source := catalogSource(in.ProjectDir, p.Fset, call.Pos())
				cl := servicecatalog.Client{Provider: provider, Source: source, Resolution: "unresolved"}
				if len(call.Args) >= 2 {
					if lit, ok := call.Args[0].(*ast.CompositeLit); ok {
						for _, el := range lit.Elts {
							if kv, ok := el.(*ast.KeyValueExpr); ok {
								if key, ok := kv.Key.(*ast.Ident); ok {
									v := p.TypesInfo.Types[kv.Value].Value
									if v != nil && v.Kind() == constant.String {
										switch key.Name {
										case "Service":
											cl.Service = constant.StringVal(v)
										case "ConfigField":
											cl.ConfigField = constant.StringVal(v)
										}
									}
								}
							}
						}
					}
					if cs, ok := call.Args[1].(*ast.SelectorExpr); ok {
						if obj := p.TypesInfo.Uses[cs.Sel]; obj != nil && obj.Pkg() != nil {
							cl.Constructor = obj.Pkg().Path() + "." + obj.Name()
						}
					}
				}
				// Only proto identities, never arbitrary URLs or secret-like strings.
				if !catalogProtoRef(cl.Service) || !catalogProtoRef(cl.ConfigField) || cl.Constructor == "" || !configFields[cl.ConfigField] || !catalogClientService(p.TypesInfo, call, cl.Service) {
					cl.Service = ""
					cl.ConfigField = ""
					*diagnostics = append(*diagnostics, servicecatalog.Diagnostic{Code: "client_metadata_unresolved", Component: provider, Source: source})
				} else {
					cl.Resolution = "declared"
				}
				cl.ID = fmt.Sprintf("%s:%s:%d", provider, source.Path, source.Line)
				out = append(out, cl)
				return true
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Connect exports <Service>Name next to the constructor; compare that typed
// constant, not the constructor's spelling or package basename.
func catalogClientService(info *types.Info, call *ast.CallExpr, service string) bool {
	if len(call.Args) < 2 {
		return false
	}
	sel, ok := call.Args[1].(*ast.SelectorExpr)
	if !ok {
		return false
	}
	fn, ok := info.Uses[sel.Sel].(*types.Func)
	if !ok || fn.Pkg() == nil {
		return false
	}
	short := service[strings.LastIndex(service, ".")+1:]
	obj, ok := fn.Pkg().Scope().Lookup(short + "Name").(*types.Const)
	return ok && obj.Val().Kind() == constant.String && constant.StringVal(obj.Val()) == service && fn.Name() == "New"+short+"Client"
}

func catalogProtoRef(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for i, r := range part {
			if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
				return false
			}
		}
	}
	return true
}
