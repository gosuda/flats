package mcpx

import "slices"

func notebookReadSchema() map[string]any {
	properties := map[string]any{
		"slug":       notebookSchemaString(),
		"revision":   map[string]any{"type": "integer", "minimum": 1},
		"page":       notebookSchemaSelector("Page"),
		"database":   notebookSchemaSelector("Database"),
		"view":       notebookSchemaSelector("View"),
		"format":     notebookSchemaEnum("structured", "markdown"),
		"sort":       notebookSchemaRef("Sort"),
		"filter":     notebookSchemaRef("Filter"),
		"limit":      map[string]any{"type": "integer", "minimum": 1, "maximum": 2000},
		"cursor":     map[string]any{"type": "string", "minLength": 1},
		"op_id":      notebookSchemaOpID(),
		"retry_hint": map[string]any{"type": "string", "minLength": 1, "description": "Admission ticket from an unavailable read; repeat the original selectors unchanged before expiry."},
	}
	branch := func(names []string, optional ...string) map[string]any {
		fields := make(map[string]any, len(names))
		for _, name := range names {
			fields[name] = properties[name]
		}
		return notebookSchemaObject(fields, optional...)
	}
	tree := branch([]string{"slug", "revision", "format", "limit", "retry_hint"}, "revision", "format", "limit", "retry_hint")
	tree["properties"].(map[string]any)["format"] = notebookSchemaEnum("structured")
	page := branch([]string{"slug", "revision", "page", "format", "limit", "retry_hint"}, "revision", "format", "limit", "retry_hint")
	database := branch(
		[]string{"slug", "revision", "database", "format", "sort", "filter", "limit", "retry_hint"},
		"revision", "format", "sort", "filter", "limit", "retry_hint",
	)
	database["properties"].(map[string]any)["format"] = notebookSchemaEnum("structured")
	view := branch(
		[]string{"slug", "revision", "database", "view", "format", "limit", "retry_hint"},
		"revision", "format", "limit", "retry_hint",
	)
	view["properties"].(map[string]any)["format"] = notebookSchemaEnum("structured")
	schema := notebookSchemaObject(
		properties,
		"revision", "page", "database", "view", "format", "sort", "filter", "limit", "cursor", "op_id", "retry_hint",
	)
	schema["oneOf"] = []any{
		tree, page, database, view,
		branch([]string{"slug", "cursor"}),
		branch([]string{"slug", "op_id", "cursor"}),
	}
	schema["$defs"] = notebookSchemaDefinitions(false)
	return schema
}

func notebookUpdateSchema() map[string]any {
	defs := notebookSchemaDefinitions(true)
	defs["Placement"] = notebookSchemaUnion(
		notebookSchemaObject(map[string]any{
			"page":   notebookSchemaRef("PageTarget"),
			"parent": notebookSchemaNullable(notebookSchemaRef("BlockTarget")),
			"before": notebookSchemaNullable(notebookSchemaRef("BlockTarget")),
		}),
		notebookSchemaObject(map[string]any{
			"page":   notebookSchemaRef("PageTarget"),
			"parent": notebookSchemaNullable(notebookSchemaRef("BlockTarget")),
			"after":  notebookSchemaRef("BlockTarget"),
		}),
	)
	defs["PropertyDef"] = notebookSchemaUnion(
		notebookSchemaObject(map[string]any{
			"name": notebookSchemaString(),
			"type": notebookSchemaEnum("title", "text", "select", "multi_select", "date", "checkbox", "url"),
		}),
		notebookSchemaObject(map[string]any{
			"name":   notebookSchemaString(),
			"type":   notebookSchemaEnum("number"),
			"format": notebookSchemaEnum("plain", "comma", "percent"),
		}, "format"),
	)
	defs["CellInput"] = notebookSchemaObject(map[string]any{
		"property": notebookSchemaRef("PropertyTarget"),
		// The live property type selects the input arm when the write applies.
		"value": notebookSchemaUnion(
			notebookSchemaString(),
			map[string]any{"type": "boolean"},
			map[string]any{"type": "null"},
			notebookSchemaRef("OptionTarget"),
			notebookSchemaArray(notebookSchemaRef("OptionTarget")),
		),
	})
	defs["ViewDef"] = notebookSchemaView()
	defs["CreateContent"] = notebookSchemaContent(false)
	defs["Content"] = notebookSchemaContent(true)
	defs["Guards"] = notebookSchemaObject(map[string]any{
		"state": notebookSchemaObject(map[string]any{
			"store": map[string]any{"type": "string", "minLength": 1},
			"seq":   notebookSchemaDecimal(),
		}),
		"items": notebookSchemaArray(notebookSchemaObject(map[string]any{
			"ref": notebookSchemaRef("AnyRef"),
			"ver": notebookSchemaDecimal(),
		})),
	}, "state", "items")
	defs["AssetRef"] = notebookSchemaObject(map[string]any{
		"path": map[string]any{"type": "string", "minLength": 1},
		"asset": notebookSchemaUnion(
			notebookSchemaHash(),
			notebookSchemaRef("TemporaryTarget"),
		),
	})
	schema := notebookSchemaObject(map[string]any{
		"slug":    notebookSchemaString(),
		"op_id":   notebookSchemaOpID(),
		"message": notebookSchemaString(),
		"guards":  notebookSchemaRef("Guards"),
		"ops":     notebookSchemaArray(notebookSchemaOperations()),
		"assets": notebookSchemaArray(notebookSchemaObject(map[string]any{
			"ref":  notebookSchemaName(),
			"data": map[string]any{"type": "string", "contentEncoding": "base64"},
		})),
	}, "message", "guards", "assets")
	schema["$defs"] = defs
	return schema
}

func notebookSchemaOperations() map[string]any {
	page := notebookSchemaRef("PageTarget")
	block := notebookSchemaRef("BlockTarget")
	database := notebookSchemaRef("DatabaseTarget")
	property := notebookSchemaRef("PropertyTarget")
	option := notebookSchemaRef("OptionTarget")
	row := notebookSchemaRef("RowTarget")
	view := notebookSchemaRef("ViewTarget")
	place := notebookSchemaRef("Placement")
	name := notebookSchemaName()
	text := notebookSchemaString()
	propertyDef := notebookSchemaRef("PropertyDef")
	viewDef := notebookSchemaRef("ViewDef")
	color := notebookSchemaEnum("gray", "brown", "orange", "yellow", "green", "blue", "purple", "pink", "red")
	return notebookSchemaUnion(
		notebookSchemaOperation("create_page", map[string]any{
			"ref": name, "parent": notebookSchemaNullable(page), "before": notebookSchemaNullable(page), "title": text,
		}),
		notebookSchemaOperation("set_page_title", map[string]any{"page": page, "title": text}),
		notebookSchemaOperation("move_page", map[string]any{
			"page": page, "parent": notebookSchemaNullable(page), "before": notebookSchemaNullable(page),
		}),
		notebookSchemaOperation("delete_page", map[string]any{"page": page}),
		notebookSchemaOperation("duplicate_page", map[string]any{"page": page}),
		notebookSchemaOperation("create_block", map[string]any{
			"ref": name, "place": place, "content": notebookSchemaRef("CreateContent"),
		}),
		notebookSchemaOperation("set_block", map[string]any{
			"block": block, "content": notebookSchemaRef("Content"), "base_v": notebookSchemaDecimal(),
		}, "base_v"),
		notebookSchemaOperation("turn_blocks", map[string]any{
			"blocks": notebookSchemaArray(block),
			"kind": notebookSchemaEnum(
				"paragraph", "heading1", "heading2", "heading3", "bullet", "numbered",
				"todo", "toggle", "quote", "callout", "code",
			),
		}),
		notebookSchemaOperation("move_blocks", map[string]any{"blocks": notebookSchemaArray(block), "place": place}),
		notebookSchemaOperation("delete_blocks", map[string]any{"blocks": notebookSchemaArray(block)}),
		notebookSchemaOperation("duplicate_blocks", map[string]any{
			"blocks": notebookSchemaArray(block), "after": block,
		}),
		notebookSchemaOperation("create_database", map[string]any{
			"ref": name, "block_ref": name, "place": place, "title": text, "title_property_name": text,
		}, "title_property_name"),
		notebookSchemaOperation("set_database_title", map[string]any{"database": database, "title": text}),
		notebookSchemaOperation("create_property", map[string]any{
			"ref": name, "database": database, "definition": propertyDef,
		}),
		notebookSchemaOperation("set_property", map[string]any{
			"property": property, "definition": propertyDef, "confirmed_preview": text,
		}, "confirmed_preview"),
		notebookSchemaOperation("delete_property", map[string]any{"property": property}),
		notebookSchemaOperation("set_property_order", map[string]any{
			"database": database, "properties": notebookSchemaArray(property),
		}),
		notebookSchemaOperation("create_option", map[string]any{
			"ref": name, "property": property, "name": text, "color": color,
		}),
		notebookSchemaOperation("set_option", map[string]any{"option": option, "name": text, "color": color}),
		notebookSchemaOperation("delete_option", map[string]any{"option": option}),
		notebookSchemaOperation("set_option_order", map[string]any{
			"property": property, "options": notebookSchemaArray(option),
		}),
		notebookSchemaOperation("create_row", map[string]any{
			"ref": name, "database": database, "cells": notebookSchemaArray(notebookSchemaRef("CellInput")),
			"before": notebookSchemaNullable(row), "view": view,
		}, "view"),
		notebookSchemaOperation("set_cell", map[string]any{"row": row, "cell": notebookSchemaRef("CellInput")}),
		notebookSchemaOperation("move_rows", map[string]any{
			"rows": notebookSchemaArray(row), "before": notebookSchemaNullable(row),
		}),
		notebookSchemaOperation("delete_rows", map[string]any{"rows": notebookSchemaArray(row)}),
		notebookSchemaOperation("create_view", map[string]any{"ref": name, "database": database, "definition": viewDef}),
		notebookSchemaOperation("set_view", map[string]any{"view": view, "definition": viewDef}),
		notebookSchemaOperation("delete_view", map[string]any{"view": view}),
		notebookSchemaOperation("set_view_order", map[string]any{"database": database, "views": notebookSchemaArray(view)}),
		notebookSchemaOperation("markdown_to_blocks", map[string]any{
			"place": place, "markdown": text, "asset_refs": notebookSchemaArray(notebookSchemaRef("AssetRef")),
		}, "asset_refs"),
	)
}

func notebookSchemaDefinitions(temporary bool) map[string]any {
	defs := map[string]any{}
	if temporary {
		defs["TemporaryTarget"] = notebookSchemaObject(map[string]any{"tmp": notebookSchemaName()})
	}
	for kind, prefix := range map[string]string{
		"Page": "p", "Block": "b", "Database": "d", "Property": "pr", "Option": "o", "Row": "r", "View": "v",
		"Any": "(p|b|d|pr|o|r|v)",
	} {
		defs[kind+"ID"] = map[string]any{"type": "string", "pattern": "^" + prefix + "_[a-z2-7]{25}[aeimquy4]$"}
		defs[kind+"Ref"] = notebookSchemaObject(map[string]any{
			"id":  notebookSchemaRef(kind + "ID"),
			"inc": map[string]any{"type": "string", "pattern": "^[1-9][0-9]*$", "maxLength": 20},
		})
		if temporary {
			defs[kind+"Target"] = notebookSchemaUnion(
				notebookSchemaRef(kind+"Ref"),
				notebookSchemaRef("TemporaryTarget"),
			)
		} else {
			defs[kind+"Target"] = notebookSchemaRef(kind + "Ref")
		}
	}
	defs["Sort"] = notebookSchemaArray(notebookSchemaObject(map[string]any{
		"property":  notebookSchemaRef("PropertyTarget"),
		"direction": notebookSchemaEnum("asc", "desc"),
	}))
	defs["Filter"] = notebookSchemaFilter()
	return defs
}

func notebookSchemaFilter() map[string]any {
	condition := func(operators []string, value map[string]any) map[string]any {
		fields := map[string]any{
			"property": notebookSchemaRef("PropertyTarget"),
			"op":       notebookSchemaEnum(operators...),
		}
		if value != nil {
			fields["value"] = value
		}
		return notebookSchemaObject(fields)
	}
	return notebookSchemaArray(notebookSchemaUnion(
		condition([]string{"is empty", "is not empty", "is checked", "is not checked"}, nil),
		condition([]string{"is", "is not", "contains", "does not contain"}, notebookSchemaUnion(
			notebookSchemaString(), notebookSchemaRef("OptionTarget"),
		)),
		condition([]string{"=", "≠", "<", "≤", ">", "≥"}, map[string]any{
			"type": "string", "pattern": `^[+-]?([0-9]+(\.[0-9]*)?|\.[0-9]+)([eE][+-]?[0-9]+)?$`,
		}),
		condition([]string{"before", "after", "on or before", "on or after"}, map[string]any{
			"type": "string", "pattern": `^[0-9]{4}-[0-9]{2}-[0-9]{2}(T[0-9]{2}:[0-9]{2}:[0-9]{2})?$`,
		}),
		condition([]string{"is any of"}, notebookSchemaArray(notebookSchemaRef("OptionTarget"))),
	))
}

func notebookSchemaView() map[string]any {
	property := notebookSchemaRef("PropertyTarget")
	return notebookSchemaObject(map[string]any{
		"name": notebookSchemaString(),
		"display": notebookSchemaUnion(
			notebookSchemaObject(map[string]any{
				"layout":  notebookSchemaEnum("table"),
				"visible": notebookSchemaArray(property),
				"widths": notebookSchemaArray(notebookSchemaObject(map[string]any{
					"property": property,
					"px":       map[string]any{"type": "integer", "minimum": 80, "maximum": 1200},
				})),
			}),
			notebookSchemaObject(map[string]any{
				"layout":     notebookSchemaEnum("kanban"),
				"group_by":   property,
				"visible":    notebookSchemaArray(property),
				"show_empty": map[string]any{"type": "boolean"},
			}),
		),
		"sort":   notebookSchemaRef("Sort"),
		"filter": notebookSchemaRef("Filter"),
	})
}

func notebookSchemaContent(database bool) map[string]any {
	runs := notebookSchemaArray(notebookSchemaObject(map[string]any{
		"text":  notebookSchemaString(),
		"marks": notebookSchemaArray(notebookSchemaEnum("bold", "italic", "strike", "code")),
		"link": notebookSchemaUnion(
			notebookSchemaObject(map[string]any{"url": notebookSchemaString()}),
			notebookSchemaObject(map[string]any{"page": notebookSchemaRef("PageID")}),
		),
	}, "link"))
	variants := []map[string]any{}
	for _, kind := range []string{
		"paragraph", "heading1", "heading2", "heading3", "bullet", "numbered", "todo", "toggle", "quote", "callout",
	} {
		fields := map[string]any{"kind": notebookSchemaEnum(kind), "text": runs}
		optional := []string{}
		switch kind {
		case "todo":
			fields["checked"] = map[string]any{"type": "boolean"}
			optional = append(optional, "checked")
		case "toggle":
			fields["open_default"] = map[string]any{"type": "boolean"}
			optional = append(optional, "open_default")
		case "callout":
			fields["tone"] = notebookSchemaEnum("neutral", "info", "success", "warning", "danger")
			optional = append(optional, "tone")
		}
		variants = append(variants, notebookSchemaObject(fields, optional...))
	}
	variants = append(variants,
		notebookSchemaObject(map[string]any{
			"kind": notebookSchemaEnum("code"), "text": notebookSchemaString(), "language": notebookSchemaString(),
		}),
		notebookSchemaObject(map[string]any{"kind": notebookSchemaEnum("divider")}),
		notebookSchemaObject(map[string]any{
			"kind": notebookSchemaEnum("image"), "asset": notebookSchemaHash(),
			"alt": notebookSchemaString(), "caption": runs,
		}),
		notebookSchemaObject(map[string]any{
			"kind": notebookSchemaEnum("page_link"), "target": notebookSchemaRef("PageID"),
		}),
	)
	if database {
		variants = append(variants, notebookSchemaObject(map[string]any{
			"kind": notebookSchemaEnum("database"), "database": notebookSchemaRef("DatabaseID"),
		}))
	}
	return notebookSchemaUnion(variants...)
}

func notebookSchemaOperation(name string, fields map[string]any, optional ...string) map[string]any {
	fields["op"] = notebookSchemaEnum(name)
	return notebookSchemaObject(fields, optional...)
}

func notebookSchemaObject(properties map[string]any, optional ...string) map[string]any {
	required := make([]string, 0, len(properties))
	for name := range properties {
		if !slices.Contains(optional, name) {
			required = append(required, name)
		}
	}
	slices.Sort(required)
	return map[string]any{
		"type": "object", "properties": properties, "required": required, "additionalProperties": false,
	}
}

func notebookSchemaUnion(variants ...map[string]any) map[string]any {
	return map[string]any{"oneOf": variants}
}

func notebookSchemaNullable(schema map[string]any) map[string]any {
	return notebookSchemaUnion(schema, map[string]any{"type": "null"})
}

func notebookSchemaRef(name string) map[string]any {
	return map[string]any{"$ref": "#/$defs/" + name}
}

func notebookSchemaSelector(kind string) map[string]any {
	return notebookSchemaUnion(notebookSchemaRef(kind+"ID"), notebookSchemaRef(kind+"Ref"))
}

func notebookSchemaArray(items map[string]any) map[string]any {
	return map[string]any{"type": "array", "items": items}
}

func notebookSchemaEnum(values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values}
}

func notebookSchemaString() map[string]any {
	return map[string]any{"type": "string"}
}

func notebookSchemaName() map[string]any {
	return map[string]any{"type": "string", "pattern": "^[!-~]{1,128}$"}
}

func notebookSchemaDecimal() map[string]any {
	return map[string]any{"type": "string", "pattern": "^(0|[1-9][0-9]*)$", "maxLength": 20}
}

func notebookSchemaOpID() map[string]any {
	return map[string]any{"type": "string", "pattern": "^[A-Za-z0-9._:-]{1,128}$"}
}

func notebookSchemaHash() map[string]any {
	return map[string]any{"type": "string", "pattern": "^[0-9a-f]{64}$"}
}
