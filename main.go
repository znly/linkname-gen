// Copyright 2014 The Go Authors. All rights reserved.
// Copyright 2017 Zenly <hello@zen.ly>
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// linkname-gen is a tool to automate the creation of vendor-compatible
// go:linkname statements.
// It is designed to be used with go:generate.
//
// Given a remote symbol and a local function definition to bind this symbol to,
// linkname-gen will create a new self-contained Go source file implementing
// the right go:linkname statement with the necessary imports & boilerplate.
//
// The file is created in the same package and directory as the package that
// defines the go:generate clause.
//
// For example, given this snippet,
//
//	package main
//
//	//go:generate linkname-gen -symbol "github.com/gogo/protobuf/protoc-gen-gogo/generator.(*Generator).goTag" -def "func goTag(*generator.Generator, *generator.Descriptor, *descriptor.FieldDescriptorProto, string) string"
//
// a sym_linkname.go file with the following content will be created:
//
//	package main
//
//	import (
//		_ "unsafe"
//
//		"github.com/gogo/protobuf/protoc-gen-gogo/descriptor"
//		"github.com/gogo/protobuf/protoc-gen-gogo/generator"
//	)
//
//	//go:linkname goTag github.com/gogo/protobuf/protoc-gen-gogo/generator
//	func goTag(*generator.Generator, *generator.Descriptor, *descriptor.FieldDescriptorProto, string) string
//
// With no arguments, it processes the package in the current directory.
// Otherwise, the arguments must name a single directory holding a Go package
// or a set of Go source files that represent a single Go package.
//
// The default output file is sym_linkname.go, it can be overridden with
// the -output flag.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/tools/imports"
)

// -----------------------------------------------------------------------------

var (
	_symbol = flag.String(
		"symbol", "", "name of the symbol to be bound to",
	)
	_def = flag.String(
		"def", "", "definition of the function to be bound to -symbol",
	)
	_output = flag.String(
		"output", "", "output file name; default srcdir/<type>_string.go",
	)
)

// -----------------------------------------------------------------------------

// Usage is a replacement usage function for the flags package.
func Usage() {
	fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "\tlinkname-gen [flags] -symbol S -def F [directory]\n")
	fmt.Fprintf(os.Stderr, "\tlinkname-gen [flags] -symbol S -def F files... # Must be a single package\n")
	fmt.Fprintf(os.Stderr, "For more information, see:\n")
	fmt.Fprintf(os.Stderr, "\thttp://godoc.org/github.com/znly/linkname-gen\n")
	fmt.Fprintf(os.Stderr, "Flags:\n")
	flag.PrintDefaults()
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("linkname-gen: ")
	flag.Usage = Usage
	flag.Parse()
	if len(*_symbol) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	// We accept either one directory or a list of files. Which do we have?
	args := flag.Args()
	if len(args) == 0 {
		// Default: process whole package in current directory.
		args = []string{"."}
	}

	// Parse the package once.
	var (
		dir string
		g   Generator
	)
	if len(args) == 1 && isDirectory(args[0]) {
		dir = args[0]
		g.parsePackageDir(args[0])
	} else {
		dir = filepath.Dir(args[0])
		g.parsePackageFiles(args)
	}

	// Print the header and package clause.
	g.Printf("// Code generated by \"linkname-gen %s\"; DO NOT EDIT.\n", strings.Join(os.Args[1:], " "))
	g.Printf("\n")
	g.Printf("package %s", g.pkg.name)
	g.Printf("\n")

	deps, err := exec.Command(
		"go", "list", "-f", `'{{join .Imports "\n"}}'`, dir,
	).Output()
	if err != nil {
		log.Fatal(err)
	}
	symPath, symPkg := path.Split(*_symbol)
	sym := symPath + strings.Split(symPkg, ".")[0]
	var symDep string
	for _, dep := range bytes.Split(deps, []byte("\n")) {
		if strings.HasSuffix(string(dep), sym) {
			symDep = string(dep)
			break
		}
	}
	if len(symDep) <= 0 {
		log.Fatalf("no such symbol: `%s`", *_symbol)
	}
	funcName := strings.Split(strings.Split(*_def, " ")[1], "(")[0]

	g.Printf("import _ \"%s\"\n", "unsafe")
	g.Printf("import \"%s\"\n", sym)
	g.Printf("\n")
	g.Printf("//go:linkname %s %s\n", funcName, symDep)
	g.Printf("%s\n", *_def)

	// Format the output.
	src := g.format()

	// Write to file.
	outputName := *_output
	if outputName == "" {
		baseName := fmt.Sprintf("%s_linkname.go", "sym")
		outputName = filepath.Join(dir, strings.ToLower(baseName))
	}
	err = ioutil.WriteFile(outputName, src, 0644)
	if err != nil {
		log.Fatalf("writing output: %s", err)
	}

	// Write assembly stub.
	err = ioutil.WriteFile(filepath.Join(dir, "linkname.s"), []byte(""), 0644)
	if err != nil {
		log.Fatalf("writing assembly stub: %s", err)
	}
}

// -----------------------------------------------------------------------------

// isDirectory reports whether the named file is a directory.
func isDirectory(name string) bool {
	info, err := os.Stat(name)
	if err != nil {
		log.Fatal(err)
	}
	return info.IsDir()
}

// Generator holds the state of the analysis. Primarily used to buffer
// the output for format.Source.
type Generator struct {
	buf bytes.Buffer // Accumulated output.
	pkg *Package     // Package we are scanning.
}

func (g *Generator) Printf(format string, args ...interface{}) {
	fmt.Fprintf(&g.buf, format, args...)
}

// File holds a single parsed file and associated data.
type File struct {
	pkg  *Package  // Package to which this file belongs.
	file *ast.File // Parsed AST.
	// These fields are reset for each type being generated.
	symbol string // Name of the constant type.
}

type Package struct {
	dir      string
	name     string
	defs     map[*ast.Ident]types.Object
	typesPkg *types.Package
	files    []*File
}

// parsePackageDir parses the package residing in the directory.
func (g *Generator) parsePackageDir(directory string) {
	pkg, err := build.Default.ImportDir(directory, 0)
	if err != nil {
		log.Fatalf("cannot process directory %s: %s", directory, err)
	}
	var names []string
	names = append(names, pkg.GoFiles...)
	names = append(names, pkg.CgoFiles...)
	// TODO: Need to think about constants in test files. Maybe write type_string_test.go
	// in a separate pass? For later.
	// names = append(names, pkg.TestGoFiles...) // These are also in the "foo" package.
	names = append(names, pkg.SFiles...)
	names = prefixDirectory(directory, names)
	g.parsePackage(directory, names, nil)
}

// parsePackageFiles parses the package occupying the named files.
func (g *Generator) parsePackageFiles(names []string) {
	g.parsePackage(".", names, nil)
}

// prefixDirectory places the directory name on the beginning of each name in the list.
func prefixDirectory(directory string, names []string) []string {
	if directory == "." {
		return names
	}
	ret := make([]string, len(names))
	for i, name := range names {
		ret[i] = filepath.Join(directory, name)
	}
	return ret
}

// parsePackage analyzes the single package constructed from the named files.
// If text is non-nil, it is a string to be used instead of the content of the file,
// to be used for testing. parsePackage exits if there is an error.
func (g *Generator) parsePackage(directory string, names []string, text interface{}) {
	var files []*File
	var astFiles []*ast.File
	g.pkg = new(Package)
	fs := token.NewFileSet()
	for _, name := range names {
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		parsedFile, err := parser.ParseFile(fs, name, text, 0)
		if err != nil {
			log.Fatalf("parsing package: %s: %s", name, err)
		}
		astFiles = append(astFiles, parsedFile)
		files = append(files, &File{
			file: parsedFile,
			pkg:  g.pkg,
		})
	}
	if len(astFiles) == 0 {
		log.Fatalf("%s: no buildable Go files", directory)
	}
	g.pkg.name = astFiles[0].Name.Name
	g.pkg.files = files
	g.pkg.dir = directory
	// Type check the package.
	g.pkg.check(fs, astFiles)
}

// check type-checks the package. The package must be OK to proceed.
func (pkg *Package) check(fs *token.FileSet, astFiles []*ast.File) {
	pkg.defs = make(map[*ast.Ident]types.Object)
	config := types.Config{Importer: importer.Default(), FakeImportC: true}
	info := &types.Info{
		Defs: pkg.defs,
	}
	typesPkg, err := config.Check(pkg.dir, fs, astFiles, info)
	if err != nil {
		log.Fatalf("checking package: %s", err)
	}
	pkg.typesPkg = typesPkg
}

// format returns the gofmt-ed contents of the Generator's buffer.
func (g *Generator) format() []byte {
	src, err := imports.Process("", g.buf.Bytes(), nil)
	//src, err := format.Source(g.buf.Bytes())
	if err != nil {
		// Should never happen, but can arise when developing this code.
		// The user can compile the output to see the error.
		log.Printf("warning: internal error: invalid Go generated: %s", err)
		log.Printf("warning: compile the package to analyze the error")
		return g.buf.Bytes()
	}
	return src
}
