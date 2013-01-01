/*
Copyright (c) 2011, 2012 Andrew Wilkins <axwalk@gmail.com>

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction, including without limitation the rights to
use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies
of the Software, and to permit persons to whom the Software is furnished to do
so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package llgo

import (
	"fmt"
	"github.com/axw/gollvm/llvm"
	"go/ast"
	"go/scanner"
	"go/token"
	"go/types"
	"reflect"
)

func (c *compiler) VisitFuncProtoDecl(f *ast.FuncDecl) *LLVMValue {
	if f.Name.Obj != nil {
		if result, ok := f.Name.Obj.Data.(*LLVMValue); ok {
			return result
		}
	}

	var ftyp *types.Signature
	fname := f.Name.String()
	if f.Recv == nil && fname == "init" {
		// Make "init" functions anonymous.
		fname = ""
		// "init" functions aren't recorded by the parser, so f.Name.Obj is
		// not set.
		ftyp = &types.Signature{}
	} else {
		var pkgname string
		ftyp = f.Name.Obj.Type.(*types.Signature)
		if ftyp.Recv != nil {
			recvtyp := ftyp.Recv.Type.(types.Type)
			var recvname string
			switch recvtyp := recvtyp.(type) {
			case *types.Pointer:
				named := recvtyp.Base.(*types.NamedType)
				recvname = "*" + named.Obj.Name
				pkgname = c.pkgmap[named.Obj]
			case *types.NamedType:
				recvname = recvtyp.Obj.Name
				pkgname = c.pkgmap[recvtyp.Obj]
			}
			fname = fmt.Sprintf("%s.%s", recvname, fname)
		} else {
			pkgname = c.pkgmap[f.Name.Obj]
		}
		fname = pkgname + "." + fname
	}

	// gcimporter may produce multiple AST objects for the same function.
	fn := c.module.Module.NamedFunction(fname)
	if fn.IsNil() {
		llvmftyp := c.types.ToLLVM(ftyp).ElementType()
		fn = llvm.AddFunction(c.module.Module, fname, llvmftyp)
		if ftyp.Recv != nil {
			// Create an interface function if the receiver is
			// not a pointer type.
			recvtyp := ftyp.Recv.Type.(types.Type)
			if _, ptr := recvtyp.(*types.Pointer); !ptr {
				returntyp := llvmftyp.ReturnType()
				paramtypes := llvmftyp.ParamTypes()
				paramtypes[0] = llvm.PointerType(paramtypes[0], 0)
				ifntyp := llvm.FunctionType(returntyp, paramtypes, false)
				llvm.AddFunction(c.module.Module, "*"+fname, ifntyp)
			}
		}
	}
	result := c.NewValue(fn, ftyp)
	if f.Name.Obj != nil {
		f.Name.Obj.Data = result
		f.Name.Obj.Type = ftyp
	}
	return result
}

// promoteStackVar takes a stack variable Value, and promotes it to the heap,
// replacing all uses of the stack-allocated value in the process.
func (stackvar *LLVMValue) promoteStackVar() {
	c := stackvar.compiler
	stackptrval := stackvar.pointer.value

	currblock := c.builder.GetInsertBlock()
	defer c.builder.SetInsertPointAtEnd(currblock)
	c.builder.SetInsertPointBefore(stackptrval)

	typ := stackptrval.Type().ElementType()
	heapptrval := c.createTypeMalloc(typ)
	heapptrval.SetName(stackptrval.Name())
	stackptrval.ReplaceAllUsesWith(heapptrval)
	stackvar.pointer.value = heapptrval
	stackvar.stack = nil
}

// buildFunction takes a function Value, a list of parameters, and a body,
// and generates code for the function.
func (c *compiler) buildFunction(f *LLVMValue, params []*ast.Object, body *ast.BlockStmt) {
	ftyp := f.Type().(*types.Signature)
	llvm_fn := f.LLVMValue()
	entry := llvm.AddBasicBlock(llvm_fn, "entry")
	c.builder.SetInsertPointAtEnd(entry)

	// Bind receiver, arguments and return values to their identifiers/objects.
	// We'll store each parameter on the stack so they're addressable.
	for i, obj := range params {
		if obj.Name != "" {
			value := llvm_fn.Param(i)
			typ := obj.Type.(types.Type)
			stackvalue := c.builder.CreateAlloca(c.types.ToLLVM(typ), obj.Name)
			c.builder.CreateStore(value, stackvalue)
			ptrvalue := c.NewValue(stackvalue, &types.Pointer{Base: typ})
			stackvar := ptrvalue.makePointee()
			stackvar.stack = f
			obj.Data = stackvar
		}
	}

	// Allocate space on the stack for named results.
	for _, obj := range ftyp.Results {
		if obj.Name != "" {
			typ := obj.Type.(types.Type)
			llvmtyp := c.types.ToLLVM(typ)
			stackptr := c.builder.CreateAlloca(llvmtyp, obj.Name)
			c.builder.CreateStore(llvm.ConstNull(llvmtyp), stackptr)
			ptrvalue := c.NewValue(stackptr, &types.Pointer{Base: typ})
			stackvar := ptrvalue.makePointee()
			stackvar.stack = f
			obj.Data = stackvar
		}
	}

	c.functions = append(c.functions, f)
	c.VisitBlockStmt(body, false)
	c.functions = c.functions[0 : len(c.functions)-1]
	last := llvm_fn.LastBasicBlock()
	if in := last.LastInstruction(); in.IsNil() || in.IsATerminatorInst().IsNil() {
		// Assume nil return type, AST should be checked first.
		c.builder.SetInsertPointAtEnd(last)
		c.builder.CreateRetVoid()
	}
}

func (c *compiler) buildPtrRecvFunction(fn llvm.Value) llvm.Value {
	defer c.builder.SetInsertPointAtEnd(c.builder.GetInsertBlock())
	ifname := "*" + fn.Name()
	ifn := c.module.Module.NamedFunction(ifname)
	fntyp := fn.Type().ElementType()
	entry := llvm.AddBasicBlock(ifn, "entry")
	c.builder.SetInsertPointAtEnd(entry)
	args := ifn.Params()
	args[0] = c.builder.CreateLoad(args[0], "recv")
	result := c.builder.CreateCall(fn, args, "")
	if fntyp.ReturnType().TypeKind() == llvm.VoidTypeKind {
		c.builder.CreateRetVoid()
	} else {
		c.builder.CreateRet(result)
	}
	return ifn
}

func (c *compiler) VisitFuncDecl(f *ast.FuncDecl) Value {
	var fn *LLVMValue
	if f.Name.Obj != nil {
		fn = c.Resolve(f.Name.Obj).(*LLVMValue)
	} else {
		fn = c.VisitFuncProtoDecl(f)
	}
	attributes := parseAttributes(f.Doc)
	for _, attr := range attributes {
		attr.Apply(fn)
	}
	if f.Body == nil {
		return fn
	}

	ftyp := fn.Type().(*types.Signature)
	paramObjects := ftyp.Params
	if f.Recv != nil {
		paramObjects = append([]*ast.Object{ftyp.Recv}, paramObjects...)
	}
	c.buildFunction(fn, paramObjects, f.Body)

	if f.Recv != nil {
		// Create a shim function if the receiver is not
		// a pointer type.
		recvtyp := ftyp.Recv.Type.(types.Type)
		if _, ptr := recvtyp.(*types.Pointer); !ptr {
			c.buildPtrRecvFunction(fn.value)
		}
	} else if f.Name.Name == "init" {
		// Is it an 'init' function? Then record it.
		c.initfuncs = append(c.initfuncs, fn)
	}
	return fn
}

// Create a constructor function which initialises a global.
// TODO collapse all global inits into one init function?
func (c *compiler) createGlobals(idents []*ast.Ident, values []ast.Expr, pkg string) {
	globals := make([]*LLVMValue, len(idents))
	for i, ident := range idents {
		if ident.Name != "_" {
			t := ident.Obj.Type.(types.Type)
			llvmtyp := c.types.ToLLVM(t)
			gv := llvm.AddGlobal(c.module.Module, llvmtyp, pkg+"."+ident.Name)
			g := c.NewValue(gv, &types.Pointer{Base: t}).makePointee()
			globals[i] = g
			ident.Obj.Data = g
		}
	}

	if len(values) == 0 {
		for _, g := range globals {
			if g != nil {
				initializer := llvm.ConstNull(g.pointer.value.Type().ElementType())
				g.pointer.value.SetInitializer(initializer)
			}
		}
		return
	} else if len(values) == len(idents) {
		// Non-compound. Initialise global variables with constant
		// values (if any). If all expressions are constant, return
		// immediately after, to avoid the unnecessary function
		// below.
		allconst := true
		for i, expr := range values {
			constinfo := c.types.expr[expr]
			if constinfo.Value != nil {
				if globals[i] != nil {
					if isUntyped(constinfo.Type) {
						constinfo.Type = globals[i].Type()
						c.types.expr[expr] = constinfo
					}
					gv := globals[i].pointer.value
					value := c.VisitExpr(expr)
					gv.SetInitializer(value.LLVMValue())
				}
			} else {
				allconst = false
			}
		}
		if allconst {
			return
		}
	}

	// There are non-const expressions, so we must create an init()
	// function to evaluate the expressions and initialise the globals.
	if block := c.builder.GetInsertBlock(); !block.IsNil() {
		defer c.builder.SetInsertPointAtEnd(block)
	}
	fntype := &types.Signature{}
	llvmfntype := c.types.ToLLVM(fntype).ElementType()
	fn := llvm.AddFunction(c.module.Module, "", llvmfntype)
	entry := llvm.AddBasicBlock(fn, "entry")
	c.builder.SetInsertPointAtEnd(entry)

	if len(values) == 1 && len(idents) > 1 {
		values := c.destructureExpr(values[0])
		for i, v := range values {
			if globals[i] != nil {
				//v := values[i].Convert(ident.Obj.Type.(types.Type))
				gv := globals[i].pointer.value
				gv.SetInitializer(llvm.Undef(gv.Type().ElementType()))
				c.builder.CreateStore(v.LLVMValue(), gv)
			}
		}
	} else {
		for i, expr := range values {
			constval := c.types.expr[expr].Value
			if constval == nil {
				// Must evaluate regardless of whether value is
				// assigned, in event of side-effects.
				v := c.VisitExpr(expr)
				if globals[i] != nil {
					gv := globals[i].pointer.value
					gv.SetInitializer(llvm.Undef(gv.Type().ElementType()))
					c.builder.CreateStore(v.LLVMValue(), gv)
				}
			}
		}
	}

	c.builder.CreateRetVoid()
	fnvalue := c.NewValue(fn, fntype)
	c.varinitfuncs = append(c.varinitfuncs, fnvalue)
}

func (c *compiler) VisitValueSpec(valspec *ast.ValueSpec) {
	// Check if the value-spec has already been visited (referenced
	// before definition visited.)
	if len(valspec.Names) > 0 {
		if _, ok := valspec.Names[0].Obj.Data.(Value); ok {
			return
		}
	}

	pkgname, ispackagelevel := c.pkgmap[valspec.Names[0].Obj]
	if ispackagelevel {
		c.createGlobals(valspec.Names, valspec.Values, pkgname)
		return
	}

	var values []Value
	if len(valspec.Values) == 1 && len(valspec.Names) > 1 {
		values = c.destructureExpr(valspec.Values[0])
	} else if len(valspec.Values) > 0 {
		values = make([]Value, len(valspec.Names))
		for i, x := range valspec.Values {
			c.convertUntyped(x, valspec.Names[i])
			values[i] = c.VisitExpr(x)
		}
	}

	for i, name := range valspec.Names {
		if name.Name == "_" {
			continue
		}

		// The variable should be allocated on the stack if it's
		// declared inside a function.
		//
		// FIXME currently allocating all variables on the heap.
		// Change this to allocate on the stack, and perform
		// escape analysis to determine whether to promote.
		typ := name.Obj.Type.(types.Type)
		llvmtyp := c.types.ToLLVM(typ)
		ptr := c.createTypeMalloc(llvmtyp)
		if values != nil && values[i] != nil {
			// FIXME we need to revisit how aggregate types
			// are initialised/copied/etc. A CreateStore will
			// try to do everything in registers, which is
			// going to hurt when the aggregate is large.
			llvmInit := values[i].Convert(typ).LLVMValue()
			c.builder.CreateStore(llvmInit, ptr)
		}
		stackvar := c.NewValue(ptr, &types.Pointer{Base: typ}).makePointee()
		stackvar.stack = c.functions[len(c.functions)-1]
		name.Obj.Data = stackvar
	}
}

func (c *compiler) VisitGenDecl(decl *ast.GenDecl) {
	switch decl.Tok {
	case token.IMPORT:
		// Already handled in type-checking.
		break
	case token.TYPE:
		// Export runtime type information.
		for _, spec := range decl.Specs {
			typspec := spec.(*ast.TypeSpec)
			typ := typspec.Name.Obj.Type.(types.Type)
			c.types.ToRuntime(typ)
		}
	case token.CONST:
		// Nothing to do; constants are evaluated by go/types.
		// They are converted to LLVM constant values at the
		// site of use.
	case token.VAR:
		// Global variable attributes
		// TODO only parse attributes for package-level var's.
		attributes := parseAttributes(decl.Doc)
		for _, spec := range decl.Specs {
			valspec, _ := spec.(*ast.ValueSpec)
			c.VisitValueSpec(valspec)
			for _, attr := range attributes {
				for _, name := range valspec.Names {
					attr.Apply(name.Obj.Data.(Value))
				}
			}
		}
	}
}

func (c *compiler) VisitDecl(decl ast.Decl) Value {
	// This is temporary. We'll return errors later, rather than panicking.
	if c.Logger != nil {
		c.Logger.Println("Compile declaration:", c.fileset.Position(decl.Pos()))
	}
	defer func() {
		if e := recover(); e != nil {
			elist := new(scanner.ErrorList)
			elist.Add(c.fileset.Position(decl.Pos()), fmt.Sprint(e))
			panic(elist)
		}
	}()

	switch x := decl.(type) {
	case *ast.FuncDecl:
		return c.VisitFuncDecl(x)
	case *ast.GenDecl:
		c.VisitGenDecl(x)
		return nil
	}
	panic(fmt.Sprintf("Unhandled decl (%s) at %s\n",
		reflect.TypeOf(decl),
		c.fileset.Position(decl.Pos())))
}

// vim: set ft=go :
