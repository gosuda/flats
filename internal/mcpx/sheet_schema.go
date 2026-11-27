package mcpx

import "github.com/gosuda/flats/internal/sheet/formula"

func sheetSchemaInteger(minimum, maximum int) map[string]any {
	schema := map[string]any{"type": "integer", "minimum": minimum}
	if maximum > 0 {
		schema["maximum"] = maximum
	}
	return schema
}

// sheetSchemaWhole is a whole number of a write. A schema reads a number
// by its value and cannot tell 2.0 or -0 from 2 and 0. The sheet reads the
// digits and refuses them, so the description states the rule.
func sheetSchemaWhole(minimum, maximum int) map[string]any {
	schema := sheetSchemaInteger(minimum, maximum)
	schema["description"] = "Whole number written in digits, such as 2: not 2.0, 2e0 or -0, and at most 18 digits."
	return schema
}

func sheetSchemaBoolean() map[string]any { return map[string]any{"type": "boolean"} }

// sheetSchemaSelectors are the selectors of a sheet read: get_sheet takes
// them all, and get_revision takes them beside revision.
func sheetSchemaSelectors() map[string]any {
	return map[string]any{
		"tab":    map[string]any{"type": "string", "minLength": 1, "description": "Tab name. Without it the read lists the tabs."},
		"range":  map[string]any{"type": "string", "minLength": 1, "description": "Range of the tab such as A1:D9. Default: the used range."},
		"format": notebookSchemaEnum("json", "csv"),
		"include": map[string]any{"type": "array", "minItems": 1, "uniqueItems": true,
			"items": notebookSchemaEnum("shown", "formats", "widths")},
		"limit": sheetSchemaInteger(1, 10000),
	}
}

func sheetReadSchema() map[string]any {
	properties := sheetSchemaSelectors()
	properties["slug"] = notebookSchemaString()
	selected := notebookSchemaObject(properties, "tab", "range", "format", "include", "limit")
	selected["dependentRequired"] = map[string]any{"range": []string{"tab"}}
	cursor := map[string]any{"type": "string", "minLength": 1, "description": "next.cursor of the page before. It takes no other selector."}
	all := map[string]any{"cursor": cursor}
	for name, schema := range properties {
		all[name] = schema
	}
	schema := notebookSchemaObject(all, "tab", "range", "format", "include", "limit", "cursor")
	schema["oneOf"] = []any{selected, notebookSchemaObject(map[string]any{"slug": properties["slug"], "cursor": cursor})}
	return schema
}

func sheetUpdateSchema() map[string]any {
	tab := map[string]any{"type": "string", "minLength": 1, "description": "Tab name."}
	address := map[string]any{"type": "string", "minLength": 1, "description": "Cell address such as A1."}
	ranges := map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string", "minLength": 1}}
	count := sheetSchemaWhole(1, 0)
	op := notebookSchemaOperation
	settings := op("tab_settings", map[string]any{"tab": tab, "frozen_rows": sheetSchemaWhole(0, 20), "frozen_cols": sheetSchemaWhole(0, 10)}, "frozen_rows", "frozen_cols")
	settings["anyOf"] = []any{map[string]any{"required": []string{"frozen_rows"}}, map[string]any{"required": []string{"frozen_cols"}}}
	// The sheet reads an empty state or op_id as none, so a call without
	// the field and the same call with it empty are one request.
	state := map[string]any{"type": "string", "description": "state of the read the call is based on. " +
		"Only the call that creates the sheet and a call that only adds tabs may leave it out. An empty state is the same as none."}
	opID := map[string]any{"type": "string", "pattern": "^([A-Za-z0-9._:-]{1,128})?$",
		"description": "Id that makes a resend safe. An empty op_id is the same as none: the call is not protected."}
	schema := notebookSchemaObject(map[string]any{
		"slug":    notebookSchemaString(),
		"state":   state,
		"op_id":   opID,
		"message": map[string]any{"type": "string", "maxLength": 1000},
		"guard": notebookSchemaObject(map[string]any{
			"unchanged": sheetSchemaBoolean(),
			"ranges":    map[string]any{"type": "array", "items": map[string]any{"type": "string", "minLength": 1, "description": "Range with its tab such as Budget!B2:B8."}},
		}, "unchanged", "ranges"),
		"ops": notebookSchemaArray(notebookSchemaRef("Operation")),
	}, "state", "op_id", "message", "guard")
	schema["description"] = "The arguments are UTF-8 JSON. Write every field name exactly as shown and once in its object. " +
		"Nothing is null but a cell of rows, the value of a formula object and a property of a format."
	schema["$defs"] = map[string]any{
		"Cell": notebookSchemaUnion(
			map[string]any{"type": "number", "description": "Any number a double holds."},
			sheetSchemaBoolean(),
			map[string]any{"type": "null"},
			map[string]any{"type": "string", "description": "Text, or a formula when it starts with =. A leading apostrophe keeps the rest as text."},
			notebookSchemaObject(map[string]any{"date": map[string]any{"type": "string", "pattern": "^[0-9]{4}-[0-9]{2}-[0-9]{2}$"}}),
			notebookSchemaObject(map[string]any{"time": map[string]any{"type": "string", "pattern": "^[0-9]{2}:[0-9]{2}(:[0-9]{2})?$"}}),
			notebookSchemaObject(map[string]any{"datetime": map[string]any{"type": "string", "pattern": "^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}(:[0-9]{2})?$"}}),
			notebookSchemaObject(map[string]any{
				"formula": map[string]any{"type": "string", "pattern": `^=[\s\S]`, "description": "Only formula is written."},
				"value":   map[string]any{"type": []string{"number", "string", "boolean", "null"}, "description": "The value a read returned. Accepted and ignored."},
				"error":   map[string]any{"type": "string", "description": "The error a read returned, such as #DIV/0!. Accepted and ignored."},
			}, "value", "error"),
		),
		"Format": notebookSchemaObject(map[string]any{
			"bold": sheetSchemaToggle(), "italic": sheetSchemaToggle(), "underline": sheetSchemaToggle(), "strikethrough": sheetSchemaToggle(), "wrap": sheetSchemaToggle(),
			"text_color": sheetSchemaNullableString("Palette color such as red-600."),
			"fill":       sheetSchemaNullableString("Palette color such as red-600."),
			"align":      notebookSchemaNullable(notebookSchemaEnum("general", "left", "center", "right")),
			"number":     notebookSchemaNullable(sheetSchemaNumberFormat()),
		}, "bold", "italic", "underline", "strikethrough", "wrap", "text_color", "fill", "align", "number"),
		"Operation": notebookSchemaUnion(
			op("write", map[string]any{"tab": tab, "at": address, "rows": notebookSchemaArray(notebookSchemaArray(notebookSchemaRef("Cell")))}),
			op("clear", map[string]any{"tab": tab, "ranges": ranges, "formats": sheetSchemaBoolean()}, "formats"),
			op("format", map[string]any{"tab": tab, "ranges": ranges, "format": notebookSchemaRef("Format")}),
			op("widths", map[string]any{"tab": tab, "cols": map[string]any{"type": "string", "description": "Whole columns such as B:D."}, "width": sheetSchemaWhole(40, 1000)}),
			op("insert_rows", map[string]any{"tab": tab, "at": sheetSchemaWhole(1, 0), "count": count}),
			op("delete_rows", map[string]any{"tab": tab, "at": sheetSchemaWhole(1, 0), "count": count}),
			op("insert_cols", map[string]any{"tab": tab, "at": map[string]any{"type": "string", "description": "Column letters such as C."}, "count": count}),
			op("delete_cols", map[string]any{"tab": tab, "at": map[string]any{"type": "string", "description": "Column letters such as C."}, "count": count}),
			op("sort", map[string]any{"tab": tab, "rows": map[string]any{"type": "string", "description": "Whole rows such as 2:200."}, "by": map[string]any{"type": "string", "description": "Column letters."}, "desc": sheetSchemaBoolean()}, "desc"),
			op("add_tab", map[string]any{"name": notebookSchemaString(), "at": sheetSchemaWhole(1, 0)}, "at"),
			op("rename_tab", map[string]any{"tab": tab, "name": notebookSchemaString()}),
			op("move_tab", map[string]any{"tab": tab, "to": sheetSchemaWhole(1, 0)}),
			op("delete_tab", map[string]any{"tab": tab}),
			settings,
			op("import_csv", map[string]any{"tab": tab, "at": address, "csv": notebookSchemaString()}),
		),
	}
	return schema
}

func sheetSchemaToggle() map[string]any {
	return notebookSchemaNullable(sheetSchemaBoolean())
}

func sheetSchemaNullableString(description string) map[string]any {
	return notebookSchemaNullable(map[string]any{"type": "string", "description": description})
}

// sheetSchemaNumberFormat is the number format of a cell format: one object
// per kind, each with exactly the fields the sheet reads for that kind.
func sheetSchemaNumberFormat() map[string]any {
	decimals := sheetSchemaWhole(0, 10)
	dates := notebookSchemaEnum(formula.DateISO.String(), formula.DateMDY.String(), formula.DateMonth.String())
	times := notebookSchemaEnum(formula.TimeHM.String(), formula.TimeHMS.String(), formula.TimeAMPM.String())
	symbol := map[string]any{"type": "string", "minLength": 1, "maxLength": 3, "pattern": `^[\p{L}\p{Sc}]+$`,
		"description": "1 to 3 characters, each a letter or a currency sign."}
	schema := notebookSchemaUnion(
		notebookSchemaObject(map[string]any{"kind": notebookSchemaEnum("general", "text")}),
		notebookSchemaObject(map[string]any{"kind": notebookSchemaEnum("number"), "decimals": decimals, "thousands": sheetSchemaBoolean()}),
		notebookSchemaObject(map[string]any{"kind": notebookSchemaEnum("percent"), "decimals": decimals}),
		notebookSchemaObject(map[string]any{"kind": notebookSchemaEnum("currency"), "symbol": symbol, "decimals": decimals}),
		notebookSchemaObject(map[string]any{"kind": notebookSchemaEnum("date"), "pattern": dates}),
		notebookSchemaObject(map[string]any{"kind": notebookSchemaEnum("time"), "pattern": times}),
		notebookSchemaObject(map[string]any{"kind": notebookSchemaEnum("datetime"), "date": dates, "time": times}),
	)
	schema["description"] = `Number format by kind. Each kind takes exactly these fields, all required: ` +
		`{"kind":"general"}; {"kind":"text"}; {"kind":"number","decimals":2,"thousands":true}; {"kind":"percent","decimals":1}; ` +
		`{"kind":"currency","symbol":"$","decimals":2}; {"kind":"date","pattern":"yyyy-mm-dd"}; {"kind":"time","pattern":"hh:mm"}; ` +
		`{"kind":"datetime","date":"yyyy-mm-dd","time":"hh:mm"}. decimals is 0 to 10. Date patterns: yyyy-mm-dd, m/d/yyyy, "mmm d, yyyy". ` +
		`Time patterns: hh:mm, hh:mm:ss, h:mm AM/PM.`
	return schema
}
