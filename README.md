# linkname-gen

`linkname-gen` is a tool to automate the creation of vendor-compatible go:linkname statements.
It is designed to be used with go:generate.

Given a remote symbol and a local function definition to bind this symbol to, linkname-gen will create a new self-contained Go source file implementing the right go:linkname statement with the necessary imports & boilerplate.

The file is created in the same package and directory as the package that defines the go:generate clause.

For example, given this snippet,  
```Go
package main

//go:generate linkname-gen -symbol "github.com/gogo/protobuf/protoc-gen-gogo/generator.(*Generator).goTag" -def "func goTag(*generator.Generator, *generator.Descriptor, *descriptor.FieldDescriptorProto, string) string"
```

a sym_linkname.go file with the following content will be generated:  
```Go
package main

import (
	_ "unsafe"

	"github.com/gogo/protobuf/protoc-gen-gogo/descriptor"
	"github.com/gogo/protobuf/protoc-gen-gogo/generator"
)

//go:linkname goTag github.com/myorg/myproject/vendor/github.com/gogo/protobuf/protoc-gen-gogo/generator
func goTag(*generator.Generator, *generator.Descriptor, *descriptor.FieldDescriptorProto, string) string
```

With no arguments, it processes the package in the current directory.
Otherwise, the arguments must name a single directory holding a Go package or a set of Go source files that represent a single Go package.

The default output file is `sym_linkname.go`, it can be overridden with the -output flag.
