// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package types

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/internal/typeparams"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// A declInfo describes a package-level const, type, var, or func declaration.
type declInfo struct {
	file      *Scope        // scope of file containing this declaration
	lhs       []*Var        // lhs of n:1 variable declarations, or nil
	vtyp      ast.Expr      // type, or nil (for const and var declarations only)
	init      ast.Expr      // init/orig expression, or nil (for const and var declarations only)
	inherited bool          // if set, the init expression is inherited from a previous constant declaration
	tdecl     *ast.TypeSpec // type declaration, or nil
	fdecl     *ast.FuncDecl // func declaration, or nil

	// The deps field tracks initialization expression dependencies.
	deps map[Object]bool // lazily initialized
}

// hasInitializer reports whether the declared object has an initialization
// expression or function body.
func (d *declInfo) hasInitializer() bool {
	return d.init != nil || d.fdecl != nil && d.fdecl.Body != nil
}

// addDep adds obj to the set of objects d's init expression depends on.
func (d *declInfo) addDep(obj Object) {
	m := d.deps
	if m == nil {
		m = make(map[Object]bool)
		d.deps = m
	}
	m[obj] = true
}

// arityMatch checks that the lhs and rhs of a const or var decl
// have the appropriate number of names and init exprs. For const
// decls, init is the value spec providing the init exprs; for
// var decls, init is nil (the init exprs are in s in this case).
func (check *Checker) arityMatch(s, init *ast.ValueSpec) {
	l := len(s.Names)
	r := len(s.Values)
	if init != nil {
		r = len(init.Values)
	}

	const code = _WrongAssignCount
	switch {
	case init == nil && r == 0:
		// var decl w/o init expr
		if s.Type == nil {
			check.errorf(s, code, "missing type or init expr")
		}
	case l < r:
		if l < len(s.Values) {
			// init exprs from s
			n := s.Values[l]
			check.errorf(n, code, "extra init expr %s", n)
			// TODO(gri) avoid declared but not used error here
		} else {
			// init exprs "inherited"
			check.errorf(s, code, "extra init expr at %s", check.fset.Position(init.Pos()))
			// TODO(gri) avoid declared but not used error here
		}
	case l > r && (init != nil || r != 1):
		n := s.Names[r]
		check.errorf(n, code, "missing init expr for %s", n)
	}
}

func validatedImportPath(path string) (string, error) {
	s, err := strconv.Unquote(path)
	if err != nil {
		return "", err
	}
	if s == "" {
		return "", fmt.Errorf("empty string")
	}
	const illegalChars = `!"#$%&'()*,:;<=>?[\]^{|}` + "`\uFFFD"
	for _, r := range s {
		if !unicode.IsGraphic(r) || unicode.IsSpace(r) || strings.ContainsRune(illegalChars, r) {
			return s, fmt.Errorf("invalid character %#U", r)
		}
	}
	return s, nil
}

// declarePkgObj declares obj in the package scope, records its ident -> obj mapping,
// and updates check.objMap. The object must not be a function or method.
func (check *Checker) declarePkgObj(ident *ast.Ident, obj Object, d *declInfo) {
	assert(ident.Name == obj.Name())

	// spec: "A package-scope or file-scope identifier with name init
	// may only be declared to be a function with this (func()) signature."
	if ident.Name == "init" {
		check.errorf(ident, _InvalidInitDecl, "cannot declare init - must be func")
		return
	}

	// spec: "The main package must have package name main and declare
	// a function main that takes no arguments and returns no value."
	if ident.Name == "main" && check.pkg.name == "main" {
		check.errorf(ident, _InvalidMainDecl, "cannot declare main - must be func")
		return
	}

	check.declare(check.pkg.scope, ident, obj, token.NoPos)
	check.objMap[obj] = d
	obj.setOrder(uint32(len(check.objMap)))
}

// filename returns a filename suitable for debugging output.
func (check *Checker) filename(fileNo int) string {
	file := check.files[fileNo]
	if pos := file.Pos(); pos.IsValid() {
		return check.fset.File(pos).Name()
	}
	return fmt.Sprintf("file[%d]", fileNo)
}

func (check *Checker) importPackage(at positioner, path, dir string) *Package {
	// If we already have a package for the given (path, dir)
	// pair, use it instead of doing a full import.
	// Checker.impMap only caches packages that are marked Complete
	// or fake (dummy packages for failed imports). Incomplete but
	// non-fake packages do require an import to complete them.
	key := importKey{path, dir}
	imp := check.impMap[key]
	if imp != nil {
		return imp
	}

	// no package yet => import it
	if path == "C" && (check.conf.FakeImportC || check.conf.go115UsesCgo) {
		imp = NewPackage("C", "C")
		imp.fake = true // package scope is not populated
		imp.cgo = check.conf.go115UsesCgo
	} else {
		// ordinary import
		var err error
		if importer := check.conf.Importer; importer == nil {
			err = fmt.Errorf("Config.Importer not installed")
		} else if importerFrom, ok := importer.(ImporterFrom); ok {
			imp, err = importerFrom.ImportFrom(path, dir, 0)
			if imp == nil && err == nil {
				err = fmt.Errorf("Config.Importer.ImportFrom(%s, %s, 0) returned nil but no error", path, dir)
			}
		} else {
			imp, err = importer.Import(path)
			if imp == nil && err == nil {
				err = fmt.Errorf("Config.Importer.Import(%s) returned nil but no error", path)
			}
		}
		// make sure we have a valid package name
		// (errors here can only happen through manipulation of packages after creation)
		if err == nil && imp != nil && (imp.name == "_" || imp.name == "") {
			err = fmt.Errorf("invalid package name: %q", imp.name)
			imp = nil // create fake package below
		}
		if err != nil {
			check.errorf(at, _BrokenImport, "could not import %s (%s)", path, err)
			if imp == nil {
				// create a new fake package
				// come up with a sensible package name (heuristic)
				name := path
				if i := len(name); i > 0 && name[i-1] == '/' {
					name = name[:i-1]
				}
				if i := strings.LastIndex(name, "/"); i >= 0 {
					name = name[i+1:]
				}
				imp = NewPackage(path, name)
			}
			// continue to use the package as best as we can
			imp.fake = true // avoid follow-up lookup failures
		}
	}

	// package should be complete or marked fake, but be cautious
	if imp.complete || imp.fake {
		check.impMap[key] = imp
		// Once we've formatted an error message once, keep the pkgPathMap
		// up-to-date on subsequent imports.
		if check.pkgPathMap != nil {
			check.markImports(imp)
		}
		return imp
	}

	// something went wrong (importer may have returned incomplete package without error)
	return nil
}

// collectObjects collects all file and package objects and inserts them
// into their respective scopes. It also performs imports and associates
// methods with receiver base type names.
func (check *Checker) collectObjects() {
	pkg := check.pkg

	// pkgImports is the set of packages already imported by any package file seen
	// so far. Used to avoid duplicate entries in pkg.imports. Allocate and populate
	// it (pkg.imports may not be empty if we are checking test files incrementally).
	// Note that pkgImports is keyed by package (and thus package path), not by an
	// importKey value. Two different importKey values may map to the same package
	// which is why we cannot use the check.impMap here.
	var pkgImports = make(map[*Package]bool)
	for _, imp := range pkg.imports {
		pkgImports[imp] = true
	}

	type methodInfo struct {
		obj  *Func      // method
		ptr  bool       // true if pointer receiver
		recv *ast.Ident // receiver type name
	}
	var methods []methodInfo // collected methods with valid receivers and non-blank _ names
	var fileScopes []*Scope
	for fileNo, file := range check.files {
		// The package identifier denotes the current package,
		// but there is no corresponding package object.
		check.recordDef(file.Name, nil)

		// Use the actual source file extent rather than *ast.File extent since the
		// latter doesn't include comments which appear at the start or end of the file.
		// Be conservative and use the *ast.File extent if we don't have a *token.File.
		pos, end := file.Pos(), file.End()
		if f := check.fset.File(file.Pos()); f != nil {
			pos, end = token.Pos(f.Base()), token.Pos(f.Base()+f.Size())
		}
		fileScope := NewScope(check.pkg.scope, pos, end, check.filename(fileNo))
		fileScopes = append(fileScopes, fileScope)
		check.recordScope(file, fileScope)

		// determine file directory, necessary to resolve imports
		// FileName may be "" (typically for tests) in which case
		// we get "." as the directory which is what we would want.
		fileDir := dir(check.fset.Position(file.Name.Pos()).Filename)

		check.walkDecls(file.Decls, func(d decl) {
			switch d := d.(type) {
			case importDecl:
				// import package
				path, err := validatedImportPath(d.spec.Path.Value)
				if err != nil {
					check.errorf(d.spec.Path, _BadImportPath, "invalid import path (%s)", err)
					return
				}

				imp := check.importPackage(d.spec.Path, path, fileDir)
				if imp == nil {
					return
				}

				// local name overrides imported package name
				name := imp.name
				if d.spec.Name != nil {
					name = d.spec.Name.Name
					if path == "C" {
						// match cmd/compile (not prescribed by spec)
						check.errorf(d.spec.Name, _ImportCRenamed, `cannot rename import "C"`)
						return
					}
				}

				if name == "init" {
					check.errorf(d.spec, _InvalidInitDecl, "cannot import package as init - init must be a func")
					return
				}

				// add package to list of explicit imports
				// (this functionality is provided as a convenience
				// for clients; it is not needed for type-checking)
				if !pkgImports[imp] {
					pkgImports[imp] = true
					pkg.imports = append(pkg.imports, imp)
				}

				pkgName := NewPkgName(d.spec.Pos(), pkg, name, imp)
				if d.spec.Name != nil {
					// in a dot-import, the dot represents the package
					check.recordDef(d.spec.Name, pkgName)
				} else {
					check.recordImplicit(d.spec, pkgName)
				}

				if path == "C" {
					// match cmd/compile (not prescribed by spec)
					pkgName.used = true
				}

				// add import to file scope
				check.imports = append(check.imports, pkgName)
				if name == "." {
					// dot-import
					if check.dotImportMap == nil {
						check.dotImportMap = make(map[dotImportKey]*PkgName)
					}
					// merge imported scope with file scope
					for name, obj := range imp.scope.elems {
						// Note: Avoid eager resolve(name, obj) here, so we only
						// resolve dot-imported objects as needed.

						// A package scope may contain non-exported objects,
						// do not import them!
						if token.IsExported(name) {
							// declare dot-imported object
							// (Do not use check.declare because it modifies the object
							// via Object.setScopePos, which leads to a race condition;
							// the object may be imported into more than one file scope
							// concurrently. See issue #32154.)
							if alt := fileScope.Lookup(name); alt != nil {
								check.errorf(d.spec.Name, _DuplicateDecl, "%s redeclared in this block", alt.Name())
								check.reportAltDecl(alt)
							} else {
								fileScope.insert(name, obj)
								check.dotImportMap[dotImportKey{fileScope, name}] = pkgName
							}
						}
					}
				} else {
					// declare imported package object in file scope
					// (no need to provide s.Name since we called check.recordDef earlier)
					check.declare(fileScope, nil, pkgName, token.NoPos)
				}
			case constDecl:
				// declare all constants
				for i, name := range d.spec.Names {
					obj := NewConst(name.Pos(), pkg, name.Name, nil, constant.MakeInt64(int64(d.iota)))

					var init ast.Expr
					if i < len(d.init) {
						init = d.init[i]
					}

					d := &declInfo{file: fileScope, vtyp: d.typ, init: init, inherited: d.inherited}
					check.declarePkgObj(name, obj, d)
				}

			case varDecl:
				lhs := make([]*Var, len(d.spec.Names))
				// If there's exactly one rhs initializer, use
				// the same declInfo d1 for all lhs variables
				// so that each lhs variable depends on the same
				// rhs initializer (n:1 var declaration).
				var d1 *declInfo
				if len(d.spec.Values) == 1 {
					// The lhs elements are only set up after the for loop below,
					// but that's ok because declareVar only collects the declInfo
					// for a later phase.
					d1 = &declInfo{file: fileScope, lhs: lhs, vtyp: d.spec.Type, init: d.spec.Values[0]}
				}

				// declare all variables
				for i, name := range d.spec.Names {
					obj := NewVar(name.Pos(), pkg, name.Name, nil)
					lhs[i] = obj

					di := d1
					if di == nil {
						// individual assignments
						var init ast.Expr
						if i < len(d.spec.Values) {
							init = d.spec.Values[i]
						}
						di = &declInfo{file: fileScope, vtyp: d.spec.Type, init: init}
					}

					check.declarePkgObj(name, obj, di)
				}
			case typeDecl:
				if d.spec.TParams.NumFields() != 0 && !check.allowVersion(pkg, 1, 18) {
					check.softErrorf(d.spec.TParams.List[0], _Todo, "type parameters require go1.18 or later")
				}
				obj := NewTypeName(d.spec.Name.Pos(), pkg, d.spec.Name.Name, nil)
				check.declarePkgObj(d.spec.Name, obj, &declInfo{file: fileScope, tdecl: d.spec})
			case funcDecl:
				name := d.decl.Name.Name
				obj := NewFunc(d.decl.Name.Pos(), pkg, name, nil)
				hasTParamError := false // avoid duplicate type parameter errors
				if d.decl.Recv.NumFields() == 0 {
					// regular function
					if d.decl.Recv != nil {
						check.error(d.decl.Recv, _BadRecv, "method is missing receiver")
						// treat as function
					}
					if name == "init" || (name == "main" && check.pkg.name == "main") {
						code := _InvalidInitDecl
						if name == "main" {
							code = _InvalidMainDecl
						}
						if d.decl.Type.TParams.NumFields() != 0 {
							check.softErrorf(d.decl.Type.TParams.List[0], code, "func %s must have no type parameters", name)
							hasTParamError = true
						}
						if t := d.decl.Type; t.Params.NumFields() != 0 || t.Results != nil {
							// TODO(rFindley) Should this be a hard error?
							check.softErrorf(d.decl, code, "func %s must have no arguments and no return values", name)
						}
					}
					if name == "init" {
						// don't declare init functions in the package scope - they are invisible
						obj.parent = pkg.scope
						check.recordDef(d.decl.Name, obj)
						// init functions must have a body
						if d.decl.Body == nil {
							// TODO(gri) make this error message consistent with the others above
							check.softErrorf(obj, _MissingInitBody, "missing function body")
						}
					} else {
						check.declare(pkg.scope, d.decl.Name, obj, token.NoPos)
					}
				} else {
					// method

					// TODO(rFindley) earlier versions of this code checked that methods
					//                have no type parameters, but this is checked later
					//                when type checking the function type. Confirm that
					//                we don't need to check tparams here.

					ptr, recv, _ := check.unpackRecv(d.decl.Recv.List[0].Type, false)
					// (Methods with invalid receiver cannot be associated to a type, and
					// methods with blank _ names are never found; no need to collect any
					// of them. They will still be type-checked with all the other functions.)
					if recv != nil && name != "_" {
						methods = append(methods, methodInfo{obj, ptr, recv})
					}
					check.recordDef(d.decl.Name, obj)
				}
				if d.decl.Type.TParams.NumFields() != 0 && !check.allowVersion(pkg, 1, 18) && !hasTParamError {
					check.softErrorf(d.decl.Type.TParams.List[0], _Todo, "type parameters require go1.18 or later")
				}
				info := &declInfo{file: fileScope, fdecl: d.decl}
				// Methods are not package-level objects but we still track them in the
				// object map so that we can handle them like regular functions (if the
				// receiver is invalid); also we need their fdecl info when associating
				// them with their receiver base type, below.
				check.objMap[obj] = info
				obj.setOrder(uint32(len(check.objMap)))
			}
		})
	}

	// verify that objects in package and file scopes have different names
	for _, scope := range fileScopes {
		for name, obj := range scope.elems {
			if alt := pkg.scope.Lookup(name); alt != nil {
				obj = resolve(name, obj)
				if pkg, ok := obj.(*PkgName); ok {
					check.errorf(alt, _DuplicateDecl, "%s already declared through import of %s", alt.Name(), pkg.Imported())
					check.reportAltDecl(pkg)
				} else {
					check.errorf(alt, _DuplicateDecl, "%s already declared through dot-import of %s", alt.Name(), obj.Pkg())
					// TODO(gri) dot-imported objects don't have a position; reportAltDecl won't print anything
					check.reportAltDecl(obj)
				}
			}
		}
	}

	// Now that we have all package scope objects and all methods,
	// associate methods with receiver base type name where possible.
	// Ignore methods that have an invalid receiver. They will be
	// type-checked later, with regular functions.
	if methods == nil {
		return // nothing to do
	}
	check.methods = make(map[*TypeName][]*Func)
	for i := range methods {
		m := &methods[i]
		// Determine the receiver base type and associate m with it.
		ptr, base := check.resolveBaseTypeName(m.ptr, m.recv)
		if base != nil {
			m.obj.hasPtrRecv = ptr
			check.methods[base] = append(check.methods[base], m.obj)
		}
	}
}

// unpackRecv unpacks a receiver type and returns its components: ptr indicates whether
// rtyp is a pointer receiver, rname is the receiver type name, and tparams are its
// type parameters, if any. The type parameters are only unpacked if unpackParams is
// set. If rname is nil, the receiver is unusable (i.e., the source has a bug which we
// cannot easily work around).
func (check *Checker) unpackRecv(rtyp ast.Expr, unpackParams bool) (ptr bool, rname *ast.Ident, tparams []*ast.Ident) {
L: // unpack receiver type
	// This accepts invalid receivers such as ***T and does not
	// work for other invalid receivers, but we don't care. The
	// validity of receiver expressions is checked elsewhere.
	for {
		switch t := rtyp.(type) {
		case *ast.ParenExpr:
			rtyp = t.X
		case *ast.StarExpr:
			ptr = true
			rtyp = t.X
		default:
			break L
		}
	}

	// unpack type parameters, if any
	switch rtyp.(type) {
	case *ast.IndexExpr, *ast.MultiIndexExpr:
		ix := typeparams.UnpackIndexExpr(rtyp)
		rtyp = ix.X
		if unpackParams {
			for _, arg := range ix.Indices {
				var par *ast.Ident
				switch arg := arg.(type) {
				case *ast.Ident:
					par = arg
				case *ast.BadExpr:
					// ignore - error already reported by parser
				case nil:
					check.invalidAST(ix.Orig, "parameterized receiver contains nil parameters")
				default:
					check.errorf(arg, _Todo, "receiver type parameter %s must be an identifier", arg)
				}
				if par == nil {
					par = &ast.Ident{NamePos: arg.Pos(), Name: "_"}
				}
				tparams = append(tparams, par)
			}
		}
	}

	// unpack receiver name
	if name, _ := rtyp.(*ast.Ident); name != nil {
		rname = name
	}

	return
}

// resolveBaseTypeName returns the non-alias base type name for typ, and whether
// there was a pointer indirection to get to it. The base type name must be declared
// in package scope, and there can be at most one pointer indirection. If no such type
// name exists, the returned base is nil.
func (check *Checker) resolveBaseTypeName(seenPtr bool, name *ast.Ident) (ptr bool, base *TypeName) {
	// Algorithm: Starting from a type expression, which may be a name,
	// we follow that type through alias declarations until we reach a
	// non-alias type name. If we encounter anything but pointer types or
	// parentheses we're done. If we encounter more than one pointer type
	// we're done.
	ptr = seenPtr
	var seen map[*TypeName]bool
	var typ ast.Expr = name
	for {
		typ = unparen(typ)

		// check if we have a pointer type
		if pexpr, _ := typ.(*ast.StarExpr); pexpr != nil {
			// if we've already seen a pointer, we're done
			if ptr {
				return false, nil
			}
			ptr = true
			typ = unparen(pexpr.X) // continue with pointer base type
		}

		// typ must be a name
		name, _ := typ.(*ast.Ident)
		if name == nil {
			return false, nil
		}

		// name must denote an object found in the current package scope
		// (note that dot-imported objects are not in the package scope!)
		obj := check.pkg.scope.Lookup(name.Name)
		if obj == nil {
			return false, nil
		}

		// the object must be a type name...
		tname, _ := obj.(*TypeName)
		if tname == nil {
			return false, nil
		}

		// ... which we have not seen before
		if seen[tname] {
			return false, nil
		}

		// we're done if tdecl defined tname as a new type
		// (rather than an alias)
		tdecl := check.objMap[tname].tdecl // must exist for objects in package scope
		if !tdecl.Assign.IsValid() {
			return ptr, tname
		}

		// otherwise, continue resolving
		typ = tdecl.Type
		if seen == nil {
			seen = make(map[*TypeName]bool)
		}
		seen[tname] = true
	}
}

// packageObjects typechecks all package objects, but not function bodies.
func (check *Checker) packageObjects() {
	// process package objects in source order for reproducible results
	objList := make([]Object, len(check.objMap))
	i := 0
	for obj := range check.objMap {
		objList[i] = obj
		i++
	}
	sort.Sort(inSourceOrder(objList))

	// add new methods to already type-checked types (from a prior Checker.Files call)
	for _, obj := range objList {
		if obj, _ := obj.(*TypeName); obj != nil && obj.typ != nil {
			check.collectMethods(obj)
		}
	}

	// We process non-alias declarations first, in order to avoid situations where
	// the type of an alias declaration is needed before it is available. In general
	// this is still not enough, as it is possible to create sufficiently convoluted
	// recursive type definitions that will cause a type alias to be needed before it
	// is available (see issue #25838 for examples).
	// As an aside, the cmd/compiler suffers from the same problem (#25838).
	var aliasList []*TypeName
	// phase 1
	for _, obj := range objList {
		// If we have a type alias, collect it for the 2nd phase.
		if tname, _ := obj.(*TypeName); tname != nil && check.objMap[tname].tdecl.Assign.IsValid() {
			aliasList = append(aliasList, tname)
			continue
		}

		check.objDecl(obj, nil)
	}
	// phase 2
	for _, obj := range aliasList {
		check.objDecl(obj, nil)
	}

	// At this point we may have a non-empty check.methods map; this means that not all
	// entries were deleted at the end of typeDecl because the respective receiver base
	// types were not found. In that case, an error was reported when declaring those
	// methods. We can now safely discard this map.
	check.methods = nil
}

// inSourceOrder implements the sort.Sort interface.
type inSourceOrder []Object

func (a inSourceOrder) Len() int           { return len(a) }
func (a inSourceOrder) Less(i, j int) bool { return a[i].order() < a[j].order() }
func (a inSourceOrder) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

// unusedImports checks for unused imports.
func (check *Checker) unusedImports() {
	// if function bodies are not checked, packages' uses are likely missing - don't check
	if check.conf.IgnoreFuncBodies {
		return
	}

	// spec: "It is illegal (...) to directly import a package without referring to
	// any of its exported identifiers. To import a package solely for its side-effects
	// (initialization), use the blank identifier as explicit package name."

	for _, obj := range check.imports {
		if !obj.used && obj.name != "_" {
			check.errorUnusedPkg(obj)
		}
	}
}

func (check *Checker) errorUnusedPkg(obj *PkgName) {
	// If the package was imported with a name other than the final
	// import path element, show it explicitly in the error message.
	// Note that this handles both renamed imports and imports of
	// packages containing unconventional package declarations.
	// Note that this uses / always, even on Windows, because Go import
	// paths always use forward slashes.
	path := obj.imported.path
	elem := path
	if i := strings.LastIndex(elem, "/"); i >= 0 {
		elem = elem[i+1:]
	}
	if obj.name == "" || obj.name == "." || obj.name == elem {
		check.softErrorf(obj, _UnusedImport, "%q imported but not used", path)
	} else {
		check.softErrorf(obj, _UnusedImport, "%q imported but not used as %s", path, obj.name)
	}
}

// dir makes a good-faith attempt to return the directory
// portion of path. If path is empty, the result is ".".
// (Per the go/build package dependency tests, we cannot import
// path/filepath and simply use filepath.Dir.)
func dir(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i > 0 {
		return path[:i]
	}
	// i <= 0
	return "."
}
