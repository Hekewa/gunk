package generate

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"io/ioutil"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/golang/protobuf/proto"
	desc "github.com/golang/protobuf/protoc-gen-go/descriptor"
	plugin "github.com/golang/protobuf/protoc-gen-go/plugin"
	"golang.org/x/tools/go/packages"
	"google.golang.org/genproto/googleapis/api/annotations"

	"github.com/gunk/gunk/loader"
)

// Generate runs protobuf generators on the specified Gunk packages, writing the
// output files in the same directories.
func Generate(dir string, patterns ...string) error {
	g := &Generator{
		dir:  dir,
		fset: token.NewFileSet(),
		tconfig: &types.Config{
			DisableUnusedImportCheck: true,
		},
		info: &types.Info{
			Types: make(map[ast.Expr]types.TypeAndValue),
			Defs:  make(map[*ast.Ident]types.Object),
			Uses:  make(map[*ast.Ident]types.Object),
		},

		gunkPkgs:  make(map[string]*loader.GunkPackage),
		toGen:     make(map[string]map[string]bool),
		allProto:  make(map[string]*desc.FileDescriptorProto),
		origPaths: make(map[string]string),
	}
	g.tconfig.Importer = g

	pkgs, err := loader.Load(g.dir, g.fset, patterns...)
	if err != nil {
		return err
	}
	if len(pkgs) == 0 {
		return fmt.Errorf("no Gunk packages to generate")
	}

	// Record the loaded packages
	for _, pkg := range pkgs {
		g.gunkPkgs[pkg.PkgPath] = pkg
		for i, name := range pkg.GunkNames {
			g.origPaths[name] = pkg.GunkFiles[i]
		}
	}

	// Translate the packages from Gunk to Proto.
	for _, pkg := range pkgs {
		if err := g.addPkg(pkg.PkgPath); err != nil {
			return err
		}
		if err := g.translatePkg(pkg.PkgPath); err != nil {
			return err
		}
	}
	if err := g.loadProtoDeps(); err != nil {
		return err
	}

	// Finally, run the code generators.
	for _, pkg := range pkgs {
		if err := g.GeneratePkg(pkg.PkgPath); err != nil {
			return err
		}
	}
	return nil
}

type Generator struct {
	dir string // if empty, uses the current directory

	curPkg *loader.GunkPackage // current package being translated or generated
	gfile  *ast.File
	pfile  *desc.FileDescriptorProto

	fset    *token.FileSet
	tconfig *types.Config
	info    *types.Info

	// Maps from package import path to package information.
	gunkPkgs map[string]*loader.GunkPackage

	toGen     map[string]map[string]bool
	allProto  map[string]*desc.FileDescriptorProto
	origPaths map[string]string

	messageIndex int32
	serviceIndex int32
	enumIndex    int32
}

// GeneratePkg runs the proto files resulting from translating gunk packages
// through a code generator, such as protoc-gen-go to generate Go packages.
//
// Generated files are written to the same directory, next to the source gunk
// files.
//
// It is fine to pass the plugin.CodeGeneratorRequest to every protoc generator
// unaltered; this is what protoc does when calling out to the generators and
// the generators should already handle the case where they have nothing to do.
func (g *Generator) GeneratePkg(path string) error {
	req := g.requestForPkg(path)
	if err := g.generatePluginGo(*req); err != nil {
		return fmt.Errorf("error generating plugin go: %v", err)
	}
	if err := g.generatePluginGrpcGateway(*req); err != nil {
		return fmt.Errorf("error generating plugin grpc-gateway: %v", err)
	}
	return nil
}

func (g *Generator) generatePluginGo(req plugin.CodeGeneratorRequest) error {
	req.Parameter = proto.String("plugins=grpc")
	bs, err := proto.Marshal(&req)
	if err != nil {
		return err
	}

	cmd := exec.Command("protoc-gen-go")
	cmd.Stdin = bytes.NewReader(bs)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("error executing protoc-gen-go: %s, %v", out, err)
	}
	var resp plugin.CodeGeneratorResponse
	if err := proto.Unmarshal(out, &resp); err != nil {
		return err
	}
	for _, rf := range resp.File {
		// to turn foo.gunk.pb.go into foo.pb.go
		inPath := strings.Replace(*rf.Name, ".pb.go", "", 1)
		outPath := g.origPaths[inPath]
		outPath = strings.Replace(outPath, ".gunk", ".pb.go", 1)
		data := []byte(*rf.Content)
		if err := ioutil.WriteFile(outPath, data, 0644); err != nil {
			return fmt.Errorf("unable to write to file %q: %v", outPath, err)
		}
	}
	return nil
}

func (g *Generator) generatePluginGrpcGateway(req plugin.CodeGeneratorRequest) error {
	bs, err := proto.Marshal(&req)
	if err != nil {
		return err
	}

	cmd := exec.Command("protoc-gen-grpc-gateway")
	cmd.Stdin = bytes.NewReader(bs)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("error executing protoc-gen-grpc-gateway: %s, %v", out, err)
	}
	var resp plugin.CodeGeneratorResponse
	if err := proto.Unmarshal(out, &resp); err != nil {
		return err
	}
	if rerr := resp.GetError(); rerr != "" {
		return fmt.Errorf("error executing protoc-gen-grpc-gateway: %v", rerr)
	}
	for _, rf := range resp.File {
		// to turn foo.gunk.pb.gw.go into foo.pb.gw.go
		inPath := strings.Replace(*rf.Name, ".pb.gw.go", ".gunk", 1)
		outPath := g.origPaths[inPath]
		outPath = strings.Replace(outPath, ".gunk", ".pb.gw.go", 1)
		data := []byte(*rf.Content)
		if err := ioutil.WriteFile(outPath, data, 0644); err != nil {
			return fmt.Errorf("unable to write to file %q: %v", outPath, err)
		}
	}
	return nil
}

func (g *Generator) requestForPkg(path string) *plugin.CodeGeneratorRequest {
	req := &plugin.CodeGeneratorRequest{}
	for file := range g.toGen[path] {
		req.FileToGenerate = append(req.FileToGenerate, file)
	}
	for _, pfile := range g.allProto {
		req.ProtoFile = append(req.ProtoFile, pfile)
	}

	// Sort all the files by name to get deterministic output. For example,
	// the first file in each package gets an extra package godoc from
	// protoc-gen-go. And protoc-gen-grpc-gateway cares about the order of
	// the ProtoFile list.
	sort.Strings(req.FileToGenerate)
	sort.Slice(req.ProtoFile, func(i, j int) bool {
		return *req.ProtoFile[i].Name < *req.ProtoFile[j].Name
	})

	return req
}

// translatePkg translates all the gunk files in a gunk package to the
// proto language. All the files within the package, including all the
// files for its transitive dependencies, must already be loaded via
// addPkg.
func (g *Generator) translatePkg(pkgPath string) error {
	gpkg := g.gunkPkgs[pkgPath]
	g.curPkg = gpkg

	for i, fpath := range gpkg.GunkNames {
		if err := g.translateFile(pkgPath, fpath, gpkg.GunkSyntax[i]); err != nil {
			return err
		}
	}

	for i, name := range gpkg.GunkNames {
		gfile := gpkg.GunkSyntax[i]
		pfile := g.allProto[name]
		for _, oname := range gpkg.GunkNames {
			if name != oname {
				pfile.Dependency = append(pfile.Dependency, oname)
			}
		}
		for _, imp := range gfile.Imports {
			if imp.Name != nil && imp.Name.Name == "_" {
				continue
			}
			opath, _ := strconv.Unquote(imp.Path.Value)
			pfile.Dependency = append(pfile.Dependency, g.gunkPkgs[opath].GunkNames...)
		}
	}
	return nil
}

// translateFile translates a single gunk file to a proto file.
func (g *Generator) translateFile(pkgPath, fpath string, file *ast.File) error {
	g.messageIndex = 0
	g.serviceIndex = 0
	g.enumIndex = 0
	g.toGen[pkgPath][fpath] = true
	if _, ok := g.allProto[fpath]; ok {
		// already translated
		return nil
	}
	gpkg := g.gunkPkgs[pkgPath]
	g.gfile = file
	g.pfile = &desc.FileDescriptorProto{
		Syntax:  proto.String("proto3"),
		Name:    proto.String(fpath),
		Package: proto.String(gpkg.ProtoName),
		Options: &desc.FileOptions{
			GoPackage: proto.String(gpkg.Name),
			// TODO: Add other package options
		},
	}
	g.addDoc(file.Doc, nil, packagePath)
	for _, decl := range file.Decls {
		if err := g.translateDecl(decl); err != nil {
			return err
		}
	}
	g.allProto[fpath] = g.pfile
	return nil
}

// translateDecl translates a top-level declaration in a gunk file. It
// only acts on type declarations; struct types become proto messages,
// interfaces become services, and basic integer types become enums.
func (g *Generator) translateDecl(decl ast.Decl) error {
	gd, ok := decl.(*ast.GenDecl)
	if !ok {
		return fmt.Errorf("invalid declaration %T", decl)
	}
	switch gd.Tok {
	case token.TYPE:
		// continue below
	case token.CONST:
		return nil // used for enums
	case token.IMPORT:
		return nil // imports; ignore
	default:
		return fmt.Errorf("invalid declaration token %v", gd.Tok)
	}
	for _, spec := range gd.Specs {
		ts := spec.(*ast.TypeSpec)
		if ts.Doc == nil {
			// pass it on to the helpers
			ts.Doc = gd.Doc
		}
		switch ts.Type.(type) {
		case *ast.StructType:
			msg, err := g.convertMessage(ts)
			if err != nil {
				return err
			}
			g.pfile.MessageType = append(g.pfile.MessageType, msg)
		case *ast.InterfaceType:
			srv, err := g.convertService(ts)
			if err != nil {
				return err
			}
			g.pfile.Service = append(g.pfile.Service, srv)
		case *ast.Ident:
			enum, err := g.convertEnum(ts)
			if err != nil {
				return err
			}
			g.pfile.EnumType = append(g.pfile.EnumType, enum)
		default:
			return fmt.Errorf("invalid declaration type %T", ts.Type)
		}
	}
	return nil
}

func (g *Generator) addDoc(doc *ast.CommentGroup, transform func(string) string, path ...int32) {
	if doc == nil {
		return
	}
	if g.pfile.SourceCodeInfo == nil {
		g.pfile.SourceCodeInfo = &desc.SourceCodeInfo{}
	}
	text := doc.Text()
	if transform != nil {
		text = transform(text)
	}
	g.pfile.SourceCodeInfo.Location = append(g.pfile.SourceCodeInfo.Location,
		&desc.SourceCodeInfo_Location{
			Path:            path,
			LeadingComments: proto.String(text),
		},
	)
}

func (g *Generator) convertMessage(tspec *ast.TypeSpec) (*desc.DescriptorProto, error) {
	g.addDoc(tspec.Doc, nil, messagePath, g.messageIndex)
	msg := &desc.DescriptorProto{
		Name: proto.String(tspec.Name.Name),
	}
	stype := tspec.Type.(*ast.StructType)
	for i, field := range stype.Fields.List {
		if len(field.Names) != 1 {
			return nil, fmt.Errorf("need all fields to have one name")
		}
		fieldName := field.Names[0].Name
		g.addDoc(field.Doc, nil, messagePath, g.messageIndex, messageFieldPath, int32(i))
		ftype := g.info.TypeOf(field.Type)

		var ptype desc.FieldDescriptorProto_Type
		var plabel desc.FieldDescriptorProto_Label
		var tname string
		var msgNestedType *desc.DescriptorProto

		// Check to see if the type is a map. Maps need to be made into a
		// repeated nested message containing key and value fields.
		if mtype, ok := ftype.(*types.Map); ok {
			ptype = desc.FieldDescriptorProto_TYPE_MESSAGE
			plabel = desc.FieldDescriptorProto_LABEL_REPEATED
			tname, msgNestedType = g.convertMap(tspec.Name.Name, fieldName, mtype)
			msg.NestedType = append(msg.NestedType, msgNestedType)
		} else {
			ptype, plabel, tname = g.convertType(ftype)
		}
		if ptype == 0 {
			return nil, fmt.Errorf("unsupported field type: %v", ftype)
		}

		// Check that the struct field has a tag. We currently
		// require all struct fields to have a tag; this is used
		// to assign the position number for a field, ie: `pb:"1"`
		if field.Tag == nil {
			return nil, fmt.Errorf("missing required tag on %s", fieldName)
		}
		// Can skip the error here because we've already parsed the file.
		str, _ := strconv.Unquote(field.Tag.Value)
		tag := reflect.StructTag(str)
		// TODO: record the position numbers used so we can return an
		// error if position number is used more than once? This would
		// also allow us to automatically assign fields a position
		// number if it is missing one.
		num, err := protoNumber(tag)
		if err != nil {
			return nil, fmt.Errorf("unable to convert tag to number on %s: %v", fieldName, err)
		}
		msg.Field = append(msg.Field, &desc.FieldDescriptorProto{
			Name:     proto.String(fieldName),
			Number:   num,
			TypeName: &tname,
			Type:     &ptype,
			Label:    &plabel,
			JsonName: jsonName(tag),
		})
	}
	g.messageIndex++
	return msg, nil
}

func (g *Generator) convertService(tspec *ast.TypeSpec) (*desc.ServiceDescriptorProto, error) {
	srv := &desc.ServiceDescriptorProto{
		Name: proto.String(tspec.Name.Name),
	}
	itype := tspec.Type.(*ast.InterfaceType)
	for i, method := range itype.Methods.List {
		if len(method.Names) != 1 {
			return nil, fmt.Errorf("need all methods to have one name")
		}
		tag := ""
		stripTag := func(text string) string {
			text, tag = splitGunkTag(text)
			return text
		}
		g.addDoc(method.Doc, stripTag, servicePath, g.serviceIndex,
			serviceMethodPath, int32(i))
		pmethod := &desc.MethodDescriptorProto{
			Name: proto.String(method.Names[0].Name),
		}
		sign := g.info.TypeOf(method.Type).(*types.Signature)
		var err error
		pmethod.InputType, err = g.convertParameter(sign.Params())
		if err != nil {
			return nil, err
		}
		pmethod.OutputType, err = g.convertParameter(sign.Results())
		if err != nil {
			return nil, err
		}
		if tag != "" {
			edesc, val, err := g.interpretTagValue(tag)
			if err != nil {
				return nil, err
			}
			if pmethod.Options == nil {
				pmethod.Options = &desc.MethodOptions{}
			}
			if err := proto.SetExtension(pmethod.Options, edesc, val); err != nil {
				return nil, err
			}
		}
		srv.Method = append(srv.Method, pmethod)
	}
	g.serviceIndex++
	return srv, nil
}

// convertMap will translate a Go map to a Protobuf respresentation of a map,
// returning the nested type name and definition.
//
// Protobuf represents a map as a nested message on the parent message. This
// nested message contains two fields; key and value (map[key]value), and has
// the MapEntry option set to true.
//
// https://developers.google.com/protocol-buffers/docs/proto#maps
func (g *Generator) convertMap(parentName, fieldName string, mapTyp *types.Map) (string, *desc.DescriptorProto) {
	mapName := fieldName + "Entry"
	typeName := g.qualifiedTypeName(parentName+"."+mapName, nil)

	keyType, _, keyTypeName := g.convertType(mapTyp.Key())
	if keyType == 0 {
		return "", nil
	}
	elemType, _, elemTypeName := g.convertType(mapTyp.Elem())
	if elemType == 0 {
		return "", nil
	}

	fieldLabel := desc.FieldDescriptorProto_LABEL_OPTIONAL
	nestedType := &desc.DescriptorProto{
		Name: proto.String(mapName),
		Options: &desc.MessageOptions{
			MapEntry: proto.Bool(true),
		},
		Field: []*desc.FieldDescriptorProto{
			{
				Name:     proto.String("key"),
				Number:   proto.Int32(1),
				Label:    &fieldLabel,
				Type:     &keyType,
				TypeName: &keyTypeName,
			},
			{
				Name:     proto.String("value"),
				Number:   proto.Int32(2),
				Label:    &fieldLabel,
				Type:     &elemType,
				TypeName: &elemTypeName,
			},
		},
	}
	return typeName, nestedType
}

func (g *Generator) interpretTagValue(tag string) (*proto.ExtensionDesc, interface{}, error) {
	// use Eval to resolve the type, and check for any errors in the
	// value expression
	tv, err := types.Eval(g.fset, g.curPkg.Types, g.gfile.End(), tag)
	if err != nil {
		return nil, nil, err
	}
	switch s := tv.Type.String(); s {
	case "github.com/gunk/opt/http.Match":
		// an error would be caught in Eval
		expr, _ := parser.ParseExpr(tag)

		// Capture the values required to use in annotations.HttpRule.
		// We need to evaluate the entire expression, and then we can
		// create an annotations.HttpRule.
		var path string
		var body string
		method := "GET"
		for _, elt := range expr.(*ast.CompositeLit).Elts {
			kv := elt.(*ast.KeyValueExpr)
			val, _ := strconv.Unquote(kv.Value.(*ast.BasicLit).Value)
			switch name := kv.Key.(*ast.Ident).Name; name {
			case "Method":
				method = val
			case "Path":
				path = val
				// TODO: grpc-gateway doesn't allow paths with a trailing "/", should
				// we return an error here, because the error from grpc-gateway is very
				// cryptic and unhelpful?
				// https://github.com/grpc-ecosystem/grpc-gateway/issues/472
			case "Body":
				body = val
			default:
				return nil, nil, fmt.Errorf("unknown expression key %q", name)
			}
		}
		rule := &annotations.HttpRule{
			Body: body,
		}
		switch method {
		case "GET":
			rule.Pattern = &annotations.HttpRule_Get{Get: path}
		case "POST":
			rule.Pattern = &annotations.HttpRule_Post{Post: path}
		case "DELETE":
			rule.Pattern = &annotations.HttpRule_Delete{Delete: path}
		case "PUT":
			rule.Pattern = &annotations.HttpRule_Put{Put: path}
		case "PATCH":
			rule.Pattern = &annotations.HttpRule_Patch{Patch: path}
		default:
			return nil, nil, fmt.Errorf("unknown method type: %q", method)
		}
		// TODO: Add support for custom rules - HttpRule_Custom?
		return annotations.E_Http, rule, nil
	default:
		return nil, nil, fmt.Errorf("unknown option type: %s", s)
	}
}

func (g *Generator) convertParameter(tuple *types.Tuple) (*string, error) {
	switch tuple.Len() {
	case 0:
		g.addProtoDep("google/protobuf/empty.proto")
		return proto.String(".google.protobuf.Empty"), nil
	case 1:
		// below
	default:
		return nil, fmt.Errorf("multiple parameters are not supported")
	}
	param := tuple.At(0).Type()
	_, _, tname := g.convertType(param)
	if tname == "" {
		return nil, fmt.Errorf("unsupported parameter type: %v", param)
	}
	return &tname, nil
}

func (g *Generator) convertEnum(tspec *ast.TypeSpec) (*desc.EnumDescriptorProto, error) {
	g.addDoc(tspec.Doc, nil, enumPath, g.enumIndex)
	enum := &desc.EnumDescriptorProto{
		Name: proto.String(tspec.Name.Name),
	}
	enumType := g.info.TypeOf(tspec.Name)
	for _, decl := range g.gfile.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for i, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			// .proto files have the same limitation, and it
			// allows per-value godocs
			if len(vs.Names) != 1 {
				return nil, fmt.Errorf("need all value specs to define one name")
			}
			name := vs.Names[0]
			if g.info.TypeOf(name) != enumType {
				continue
			}
			// SomeVal will be exported as SomeType_SomeVal
			g.addDoc(vs.Doc, namePrefix(tspec.Name.Name),
				enumPath, g.enumIndex, enumValuePath, int32(i))
			val := g.info.Defs[name].(*types.Const).Val()
			ival, _ := constant.Int64Val(val)
			enum.Value = append(enum.Value, &desc.EnumValueDescriptorProto{
				Name:   proto.String(name.Name),
				Number: proto.Int32(int32(ival)),
			})
		}
	}
	g.enumIndex++
	return enum, nil
}

// qualifiedTypeName will format the type name for that package. If the
// package is nil, it will format the type for the current package that is
// being processed.
//
// Currently we format the type as ".<pkg_name>.<type_name>"
func (g *Generator) qualifiedTypeName(typeName string, pkg *types.Package) string {
	// If pkg is nil, we should format the type for the current package.
	if pkg == nil {
		return "." + g.curPkg.ProtoName + "." + typeName
	}
	gpkg := g.gunkPkgs[pkg.Path()]
	return "." + gpkg.ProtoName + "." + typeName
}

// convertType converts a Go field or parameter type to Protobuf, returning its
// type descriptor, a label such as "repeated", and a name, if the final type is
// an enum or a message.
func (g *Generator) convertType(typ types.Type) (desc.FieldDescriptorProto_Type, desc.FieldDescriptorProto_Label, string) {
	switch typ := typ.(type) {
	case *types.Basic:
		// Map Go types to proto types via
		// https://developers.google.com/protocol-buffers/docs/proto3#scalar
		switch typ.Kind() {
		case types.String:
			return desc.FieldDescriptorProto_TYPE_STRING, desc.FieldDescriptorProto_LABEL_OPTIONAL, ""
		case types.Int, types.Int32:
			return desc.FieldDescriptorProto_TYPE_INT32, desc.FieldDescriptorProto_LABEL_OPTIONAL, ""
		case types.Bool:
			return desc.FieldDescriptorProto_TYPE_BOOL, desc.FieldDescriptorProto_LABEL_OPTIONAL, ""
		}
	case *types.Named:
		switch typ.String() {
		case "time.Time":
			g.addProtoDep("google/protobuf/timestamp.proto")
			return desc.FieldDescriptorProto_TYPE_MESSAGE, desc.FieldDescriptorProto_LABEL_OPTIONAL, ".google.protobuf.Timestamp"
		case "time.Duration":
			g.addProtoDep("google/protobuf/duration.proto")
			return desc.FieldDescriptorProto_TYPE_MESSAGE, desc.FieldDescriptorProto_LABEL_OPTIONAL, ".google.protobuf.Duration"
		}
		fullName := g.qualifiedTypeName(typ.Obj().Name(), typ.Obj().Pkg())
		switch u := typ.Underlying().(type) {
		case *types.Basic:
			switch u.Kind() {
			case types.Int, types.Int32:
				return desc.FieldDescriptorProto_TYPE_ENUM, desc.FieldDescriptorProto_LABEL_OPTIONAL, fullName
			}
		case *types.Struct:
			return desc.FieldDescriptorProto_TYPE_MESSAGE, desc.FieldDescriptorProto_LABEL_OPTIONAL, fullName
		}
	case *types.Slice:
		dtyp, _, name := g.convertType(typ.Elem())
		if dtyp == 0 {
			return 0, 0, ""
		}
		return dtyp, desc.FieldDescriptorProto_LABEL_REPEATED, name
	}
	return 0, 0, ""
}

// addPkg sets up a gunk package to be translated and generated. It is
// parsed from the gunk files on disk and type-checked, gathering all
// the info needed later on.
func (g *Generator) addPkg(pkgPath string) error {
	pkg := g.gunkPkgs[pkgPath]
	if pkg == nil {
		// Implicit gunk package dependency; load it and add it to
		// g.gunkPkgs.
		pkgs, err := loader.Load(g.dir, g.fset, pkgPath)
		if err != nil {
			return err
		}
		if len(pkgs) != 1 {
			panic("expected go/packages.Load to return exactly one package")
		}
		pkg = pkgs[0]
		g.gunkPkgs[pkgPath] = pkg
	}
	if len(pkg.GunkFiles) == 0 {
		return fmt.Errorf("gunk package %q contains no gunk files", pkgPath)
	}
	pkg.Types = types.NewPackage(pkgPath, pkg.Name)
	check := types.NewChecker(g.tconfig, g.fset, pkg.Types, g.info)
	if err := check.Files(pkg.GunkSyntax); err != nil {
		return err
	}
	g.toGen[pkgPath] = make(map[string]bool)
	return nil
}

// Import satisfies the go/types.Importer interface.
//
// Unlike standard Go ones like go/importer and x/tools/go/packages, this one
// uses our own addPkg to instead load gunk packages.
//
// Aside from that, it is very similar to standard Go importers that load from
// source. It too uses a cache to avoid loading packages multiple times.
func (g *Generator) Import(path string) (*types.Package, error) {
	// Has it been loaded with types before?
	if gpkg := g.gunkPkgs[path]; gpkg != nil && gpkg.Types != nil {
		return gpkg.Types, nil
	}

	// Loading a standard library package for the first time.
	if !strings.Contains(path, ".") {
		cfg := &packages.Config{Mode: packages.LoadTypes}
		pkgs, err := packages.Load(cfg, path)
		if err != nil {
			return nil, err
		}
		if len(pkgs) != 1 {
			panic("expected go/packages.Load to return exactly one package")
		}
		gpkg := &loader.GunkPackage{Package: *pkgs[0]}
		g.gunkPkgs[path] = gpkg
		return gpkg.Types, nil
	}

	// Loading a Gunk package for the first time.
	if err := g.addPkg(path); err != nil {
		return nil, err
	}
	if err := g.translatePkg(path); err != nil {
		return nil, err
	}
	if gpkg := g.gunkPkgs[path]; gpkg != nil {
		return gpkg.Types, nil
	}

	// Not found.
	return nil, nil
}

// addProtoDep is called when a gunk file is known to require importing of a
// proto file, such as when using google.protobuf.Empty.
func (g *Generator) addProtoDep(protoPath string) {
	for _, dep := range g.pfile.Dependency {
		if dep == protoPath {
			return // already in there
		}
	}
	g.pfile.Dependency = append(g.pfile.Dependency, protoPath)
}

// loadProtoDeps loads all the missing proto dependencies added with
// addProtoDep.
func (g *Generator) loadProtoDeps() error {
	missing := make(map[string]bool)
	var list []string
	for _, pfile := range g.allProto {
		for _, dep := range pfile.Dependency {
			if _, e := g.allProto[dep]; !e && !missing[dep] {
				missing[dep] = true
				list = append(list, dep)
			}
		}
	}

	files, err := loader.LoadProto(list...)
	if err != nil {
		return nil
	}

	for _, pfile := range files {
		g.allProto[*pfile.Name] = pfile
	}
	return nil
}