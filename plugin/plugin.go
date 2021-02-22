package plugin

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/golang/protobuf/protoc-gen-go/generator"
	jgorm "github.com/jinzhu/gorm"
	"github.com/jinzhu/inflection"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"log"

	gorm "github.com/edhaight/protoc-gen-gorm/options"
)

const (
	typeMessage = 11
	typeEnum    = 14

	protoTypeTimestamp = "Timestamp" // last segment, first will be *google_protobufX
	protoTypeJSON      = "JSONValue"
	protoTypeUUID      = "UUID"
	protoTypeUUIDValue = "UUIDValue"
	protoTypeResource  = "Identifier"
	protoTypeInet      = "InetValue"
	protoTimeOnly      = "TimeOnly"
)

// DB Engine Enum
const (
	ENGINE_UNSET = iota
	ENGINE_POSTGRES
)

var wellKnownTypes = map[string]string{
	"StringValue": "*string",
	"DoubleValue": "*float64",
	"FloatValue":  "*float32",
	"Int32Value":  "*int32",
	"Int64Value":  "*int64",
	"UInt32Value": "*uint32",
	"UInt64Value": "*uint64",
	"BoolValue":   "*bool",
	//  "BytesValue" : "*[]byte",
}

var protoPrimitiveKinds = map[protoreflect.Kind]string{
	protoreflect.BoolKind:   "bool",
	protoreflect.Int32Kind:  "int32",
	protoreflect.Int64Kind:  "int64",
	protoreflect.Sint32Kind: "int32",
	protoreflect.Sint64Kind: "int64",
	protoreflect.Uint32Kind: "uint32",
	protoreflect.Uint64Kind: "uint64",

	protoreflect.Fixed32Kind:  "uint32",
	protoreflect.Fixed64Kind:  "uint64",
	protoreflect.FloatKind:    "float32",
	protoreflect.Sfixed32Kind: "int32",
	protoreflect.Sfixed64Kind: "int64",

	protoreflect.DoubleKind: "float64",
	protoreflect.StringKind: "string",
	protoreflect.BytesKind:  "[]byte",
}

var builtinTypes = map[string]struct{}{
	"bool": {},
	"int":  {},
	"int8": {}, "int16": {},
	"int32": {}, "int64": {},
	"uint":  {},
	"uint8": {}, "uint16": {},
	"uint32": {}, "uint64": {},
	"uintptr": {},
	"float32": {}, "float64": {},
	"string": {},
	"[]byte": {},
}

type OrmableType struct {
	OriginName string
	Name       string
	Message    *protogen.Message
	Package    string
	File       *protogen.File
	Fields     map[string]*Field
	debug      map[string]bool
	Methods    map[string]*autogenMethod
}

type Field struct {
	ParentGoType string
	Type         string
	F            *protogen.Field
	Package      string

	*gorm.GormFieldOptions
	ParentOriginName string
}

func NewOrmableType(oname string, msg *protogen.Message, file *protogen.File) *OrmableType {
	return &OrmableType{
		OriginName: oname,
		Message:    msg,
		File:       file,
		Fields:     make(map[string]*Field),
		Methods:    make(map[string]*autogenMethod),
	}
}

// OrmPlugin implements the plugin interface and creates GORM code from .protos
type OrmPlugin struct {
	*protogen.Plugin
	SuppressWarnings bool
	DBEngine         int
	StringEnums      bool
	Gateway          bool
	ormableTypes     OrmableLookup
	EmptyFiles       []string
	currentPackage   protogen.GoImportPath
	currentFile      *protogen.GeneratedFile
	fileName         string
	messages         map[string]struct{}
	ormableServices  []autogenService
}

func (p *OrmPlugin) Fail(args ...string) {
	panic(errors.New(strings.Join(args, " ")))
}

func (p *OrmPlugin) P(args ...interface{}) {
	p.currentFile.P(args...)
}

func (p *OrmPlugin) setFile(file *protogen.GeneratedFile) {
	p.currentFile = file
}

// Name identifies the plugin
func (p *OrmPlugin) Name() string {
	return "gorm"
}

// Init is called once after data structures are built but before
// code generation begins.
func (p *OrmPlugin) Init(g *protogen.Plugin) {
	p.Plugin = g
	p.messages = make(map[string]struct{})
	p.ormableTypes = make(map[string]*OrmableType)

	// params := g.Request.GetParameter()
	// if strings.EqualFold(g.Request.GetParameter()["engine"], "postgres") {
	p.DBEngine = ENGINE_POSTGRES
	// } else {
	// 	p.DBEngine = ENGINE_UNSET
	// }
	// if strings.EqualFold(g.Param["enums"], "string") {
	p.StringEnums = true
	// }
	// if _, ok := g.Param["gateway"]; ok {
	// 	p.Gateway = true
	// }
	// if _, ok := g.Param["quiet"]; ok {
	// 	p.SuppressWarnings = true
	// }
}

// Generate produces the code generated by the plugin for this file,
// except for the imports, by calling the generator's methods P, In, and Out.
func (p *OrmPlugin) Generate() {

	generatedFileLookup := make(map[*protogen.File]*protogen.GeneratedFile)
	skipped := make([]string, 0)
	for _, file := range p.Plugin.Files {
		p.currentPackage = file.GoImportPath
		if file.Generate {
			outfile := p.NewGeneratedFile(file.GeneratedFilenamePrefix+".test.gorm.go", p.currentPackage)
			p.setFile(outfile)
			p.fileName = file.GeneratedFilenamePrefix
			generatedFileLookup[file] = outfile
			p.P(fmt.Sprintf("package %s\n", string(file.GoPackageName)))
		} else {
			skipped = append(skipped, file.GeneratedFilenamePrefix)
		}
		// Preload just the types we'll be creating
		for _, msg := range file.Messages {
			// We don't want to bother with the MapEntry stuff
			if msg.Desc.IsMapEntry() {
				continue
			}

			typeName := messageName(msg)
			p.messages[typeName] = struct{}{}

			if getMessageOptions(msg).GetOrmable() && !p.isOrmable(typeName) {
				p.ormableTypes[typeName] = NewOrmableType(typeName, msg, file)
			}
		}
		for _, msg := range file.Messages {
			if p.isOrmableMessage(msg) {
				p.parseBasicFields(msg)
			}
		}
		for _, msg := range file.Messages {
			if p.isOrmableMessage(msg) {
				p.parseAssociations(msg)
				o := p.getOrmableMessage(msg)
				if p.hasPrimaryKey(o) {
					_, fd := p.findPrimaryKey(o)
					fd.ParentOriginName = o.OriginName
				}
			}
		}
		p.parseServices(file)
	}
	for file, generated := range generatedFileLookup {
		p.setFile(generated)
		p.currentPackage = file.GoImportPath
		for _, msg := range file.Messages {
			if !p.isOrmableMessage(msg) {
				continue
			}
			p.generateOrmable(msg)
			p.generateTableNameFunction(msg)
			p.generateConvertFunctions(msg)
			p.generateHookInterfaces(msg)
		}
		p.generateDefaultHandlers(file)
		p.generateDefaultServer(file)
	}

}

func (p *OrmPlugin) parseBasicFields(msg *protogen.Message) {
	typeName := messageName(msg)
	ormable := p.getOrmable(typeName)
	ormable.Name = fmt.Sprintf("%sORM", typeName)

	for _, field := range msg.Fields {
		fieldOpts := getFieldOptions(field)
		if fieldOpts == nil {
			fieldOpts = &gorm.GormFieldOptions{}
		}
		if fieldOpts.GetDrop() {
			continue
		}
		tag := fieldOpts.GetTag()
		desc := field.Desc
		fieldName := field.GoName
		// ident := field.G
		fieldType := p.fieldType(field)

		var typePackage string
		if p.DBEngine == ENGINE_POSTGRES && p.IsAbleToMakePQArray(fieldType) {
			switch fieldType {
			case "[]bool":
				fieldType = p.qualifiedGoIdent(identpqBoolArray)
				fieldOpts.Tag = tagWithType(tag, "bool[]")
			case "[]float64":
				fieldType = p.qualifiedGoIdent(identpqFloat64Array)
				fieldOpts.Tag = tagWithType(tag, "float[]")
			case "[]int64":
				fieldType = p.qualifiedGoIdent(identpqInt64Array)
				fieldOpts.Tag = tagWithType(tag, "integer[]")
			case "[]string":
				fieldType = p.qualifiedGoIdent(identpqStringArray)
				fieldOpts.Tag = tagWithType(tag, "text[]")
			default:
				continue
			}
		} else if (desc.Message() != nil || !p.isOrmable(fieldType)) && desc.IsList() {
			// Not implemented yet
			continue
		} else if desc.Enum() != nil {
			field.GoIdent.GoName = "int32"
			if p.StringEnums {
				field.GoIdent.GoName = "string"
			}
		} else if desc.Message() != nil {

			fieldType = string(desc.Message().Name())
			parts := strings.Split(fieldType, ".")
			rawType := parts[len(parts)-1]
			//Check for WKTs or fields of nonormable types
			if v, exists := wellKnownTypes[rawType]; exists {
				field.GoIdent.GoName = v
			} else if rawType == protoTypeUUID {
				field.GoIdent = identUUID
				if p.DBEngine == ENGINE_POSTGRES {
					fieldOpts.Tag = tagWithType(tag, "uuid")
				}
			} else if rawType == protoTypeUUIDValue {
				field.GoIdent = ptrIdent(identUUID)
				// fieldType = p.qualifiedGoIdentPtr(identUUID)
				if p.DBEngine == ENGINE_POSTGRES {
					fieldOpts.Tag = tagWithType(tag, "uuid")
				}
			} else if rawType == protoTypeTimestamp {
				// fieldType = "*" + noQuoteTmp(identTime)
				field.GoIdent = ptrIdent(identTime)
			} else if rawType == protoTypeJSON {
				if p.DBEngine == ENGINE_POSTGRES {
					field.GoIdent = ptrIdent(identpqJsonb)
					fieldOpts.Tag = tagWithType(tag, "jsonb")
				} else {
					// Potential TODO: add types we want to use in other/default DB engine
					continue
				}
			} else if rawType == protoTypeResource {
				tag := getFieldOptions(field).GetTag()
				ttype := tag.GetType()
				ttype = strings.ToLower(ttype)
				if strings.Contains(ttype, "char") {
					ttype = "char"
				}
				if strings.Contains(ttype, "array") || strings.ContainsAny(ttype, "[]") {
					ttype = "array"
				}
				switch ttype {
				case "uuid", "text", "char", "array", "cidr", "inet", "macaddr":
					fieldType = "*string"
				case "smallint", "integer", "bigint", "numeric", "smallserial", "serial", "bigserial":
					fieldType = "*int64"
				case "jsonb", "bytea":
					fieldType = "[]byte"
				case "":
					fieldType = "interface{}" // we do not know the type yet (if it association we will fix the type later)
				default:
					p.Fail("unknown tag type of atlas.rpc.Identifier")
				}
				if tag.GetNotNull() || tag.GetPrimaryKey() {
					fieldType = strings.TrimPrefix(fieldType, "*")
				}
				field.GoIdent.GoImportPath = ""
				field.GoIdent.GoName = fieldType
			} else if rawType == protoTypeInet {
				field.GoIdent = ptrIdent(identTypesInet)
				// typePackage = gtypesImport
				if p.DBEngine == ENGINE_POSTGRES {
					fieldOpts.Tag = tagWithType(tag, "inet")
				} else {
					fieldOpts.Tag = tagWithType(tag, "varchar(48)")
				}
			} else if rawType == protoTimeOnly {
				field.GoIdent.GoName = "string"
				fieldOpts.Tag = tagWithType(tag, "time")
			} else {
				continue
			}
		} else {
			field.GoIdent.GoName = fieldType
		}

		f := &Field{F: field, Type: field.GoIdent.GoName, Package: typePackage, GormFieldOptions: fieldOpts}

		if tname := getFieldOptions(field).GetReferenceOf(); tname != "" {
			if _, ok := p.messages[tname]; !ok {
				p.Fail("unknown message type in refers_to: ", tname, " in field: ", fieldName, " of type: ", typeName)
			}
			f.ParentOriginName = tname
		}
		ormable.Fields[fieldName] = f
	}
	if getMessageOptions(msg).GetMultiAccount() {
		if accID, ok := ormable.Fields["AccountID"]; !ok {
			ormable.Fields["AccountID"] = &Field{Type: "string"}
		} else if accID.Type != "string" {
			p.Fail("Cannot include AccountID field into", ormable.Name, "as it already exists there with a different type.")
		}
	}
	for _, field := range getMessageOptions(msg).GetInclude() {
		fieldName := field.GetName()
		if _, ok := ormable.Fields[fieldName]; !ok {
			p.addIncludedField(ormable, field)
		} else {
			p.Fail("Cannot include", fieldName, "field into", ormable.Name, "as it aready exists there.")
		}
	}
}

func tagWithType(tag *gorm.GormTag, typename string) *gorm.GormTag {
	if tag == nil {
		tag = &gorm.GormTag{}
	}
	tag.Type = proto.String(typename)
	return tag
}

func (p *OrmPlugin) addIncludedField(ormable *OrmableType, field *gorm.ExtraField) {
	fieldName := generator.CamelCase(field.GetName())
	isPtr := strings.HasPrefix(field.GetType(), "*")
	rawType := strings.TrimPrefix(field.GetType(), "*")
	// cut off any package subpaths
	rawType = rawType[strings.LastIndex(rawType, ".")+1:]
	var typePackage string
	f := &protogen.Field{}
	// Handle types with a package defined
	if field.GetPackage() != "" {
		f.GoIdent = protogen.GoIdent{
			GoName:       rawType,
			GoImportPath: protogen.GoImportPath(field.GetPackage()),
		}
		typePackage = field.GetPackage()
	} else {
		// Handle types without a package defined
		if _, ok := builtinTypes[rawType]; ok {
			f.GoIdent = protogen.GoIdent{GoName: rawType, GoImportPath: protogen.GoImportPath(p.currentPackage)}
			// basic type, 100% okay, no imports or changes needed
		} else if rawType == "Time" {
			f.GoIdent = identTime
		} else if rawType == "UUID" {
			f.GoIdent = identUUID
		} else if field.GetType() == "Jsonb" && p.DBEngine == ENGINE_POSTGRES {
			f.GoIdent = identpqJsonb
		} else if rawType == "Inet" {
			f.GoIdent = identTypesInet
		} else {
			p.warning(`included field %q of type %q is not a recognized special type, and no package specified. This type is assumed to be in the same package as the generated code`,
				field.GetName(), field.GetType())
			f.GoIdent = protogen.GoIdent{GoName: field.GetType(), GoImportPath: protogen.GoImportPath(p.currentPackage)}
		}
	}
	if isPtr {
		rawType = fmt.Sprintf("*%s", rawType)
		f.GoIdent.GoName = rawType
	}
	tmp := &Field{F: f, Type: rawType, Package: typePackage, GormFieldOptions: &gorm.GormFieldOptions{Tag: field.GetTag()}}
	ormable.Fields[fieldName] = tmp

}

func (p *OrmPlugin) getSortedFieldNames(fields map[string]*Field) []string {
	var keys []string
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (p *OrmPlugin) generateOrmable(message *protogen.Message) {
	ormable := p.getOrmableMessage(message)
	p.P(`type `, ormable.Name, ` struct {`)
	for _, fieldName := range p.getSortedFieldNames(ormable.Fields) {
		field := ormable.Fields[fieldName]
		t := field.Type
		if field.F == nil {
			// TODO: this is caused by multi account functionality.. fix it
			// p.warning("nil field %s with type %s for ormable %s", fieldName, t, ormable.Name)
		} else {
			t = p.qualifiedGoIdent(field.F.GoIdent)
		}

		p.P(fieldName, ` `, t, p.renderGormTag(field))
	}
	p.P(`}`)
}

// generateTableNameFunction the function to set the gorm table name
// back to gorm default, removing "ORM" suffix
func (p *OrmPlugin) generateTableNameFunction(message *protogen.Message) {
	typeName := p.messageType(message)

	p.P(`// TableName overrides the default tablename generated by GORM`)
	p.P(`func (`, typeName, `ORM) TableName() string {`)

	tableName := inflection.Plural(jgorm.ToDBName(typeName))
	if opts := getMessageOptions(message); opts != nil && opts.Table != nil {
		tableName = opts.GetTable()
	}
	p.P(`return "`, tableName, `"`)
	p.P(`}`)
}

// generateMapFunctions creates the converter functions
func (p *OrmPlugin) generateConvertFunctions(message *protogen.Message) {
	typeName := p.messageType(message)
	ormable := p.getOrmable(typeName)
	///// To Orm
	p.P(`// ToORM runs the BeforeToORM hook if present, converts the fields of this`)
	p.P(`// object to ORM format, runs the AfterToORM hook, then returns the ORM object`)
	p.P(`func (m *`, typeName, `) ToORM (ctx `, identCtx, `) (`, typeName, `ORM, error) {`)
	p.P(`to := `, typeName, `ORM{}`)
	p.P(`var err error`)
	p.P(`if prehook, ok := interface{}(m).(`, typeName, `WithBeforeToORM); ok {`)
	p.P(`if err = prehook.BeforeToORM(ctx, &to); err != nil {`)
	p.P(`return to, err`)
	p.P(`}`)
	p.P(`}`)
	for _, field := range message.Fields {
		// Checking if field is skipped
		if getFieldOptions(field).GetDrop() {
			continue
		}
		fname := field.GoName

		ofield := ormable.Fields[fname]
		p.generateFieldConversion(message, field, true, ofield)
	}
	if getMessageOptions(message).GetMultiAccount() {
		p.P(`accountID, err := `, identGetAccountIDFn, `(ctx, nil)`)
		p.P(`if err != nil {`)
		p.P(`return to, err`)
		p.P(`}`)
		p.P(`to.AccountID = accountID`)
	}
	p.setupOrderedHasMany(message)
	p.P(`if posthook, ok := interface{}(m).(`, typeName, `WithAfterToORM); ok {`)
	p.P(`err = posthook.AfterToORM(ctx, &to)`)
	p.P(`}`)
	p.P(`return to, err`)
	p.P(`}`)

	p.P()
	///// To Pb
	p.P(`// ToPB runs the BeforeToPB hook if present, converts the fields of this`)
	p.P(`// object to PB format, runs the AfterToPB hook, then returns the PB object`)
	p.P(`func (m *`, typeName, `ORM) ToPB (ctx `, identCtx, `) (`,
		typeName, `, error) {`)
	p.P(`to := `, typeName, `{}`)
	p.P(`var err error`)
	p.P(`if prehook, ok := interface{}(m).(`, typeName, `WithBeforeToPB); ok {`)
	p.P(`if err = prehook.BeforeToPB(ctx, &to); err != nil {`)
	p.P(`return to, err`)
	p.P(`}`)
	p.P(`}`)
	for _, field := range message.Fields {
		// Checking if field is skipped
		if getFieldOptions(field).GetDrop() {
			continue
		}
		ofield := ormable.Fields[field.GoName]
		p.generateFieldConversion(message, field, false, ofield)
	}
	p.P(`if posthook, ok := interface{}(m).(`, typeName, `WithAfterToPB); ok {`)
	p.P(`err = posthook.AfterToPB(ctx, &to)`)
	p.P(`}`)
	p.P(`return to, err`)
	p.P(`}`)
}

// Output code that will convert a field to/from orm.
func (p *OrmPlugin) generateFieldConversion(message *protogen.Message, field *protogen.Field, toORM bool, ofield *Field) error {
	desc := field.Desc
	fieldName := fieldName(field)
	fieldType := p.fieldType(field)
	ident := fieldIdent(field)

	if desc.IsList() { // Repeated Object ----------------------------------
		// Some repeated fields can be handled by github.com/lib/pq
		if p.DBEngine == ENGINE_POSTGRES && p.IsAbleToMakePQArray(fieldType) {
			p.P(`if m.`, fieldName, ` != nil {`)
			switch fieldType {
			case "[]bool":
				p.P(`to.`, fieldName, ` = make(`, identpqBoolArray, `, len(m.`, fieldName, `))`)
			case "[]float64":
				p.P(`to.`, fieldName, ` = make(`, identpqFloat64Array, `, len(m.`, fieldName, `))`)
			case "[]int64":
				p.P(`to.`, fieldName, ` = make(`, identpqInt64Array, `, len(m.`, fieldName, `))`)
			case "[]string":
				p.P(`to.`, fieldName, ` = make(`, identpqStringArray, `, len(m.`, fieldName, `))`)
			}
			p.P(`copy(to.`, fieldName, `, m.`, fieldName, `)`)
			p.P(`}`)
		} else if p.isOrmable(fieldType) { // Repeated ORMable type
			//fieldType = strings.Trim(fieldType, "[]*")

			p.P(`for _, v := range m.`, fieldName, ` {`)
			p.P(`if v != nil {`)
			if toORM {
				p.P(`if temp`, fieldName, `, cErr := v.ToORM(ctx); cErr == nil {`)
			} else {
				p.P(`if temp`, fieldName, `, cErr := v.ToPB(ctx); cErr == nil {`)
			}
			p.P(`to.`, fieldName, ` = append(to.`, fieldName, `, &temp`, fieldName, `)`)
			p.P(`} else {`)
			p.P(`return to, cErr`)
			p.P(`}`)
			p.P(`} else {`)
			p.P(`to.`, fieldName, ` = append(to.`, fieldName, `, nil)`)
			p.P(`}`)
			p.P(`}`) // end repeated for
		} else {
			p.P(`// Repeated type `, fieldType, ` is not an ORMable message type`)
		}
	} else if desc.Enum() != nil { // Singular Enum, which is an int32 ---
		if toORM {
			if p.StringEnums {
				p.P(`to.`, fieldName, ` = `, ident, `_name[int32(m.`, fieldName, `)]`)
			} else {
				p.P(`to.`, fieldName, ` = int32(m.`, fieldName, `)`)
			}
		} else {
			if p.StringEnums {
				p.P(`to.`, fieldName, ` = `, ident, `(`, ident, `_value[m.`, fieldName, `])`)
			} else {
				p.P(`to.`, fieldName, ` = `, ident, `(m.`, fieldName, `)`)
			}
		}
	} else if desc.Message() != nil { // Singular Object -------------
		//Check for WKTs
		parts := strings.Split(fieldType, ".")
		coreType := parts[len(parts)-1]
		// Type is a WKT, convert to/from as ptr to base type
		if _, exists := wellKnownTypes[coreType]; exists { // Singular WKT -----
			if toORM {
				p.P(`if m.`, fieldName, ` != nil {`)
				p.P(`v := m.`, fieldName, `.Value`)
				p.P(`to.`, fieldName, ` = &v`)
				p.P(`}`)
			} else {
				p.P(`if m.`, fieldName, ` != nil {`)
				p.P(`to.`, fieldName, ` = &`, ident,
					`{Value: *m.`, fieldName, `}`)
				p.P(`}`)
			}
		} else if coreType == protoTypeUUIDValue { // Singular UUIDValue type ----
			if toORM {
				p.P(`if m.`, fieldName, ` != nil {`)
				p.P(`tempUUID, uErr := `, p.identFnCall(identUUIDFromStringFn, fmt.Sprintf("m.%s.Value", fieldName)))
				p.P(`if uErr != nil {`)
				p.P(`return to, uErr`)
				p.P(`}`)
				p.P(`to.`, fieldName, ` = &tempUUID`)
				p.P(`}`)
			} else {
				p.P(`if m.`, fieldName, ` != nil {`)
				p.P(`to.`, fieldName, ` = &`, identTypesUUIDValue, `{Value: m.`, fieldName, `.String()}`)
				p.P(`}`)
			}
		} else if coreType == protoTypeUUID { // Singular UUID type --------------
			if toORM {
				p.P(`if m.`, fieldName, ` != nil {`)
				p.P(`to.`, fieldName, `, err = `, p.identFnCall(identUUIDFromStringFn, fmt.Sprintf("m.%s.Value", fieldName)))
				p.P(`if err != nil {`)
				p.P(`return to, err`)
				p.P(`}`)
				p.P(`} else {`)
				p.P(`to.`, fieldName, ` = `, identNilUUID)
				p.P(`}`)
			} else {
				p.P(`to.`, fieldName, ` = &`, identTypesUUID, `{Value: m.`, fieldName, `.String()}`)
			}
		} else if coreType == protoTypeTimestamp { // Singular WKT Timestamp ---
			if toORM {
				p.P(`if m.`, fieldName, ` != nil {`)
				p.P(`var t `, identTime)
				p.P(`if t, err = `, identTimestamp, `(m.`, fieldName, `); err != nil {`)
				p.P(`return to, err`)
				p.P(`}`)
				p.P(`to.`, fieldName, ` = &t`)
				p.P(`}`)
			} else {
				p.P(`if m.`, fieldName, ` != nil {`)
				p.P(`if to.`, fieldName, `, err = `, identTimestampProto, `(*m.`, fieldName, `); err != nil {`)
				p.P(`return to, err`)
				p.P(`}`)
				p.P(`}`)
			}
		} else if coreType == protoTypeJSON {
			if p.DBEngine == ENGINE_POSTGRES {
				if toORM {
					p.P(`if m.`, fieldName, ` != nil {`)
					p.P(`to.`, fieldName, ` = &`, identpqJsonb, `{[]byte(m.`, fieldName, `.Value)}`)
					p.P(`}`)
				} else {
					p.P(`if m.`, fieldName, ` != nil {`)
					p.P(`to.`, fieldName, ` = &`, identTypesJSONValue, `{Value: string(m.`, fieldName, `.RawMessage)}`)
					p.P(`}`)
				}
			} // Potential TODO other DB engine handling if desired
		} else if coreType == protoTypeResource {
			resource := "nil" // assuming we do not know the PB type, nil means call codec for any resource
			if ofield != nil && ofield.ParentOriginName != "" {
				resource = "&" + ofield.ParentOriginName + "{}"
			}
			btype := strings.TrimPrefix(ofield.Type, "*")
			nillable := strings.HasPrefix(ofield.Type, "*")
			iface := ofield.Type == "interface{}"

			if toORM {
				if nillable {
					p.P(`if m.`, fieldName, ` != nil {`)
				}
				switch btype {
				case "int64":
					p.P(`if v, err :=`, identResourceDecodeInt64Fn, `(`, resource, `, m.`, fieldName, `); err != nil {`)
					p.P(`	return to, err`)
					p.P(`} else {`)
					if nillable {
						p.P(`to.`, fieldName, ` = &v`)
					} else {
						p.P(`to.`, fieldName, ` = v`)
					}
					p.P(`}`)
				case "[]byte":
					p.P(`if v, err :=`, identResourceDecodeBytesFn, `(`, resource, `, m.`, fieldName, `); err != nil {`)
					p.P(`	return to, err`)
					p.P(`} else {`)
					p.P(`	to.`, fieldName, ` = v`)
					p.P(`}`)
				default:
					p.P(`if v, err :=`, identResourceDecodeFn, `(`, resource, `, m.`, fieldName, `); err != nil {`)
					p.P(`return to, err`)
					p.P(`} else if v != nil {`)
					if nillable {
						p.P(`vv := v.(`, btype, `)`)
						p.P(`to.`, fieldName, ` = &vv`)
					} else if iface {
						p.P(`to.`, fieldName, `= v`)
					} else {
						p.P(`to.`, fieldName, ` = v.(`, btype, `)`)
					}
					p.P(`}`)
				}
				if nillable {
					p.P(`}`)
				}
			}

			if !toORM {
				if nillable {
					p.P(`if m.`, fieldName, `!= nil {`)
					p.P(`	if v, err := `, identResourceEncodeFn, `(`, resource, `, *m.`, fieldName, `); err != nil {`)
					p.P(`		return to, err`)
					p.P(`	} else {`)
					p.P(`		to.`, fieldName, ` = v`)
					p.P(`	}`)
					p.P(`}`)

				} else {
					p.P(`if v, err := `, identResourceEncodeFn, `(`, resource, `, m.`, fieldName, `); err != nil {`)
					p.P(`return to, err`)
					p.P(`} else {`)
					p.P(`to.`, fieldName, ` = v`)
					p.P(`}`)
				}
			}
		} else if coreType == protoTypeInet { // Inet type for Postgres only, currently
			if toORM {
				p.P(`if m.`, fieldName, ` != nil {`)
				p.P(`if to.`, fieldName, `, err = `, identTypesParseInetFn, `(m.`, fieldName, `.Value); err != nil {`)
				p.P(`return to, err`)
				p.P(`}`)
				p.P(`}`)
			} else {
				p.P(`if m.`, fieldName, ` != nil && m.`, fieldName, `.IPNet != nil {`)
				p.P(`to.`, fieldName, ` = &`, identTypesInetValue, `{Value: m.`, fieldName, `.String()}`)
				p.P(`}`)
			}
		} else if coreType == protoTimeOnly { // Time only to support time via string
			if toORM {
				p.P(`if m.`, fieldName, ` != nil {`)
				p.P(`if to.`, fieldName, `, err = `, identTypesParseTimeFn, `(m.`, fieldName, `.Value); err != nil {`)
				p.P(`return to, err`)
				p.P(`}`)
				p.P(`}`)
			} else {
				p.P(`if m.`, fieldName, ` != "" {`)
				p.P(`if to.`, fieldName, `, err = `, identTypesTimeOnlyByStringFn, `( m.`, fieldName, `); err != nil {`)
				p.P(`return to, err`)
				p.P(`}`)
				p.P(`}`)
			}
		} else if p.isOrmable(fieldType) {
			// Not a WKT, but a type we're building converters for
			p.P(`if m.`, fieldName, ` != nil {`)
			if toORM {
				p.P(`temp`, fieldName, `, err := m.`, fieldName, `.ToORM (ctx)`)
			} else {
				p.P(`temp`, fieldName, `, err := m.`, fieldName, `.ToPB (ctx)`)
			}
			p.P(`if err != nil {`)
			p.P(`return to, err`)
			p.P(`}`)
			p.P(`to.`, fieldName, ` = &temp`, fieldName)
			p.P(`}`)
		}
	} else { // Singular raw ----------------------------------------------------
		p.P(`to.`, fieldName, ` = m.`, fieldName)
	}
	return nil
}

func (p *OrmPlugin) generateHookInterfaces(message *protogen.Message) {
	ident := message.GoIdent
	ormIdent := ormIdent(ident)
	p.P(`// The following are interfaces you can implement for special behavior during ORM/PB conversions`)
	p.P(`// of type `, ident, ` the arg will be the target, the caller the one being converted from`)
	p.P()
	for _, desc := range [][]interface{}{
		{"BeforeToORM", ormIdent, " called before default ToORM code"},
		{"AfterToORM", ormIdent, " called after default ToORM code"},
		{"BeforeToPB", ident, " called before default ToPB code"},
		{"AfterToPB", ident, " called after default ToPB code"},
	} {
		p.P(`// `, ident, desc[0], desc[2])
		p.P(`type `, ident, `With`, desc[0], ` interface {`)
		p.P(desc[0], `(`, identCtx, `, *`, desc[1], `) error`)
		p.P(`}`)
		p.P()
	}
}

func (p *OrmPlugin) setupOrderedHasMany(message *protogen.Message) {
	ormable := p.getOrmable(message.GoIdent.GoName)
	for _, fieldName := range p.getSortedFieldNames(ormable.Fields) {
		p.setupOrderedHasManyByName(message, fieldName)
	}
}

func (p *OrmPlugin) setupOrderedHasManyByName(message *protogen.Message, fieldName string) {
	ormable := p.getOrmable(message.GoIdent.GoName)
	field := ormable.Fields[fieldName]

	if field == nil {
		return
	}

	if field.GetHasMany().GetPositionField() != "" {
		positionField := field.GetHasMany().GetPositionField()
		positionFieldType := p.getOrmable(field.Type).Fields[positionField].Type
		p.P(`for i, e := range `, `to.`, fieldName, `{`)
		p.P(`e.`, positionField, ` = `, positionFieldType, `(i)`)
		p.P(`}`)
	}
}

func (p *OrmPlugin) warning(format string, v ...interface{}) {
	if !p.SuppressWarnings {
		log.Printf("WARNING: "+format, v...)
	}
}
