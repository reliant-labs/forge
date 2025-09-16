package contract

import (
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

var Analyzer = &analysis.Analyzer{
	Name:     "contract",
	Doc:      "checks that types implementing contract interfaces (defined in contract.go) have no exported methods outside the interface",
	Run:      run,
	Requires: []*analysis.Analyzer{inspect.Analyzer},
}

func run(pass *analysis.Pass) (interface{}, error) {
	// Step 1: Find contract.go and extract interfaces from it.
	contractInterfaces := extractContractInterfaces(pass)
	if len(contractInterfaces) == 0 {
		return nil, nil
	}

	// Step 2: For each named type in the package, check if it implements any
	// contract interface. If so, collect the allowed method names.
	//
	// A type T implements interface I if either T or *T satisfies I.
	// We gather all implementing types along with which interface(s) they satisfy.
	type implInfo struct {
		ifaceName   string
		ifaceType   *types.Interface
		methodNames map[string]bool
	}

	typeConstraints := make(map[*types.Named][]implInfo)

	for _, obj := range pass.TypesInfo.Defs {
		tn, ok := obj.(*types.TypeName)
		if !ok || tn.IsAlias() {
			continue
		}
		named, ok := tn.Type().(*types.Named)
		if !ok {
			continue
		}

		for ifaceName, ifaceType := range contractInterfaces {
			// Check if *T or T implements the interface.
			ptrType := types.NewPointer(named)
			if !types.Implements(named, ifaceType) && !types.Implements(ptrType, ifaceType) {
				continue
			}
			// Collect allowed method names from the interface (including embedded).
			allowed := make(map[string]bool)
			for i := 0; i < ifaceType.NumMethods(); i++ {
				allowed[ifaceType.Method(i).Name()] = true
			}
			typeConstraints[named] = append(typeConstraints[named], implInfo{
				ifaceName:   ifaceName,
				ifaceType:   ifaceType,
				methodNames: allowed,
			})
		}
	}

	if len(typeConstraints) == 0 {
		return nil, nil
	}

	// Step 2b: For each type, prune interfaces that are strict subsets of
	// another implemented interface. E.g. if Service embeds Base, and the
	// type implements both, only enforce Service (the superset).
	for named, constraints := range typeConstraints {
		if len(constraints) <= 1 {
			continue
		}
		pruned := make([]implInfo, 0, len(constraints))
		for i, ci := range constraints {
			dominated := false
			for j, cj := range constraints {
				if i == j {
					continue
				}
				// ci is dominated if cj is a strict superset of ci.
				if ci.ifaceType.NumMethods() < cj.ifaceType.NumMethods() && types.Implements(cj.ifaceType, ci.ifaceType) {
					dominated = true
					break
				}
			}
			if !dominated {
				pruned = append(pruned, ci)
			}
		}
		typeConstraints[named] = pruned
	}

	// Step 3: Walk all method declarations and report violations.
	insp := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	nodeFilter := []ast.Node{(*ast.FuncDecl)(nil)}

	insp.Preorder(nodeFilter, func(n ast.Node) {
		funcDecl := n.(*ast.FuncDecl)

		// Only care about methods (has receiver).
		if funcDecl.Recv == nil || len(funcDecl.Recv.List) == 0 {
			return
		}

		// Only care about exported methods.
		if !funcDecl.Name.IsExported() {
			return
		}

		methodName := funcDecl.Name.Name

		// Resolve the receiver's named type.
		recvType := pass.TypesInfo.TypeOf(funcDecl.Recv.List[0].Type)
		if recvType == nil {
			return
		}
		// Dereference pointer receiver.
		if ptr, ok := recvType.(*types.Pointer); ok {
			recvType = ptr.Elem()
		}
		named, ok := recvType.(*types.Named)
		if !ok {
			return
		}

		constraints, ok := typeConstraints[named]
		if !ok {
			return
		}

		// The method must be declared in ALL interfaces the type implements.
		// (In practice, a type usually implements one contract interface.)
		for _, c := range constraints {
			if !c.methodNames[methodName] {
				pass.Reportf(funcDecl.Name.Pos(),
					"exported method %s on type %s is not declared in the %s interface (contract.go)",
					methodName, named.Obj().Name(), c.ifaceName)
			}
		}
	})

	return nil, nil
}

// extractContractInterfaces scans the package for a file named "contract.go"
// and returns all interface types declared in it, keyed by name.
func extractContractInterfaces(pass *analysis.Pass) map[string]*types.Interface {
	result := make(map[string]*types.Interface)

	for _, file := range pass.Files {
		filename := pass.Fset.Position(file.Pos()).Filename
		if filepath.Base(filename) != "contract.go" {
			continue
		}

		for _, decl := range file.Decls {
			genDecl, ok := decl.(*ast.GenDecl)
			if !ok || genDecl.Tok != token.TYPE {
				continue
			}
			for _, spec := range genDecl.Specs {
				typeSpec := spec.(*ast.TypeSpec)
				if _, ok := typeSpec.Type.(*ast.InterfaceType); !ok {
					continue
				}
				// Look up the types.Interface from the type checker.
				obj := pass.TypesInfo.Defs[typeSpec.Name]
				if obj == nil {
					continue
				}
				tn, ok := obj.(*types.TypeName)
				if !ok {
					continue
				}
				iface, ok := tn.Type().Underlying().(*types.Interface)
				if !ok {
					continue
				}
				// Only consider exported interfaces — contract interfaces are
				// part of the public API of the package.
				if !strings.HasPrefix(typeSpec.Name.Name, strings.ToUpper(typeSpec.Name.Name[:1])) {
					continue
				}
				result[typeSpec.Name.Name] = iface
			}
		}
	}

	return result
}