package compiler

import (
	"fmt"
	"strconv"

	"github.com/kyleconroy/sqlc/internal/sql/ast"
	"github.com/kyleconroy/sqlc/internal/sql/ast/pg"
	"github.com/kyleconroy/sqlc/internal/sql/astutils"
	"github.com/kyleconroy/sqlc/internal/sql/catalog"
	"github.com/kyleconroy/sqlc/internal/sql/sqlerr"
)

func dataType(n *ast.TypeName) string {
	if n.Schema != "" {
		return n.Schema + "." + n.Name
	} else {
		return n.Name
	}
}

func resolveCatalogRefs(c *catalog.Catalog, rvs []*pg.RangeVar, args []paramRef, names map[int]string) ([]Parameter, error) {
	aliasMap := map[string]*ast.TableName{}
	// TODO: Deprecate defaultTable
	var defaultTable *ast.TableName
	var tables []*ast.TableName

	parameterName := func(n int, defaultName string) string {
		if n, ok := names[n]; ok {
			return n
		}
		return defaultName
	}

	for _, rv := range rvs {
		if rv.Relname == nil {
			continue
		}
		fqn, err := parseTableName(rv)
		if err != nil {
			return nil, err
		}
		tables = append(tables, fqn)
		if defaultTable == nil {
			defaultTable = fqn
		}
		if rv.Alias == nil {
			continue
		}
		aliasMap[*rv.Alias.Aliasname] = fqn
	}

	typeMap := map[string]map[string]map[string]*catalog.Column{}
	for _, fqn := range tables {
		table, err := c.GetTable(fqn)
		if err != nil {
			continue
		}
		if _, exists := typeMap[fqn.Schema]; !exists {
			typeMap[fqn.Schema] = map[string]map[string]*catalog.Column{}
		}
		typeMap[fqn.Schema][fqn.Name] = map[string]*catalog.Column{}
		for _, c := range table.Columns {
			cc := c
			typeMap[fqn.Schema][fqn.Name][c.Name] = cc
		}
	}

	var a []Parameter
	for _, ref := range args {
		switch n := ref.parent.(type) {

		case *limitOffset:
			a = append(a, Parameter{
				Number: ref.ref.Number,
				Column: &Column{
					Name:     parameterName(ref.ref.Number, "offset"),
					DataType: "integer",
					NotNull:  true,
				},
			})

		case *limitCount:
			a = append(a, Parameter{
				Number: ref.ref.Number,
				Column: &Column{
					Name:     parameterName(ref.ref.Number, "limit"),
					DataType: "integer",
					NotNull:  true,
				},
			})

		case *pg.A_Expr:
			// TODO: While this works for a wide range of simple expressions,
			// more complicated expressions will cause this logic to fail.
			list := astutils.Search(n.Lexpr, func(node ast.Node) bool {
				_, ok := node.(*pg.ColumnRef)
				return ok
			})

			if len(list.Items) == 0 {
				return nil, &sqlerr.Error{
					Code:     "XXXXX",
					Message:  "no column reference found",
					Location: n.Location,
				}
			}

			switch left := list.Items[0].(type) {
			case *pg.ColumnRef:
				items := stringSlice(left.Fields)
				var key, alias string
				switch len(items) {
				case 1:
					key = items[0]
				case 2:
					alias = items[0]
					key = items[1]
				default:
					panic("too many field items: " + strconv.Itoa(len(items)))
				}

				search := tables
				if alias != "" {
					if original, ok := aliasMap[alias]; ok {
						search = []*ast.TableName{original}
					} else {
						for _, fqn := range tables {
							if fqn.Name == alias {
								search = []*ast.TableName{fqn}
							}
						}
					}
				}

				var found int
				for _, table := range search {
					if c, ok := typeMap[table.Schema][table.Name][key]; ok {
						found += 1
						if ref.name != "" {
							key = ref.name
						}
						a = append(a, Parameter{
							Number: ref.ref.Number,
							Column: &Column{
								Name:     parameterName(ref.ref.Number, key),
								DataType: dataType(&c.Type),
								NotNull:  c.IsNotNull,
								IsArray:  c.IsArray,
								Table:    table,
							},
						})
					}
				}

				if found == 0 {
					return nil, &sqlerr.Error{
						Code:     "42703",
						Message:  fmt.Sprintf("column \"%s\" does not exist", key),
						Location: left.Location,
					}
				}
				if found > 1 {
					return nil, &sqlerr.Error{
						Code:     "42703",
						Message:  fmt.Sprintf("column reference \"%s\" is ambiguous", key),
						Location: left.Location,
					}
				}
			}

		case *ast.FuncCall:
			fun, err := c.GetFuncN(n.Func, len(n.Args.Items))
			if err != nil {
				var args []*catalog.Argument
				for range n.Args.Items {
					args = append(args, &catalog.Argument{
						Type: &ast.TypeName{Name: "any"},
					})
				}
				// Synthesize a function on the fly to avoid returning with an error
				// for an unknown Postgres function (e.g. defined in an extension)
				fun = catalog.Function{
					Name:       n.Func.Name,
					Args:       args,
					ReturnType: &ast.TypeName{Name: "any"},
				}
			}
			for i, item := range n.Args.Items {
				defaultName := fun.Name
				switch inode := item.(type) {
				case *pg.ParamRef:
					if inode.Number != ref.ref.Number {
						continue
					}
				case *pg.NamedArgExpr:
					pr, ok := inode.Arg.(*pg.ParamRef)
					if !ok {
						continue
					}
					if pr.Number != ref.ref.Number {
						continue
					}
					if inode.Name != nil {
						defaultName = *inode.Name
					}
				}

				if fun.Args == nil {
					a = append(a, Parameter{
						Number: ref.ref.Number,
						Column: &Column{
							Name:     parameterName(ref.ref.Number, defaultName),
							DataType: "any",
						},
					})
					continue
				}

				if i >= len(fun.Args) {
					return nil, fmt.Errorf("incorrect number of arguments to %s", fun.Name)
				}
				arg := fun.Args[i]
				name := arg.Name
				if name == "" {
					name = defaultName
				}
				a = append(a, Parameter{
					Number: ref.ref.Number,
					Column: &Column{
						Name:     parameterName(ref.ref.Number, name),
						DataType: dataType(arg.Type),
						NotNull:  true,
					},
				})
			}

		case *pg.ResTarget:
			if n.Name == nil {
				return nil, fmt.Errorf("*pg.ResTarget has nil name")
			}
			key := *n.Name

			// TODO: Deprecate defaultTable
			schema := defaultTable.Schema
			rel := defaultTable.Name
			if ref.rv != nil {
				fqn, err := parseTableName(ref.rv)
				if err != nil {
					return nil, err
				}
				schema = fqn.Schema
				rel = fqn.Name
			}
			if c, ok := typeMap[schema][rel][key]; ok {
				a = append(a, Parameter{
					Number: ref.ref.Number,
					Column: &Column{
						Name:     parameterName(ref.ref.Number, key),
						DataType: dataType(&c.Type),
						NotNull:  c.IsNotNull,
						IsArray:  c.IsArray,
						Table:    &ast.TableName{Schema: schema, Name: rel},
					},
				})
			} else {
				return nil, &sqlerr.Error{
					Code:     "42703",
					Message:  fmt.Sprintf("column \"%s\" does not exist", key),
					Location: n.Location,
				}
			}

		case *pg.TypeCast:
			if n.TypeName == nil {
				return nil, fmt.Errorf("*pg.TypeCast has nil type name")
			}
			col := toColumn(n.TypeName)
			col.Name = parameterName(ref.ref.Number, col.Name)
			a = append(a, Parameter{
				Number: ref.ref.Number,
				Column: col,
			})

		case *pg.ParamRef:
			a = append(a, Parameter{Number: ref.ref.Number})

		default:
			fmt.Printf("unsupported reference type: %T", n)
		}
	}
	return a, nil
}
