// Temporary isolated proof driver; not part of a product PR. It invokes the
// actual code generator at the recorded Forge checkout, never synthesizes output.
package main

import (
	"fmt"
	"os"
	"github.com/reliant-labs/forge/internal/codegen"
)
func main(){
	if len(os.Args)!=2{panic("fixture root required")}
	if err:=codegen.GenerateGraph(codegen.GraphGenInput{GenContext:codegen.GenContext{
		ProjectDir:os.Args[1],ModulePath:"github.com/reliant-labs/control-plane/rootsproof",
	}});err!=nil{fmt.Fprintln(os.Stderr,err);os.Exit(1)}
}
