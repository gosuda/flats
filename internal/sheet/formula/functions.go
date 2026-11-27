package formula

// fnID names a function of spec rule 12. The constants are in the order of
// funcTable, which is sorted by name.
type fnID uint8

const (
	fnABS fnID = iota
	fnAND
	fnAVERAGE
	fnAVERAGEIF
	fnAVERAGEIFS
	fnCEILING
	fnCHOOSE
	fnCOLUMN
	fnCOLUMNS
	fnCONCAT
	fnCONCATENATE
	fnCOUNT
	fnCOUNTA
	fnCOUNTBLANK
	fnCOUNTIF
	fnCOUNTIFS
	fnDATE
	fnDATEDIF
	fnDAY
	fnDAYS
	fnEDATE
	fnEOMONTH
	fnEXACT
	fnEXP
	fnFALSE
	fnFIND
	fnFLOOR
	fnHLOOKUP
	fnHOUR
	fnIF
	fnIFERROR
	fnIFNA
	fnIFS
	fnINDEX
	fnINT
	fnISBLANK
	fnISERROR
	fnISLOGICAL
	fnISNA
	fnISNUMBER
	fnISTEXT
	fnLARGE
	fnLEFT
	fnLEN
	fnLN
	fnLOG
	fnLOG10
	fnLOWER
	fnMATCH
	fnMAX
	fnMAXIFS
	fnMEDIAN
	fnMID
	fnMIN
	fnMINIFS
	fnMINUTE
	fnMOD
	fnMONTH
	fnNA
	fnNOT
	fnOR
	fnPI
	fnPOWER
	fnPRODUCT
	fnPROPER
	fnQUOTIENT
	fnRANK
	fnREPLACE
	fnREPT
	fnRIGHT
	fnROUND
	fnROUNDDOWN
	fnROUNDUP
	fnROW
	fnROWS
	fnSEARCH
	fnSECOND
	fnSIGN
	fnSMALL
	fnSQRT
	fnSTDEV
	fnSTDEVP
	fnSUBSTITUTE
	fnSUM
	fnSUMIF
	fnSUMIFS
	fnSUMPRODUCT
	fnSWITCH
	fnTEXT
	fnTEXTJOIN
	fnTIME
	fnTODAY
	fnTRIM
	fnTRUE
	fnTRUNC
	fnUPPER
	fnVALUE
	fnVAR
	fnVARP
	fnVLOOKUP
	fnWEEKDAY
	fnXLOOKUP
	fnXOR
	fnYEAR

	fnCount
	fnUnknown fnID = 255 // a name that is no function: #NAME?
)

const (
	groupMath        = "Math"
	groupStatistics  = "Statistics"
	groupConditional = "Conditional totals"
	groupLogic       = "Logic"
	groupText        = "Text"
	groupLookup      = "Lookup"
	groupDate        = "Date and time"
)

const anyCount = -1

// funcSpec describes one function. max is anyCount for a function without
// a largest number of arguments. When pairs is true, the arguments after
// the first pairFrom come in pairs.
type funcSpec struct {
	name     string
	group    string
	min, max int
	pairs    bool
	pairFrom int
	args     string // argument names, separated by ", "
	desc     string
}

var funcTable = [fnCount]funcSpec{
	fnABS:         {name: "ABS", group: groupMath, min: 1, max: 1, args: "number", desc: "The number without its sign."},
	fnAND:         {name: "AND", group: groupLogic, min: 1, max: anyCount, args: "condition1, [condition2, ...]", desc: "TRUE when every condition is true."},
	fnAVERAGE:     {name: "AVERAGE", group: groupStatistics, min: 1, max: anyCount, args: "number1, [number2, ...]", desc: "The mean of the numbers."},
	fnAVERAGEIF:   {name: "AVERAGEIF", group: groupConditional, min: 2, max: 3, args: "range, criteria, [average_range]", desc: "The mean of the cells that meet the criteria."},
	fnAVERAGEIFS:  {name: "AVERAGEIFS", group: groupConditional, min: 3, max: anyCount, pairs: true, pairFrom: 1, args: "average_range, range1, criteria1, [range2, criteria2, ...]", desc: "The mean of the cells that meet every criteria."},
	fnCEILING:     {name: "CEILING", group: groupMath, min: 1, max: 2, args: "number, [significance]", desc: "The number rounded up to a multiple of the significance."},
	fnCHOOSE:      {name: "CHOOSE", group: groupLookup, min: 2, max: anyCount, args: "index, value1, [value2, ...]", desc: "The value at the index in the list."},
	fnCOLUMN:      {name: "COLUMN", group: groupLookup, min: 0, max: 1, args: "[reference]", desc: "The column number of the reference, or of this cell."},
	fnCOLUMNS:     {name: "COLUMNS", group: groupLookup, min: 1, max: 1, args: "range", desc: "The number of columns in the range."},
	fnCONCAT:      {name: "CONCAT", group: groupText, min: 1, max: anyCount, args: "text1, [text2, ...]", desc: "The texts and ranges joined into one text."},
	fnCONCATENATE: {name: "CONCATENATE", group: groupText, min: 1, max: anyCount, args: "text1, [text2, ...]", desc: "The texts joined into one text."},
	fnCOUNT:       {name: "COUNT", group: groupStatistics, min: 1, max: anyCount, args: "value1, [value2, ...]", desc: "How many of the values are numbers."},
	fnCOUNTA:      {name: "COUNTA", group: groupStatistics, min: 1, max: anyCount, args: "value1, [value2, ...]", desc: "How many of the cells are filled."},
	fnCOUNTBLANK:  {name: "COUNTBLANK", group: groupStatistics, min: 1, max: 1, args: "range", desc: "How many cells of the range are blank or hold empty text."},
	fnCOUNTIF:     {name: "COUNTIF", group: groupConditional, min: 2, max: 2, args: "range, criteria", desc: "How many cells meet the criteria."},
	fnCOUNTIFS:    {name: "COUNTIFS", group: groupConditional, min: 2, max: anyCount, pairs: true, args: "range1, criteria1, [range2, criteria2, ...]", desc: "How many rows meet every criteria."},
	fnDATE:        {name: "DATE", group: groupDate, min: 3, max: 3, args: "year, month, day", desc: "The date of a year, a month and a day."},
	fnDATEDIF:     {name: "DATEDIF", group: groupDate, min: 3, max: 3, args: "start_date, end_date, unit", desc: "The time between two dates in the unit Y, M, D, MD, YM or YD."},
	fnDAY:         {name: "DAY", group: groupDate, min: 1, max: 1, args: "date", desc: "The day of the month of a date."},
	fnDAYS:        {name: "DAYS", group: groupDate, min: 2, max: 2, args: "end_date, start_date", desc: "The days from the start date to the end date."},
	fnEDATE:       {name: "EDATE", group: groupDate, min: 2, max: 2, args: "start_date, months", desc: "The date a number of months before or after a date."},
	fnEOMONTH:     {name: "EOMONTH", group: groupDate, min: 2, max: 2, args: "start_date, months", desc: "The last day of the month a number of months before or after a date."},
	fnEXACT:       {name: "EXACT", group: groupText, min: 2, max: 2, args: "text1, text2", desc: "TRUE when the two texts are the same, capitals included."},
	fnEXP:         {name: "EXP", group: groupMath, min: 1, max: 1, args: "number", desc: "e raised to the power of the number."},
	fnFALSE:       {name: "FALSE", group: groupLogic, min: 0, max: 0, args: "", desc: "The value FALSE."},
	fnFIND:        {name: "FIND", group: groupText, min: 2, max: 3, args: "search_for, text, [start]", desc: "The position of one text in another, capitals included."},
	fnFLOOR:       {name: "FLOOR", group: groupMath, min: 1, max: 2, args: "number, [significance]", desc: "The number rounded down to a multiple of the significance."},
	fnHLOOKUP:     {name: "HLOOKUP", group: groupLookup, min: 3, max: 4, args: "key, range, row_index, [sorted]", desc: "Finds a key in the first row of a range and returns the value below it."},
	fnHOUR:        {name: "HOUR", group: groupDate, min: 1, max: 1, args: "time", desc: "The hour of a time."},
	fnIF:          {name: "IF", group: groupLogic, min: 2, max: 3, args: "condition, value_if_true, [value_if_false]", desc: "One value when the condition is true and another when it is false."},
	fnIFERROR:     {name: "IFERROR", group: groupLogic, min: 1, max: 2, args: "value, [value_if_error]", desc: "The value, or another value when the first is an error."},
	fnIFNA:        {name: "IFNA", group: groupLogic, min: 1, max: 2, args: "value, [value_if_na]", desc: "The value, or another value when the first is #N/A."},
	fnIFS:         {name: "IFS", group: groupLogic, min: 2, max: anyCount, pairs: true, args: "condition1, value1, [condition2, value2, ...]", desc: "The value of the first condition that is true."},
	fnINDEX:       {name: "INDEX", group: groupLookup, min: 2, max: 3, args: "range, row, [column]", desc: "The value at a row and a column of a range."},
	fnINT:         {name: "INT", group: groupMath, min: 1, max: 1, args: "number", desc: "The number rounded down to a whole number."},
	fnISBLANK:     {name: "ISBLANK", group: groupLogic, min: 1, max: 1, args: "value", desc: "TRUE when the cell is blank."},
	fnISERROR:     {name: "ISERROR", group: groupLogic, min: 1, max: 1, args: "value", desc: "TRUE when the value is an error."},
	fnISLOGICAL:   {name: "ISLOGICAL", group: groupLogic, min: 1, max: 1, args: "value", desc: "TRUE when the value is TRUE or FALSE."},
	fnISNA:        {name: "ISNA", group: groupLogic, min: 1, max: 1, args: "value", desc: "TRUE when the value is the error #N/A."},
	fnISNUMBER:    {name: "ISNUMBER", group: groupLogic, min: 1, max: 1, args: "value", desc: "TRUE when the value is a number."},
	fnISTEXT:      {name: "ISTEXT", group: groupLogic, min: 1, max: 1, args: "value", desc: "TRUE when the value is text."},
	fnLARGE:       {name: "LARGE", group: groupStatistics, min: 2, max: 2, args: "range, k", desc: "The k-th largest number of a range."},
	fnLEFT:        {name: "LEFT", group: groupText, min: 1, max: 2, args: "text, [count]", desc: "The first characters of a text."},
	fnLEN:         {name: "LEN", group: groupText, min: 1, max: 1, args: "text", desc: "The number of characters in a text."},
	fnLN:          {name: "LN", group: groupMath, min: 1, max: 1, args: "number", desc: "The natural logarithm of the number."},
	fnLOG:         {name: "LOG", group: groupMath, min: 1, max: 2, args: "number, [base]", desc: "The logarithm of the number to a base, 10 when none is given."},
	fnLOG10:       {name: "LOG10", group: groupMath, min: 1, max: 1, args: "number", desc: "The logarithm of the number to base 10."},
	fnLOWER:       {name: "LOWER", group: groupText, min: 1, max: 1, args: "text", desc: "The text in small letters."},
	fnMATCH:       {name: "MATCH", group: groupLookup, min: 2, max: 3, args: "key, range, [match_type]", desc: "The position of a key in a row or a column."},
	fnMAX:         {name: "MAX", group: groupStatistics, min: 1, max: anyCount, args: "number1, [number2, ...]", desc: "The largest of the numbers."},
	fnMAXIFS:      {name: "MAXIFS", group: groupConditional, min: 3, max: anyCount, pairs: true, pairFrom: 1, args: "max_range, range1, criteria1, [range2, criteria2, ...]", desc: "The largest of the cells that meet every criteria."},
	fnMEDIAN:      {name: "MEDIAN", group: groupStatistics, min: 1, max: anyCount, args: "number1, [number2, ...]", desc: "The middle of the numbers."},
	fnMID:         {name: "MID", group: groupText, min: 3, max: 3, args: "text, start, count", desc: "Characters from the middle of a text."},
	fnMIN:         {name: "MIN", group: groupStatistics, min: 1, max: anyCount, args: "number1, [number2, ...]", desc: "The smallest of the numbers."},
	fnMINIFS:      {name: "MINIFS", group: groupConditional, min: 3, max: anyCount, pairs: true, pairFrom: 1, args: "min_range, range1, criteria1, [range2, criteria2, ...]", desc: "The smallest of the cells that meet every criteria."},
	fnMINUTE:      {name: "MINUTE", group: groupDate, min: 1, max: 1, args: "time", desc: "The minute of a time."},
	fnMOD:         {name: "MOD", group: groupMath, min: 2, max: 2, args: "number, divisor", desc: "The remainder of a division, with the sign of the divisor."},
	fnMONTH:       {name: "MONTH", group: groupDate, min: 1, max: 1, args: "date", desc: "The month of a date."},
	fnNA:          {name: "NA", group: groupLogic, min: 0, max: 0, args: "", desc: "The error #N/A."},
	fnNOT:         {name: "NOT", group: groupLogic, min: 1, max: 1, args: "condition", desc: "TRUE when the condition is false."},
	fnOR:          {name: "OR", group: groupLogic, min: 1, max: anyCount, args: "condition1, [condition2, ...]", desc: "TRUE when any condition is true."},
	fnPI:          {name: "PI", group: groupMath, min: 0, max: 0, args: "", desc: "The number pi."},
	fnPOWER:       {name: "POWER", group: groupMath, min: 2, max: 2, args: "number, power", desc: "The number raised to a power."},
	fnPRODUCT:     {name: "PRODUCT", group: groupMath, min: 1, max: anyCount, args: "number1, [number2, ...]", desc: "The numbers multiplied together."},
	fnPROPER:      {name: "PROPER", group: groupText, min: 1, max: 1, args: "text", desc: "The text with a capital at the start of each word."},
	fnQUOTIENT:    {name: "QUOTIENT", group: groupMath, min: 2, max: 2, args: "number, divisor", desc: "The whole part of a division."},
	fnRANK:        {name: "RANK", group: groupStatistics, min: 2, max: 3, args: "number, range, [ascending]", desc: "The rank of a number among the numbers of a range."},
	fnREPLACE:     {name: "REPLACE", group: groupText, min: 4, max: 4, args: "text, start, count, new_text", desc: "The text with the characters at a position replaced."},
	fnREPT:        {name: "REPT", group: groupText, min: 2, max: 2, args: "text, count", desc: "The text repeated a number of times."},
	fnRIGHT:       {name: "RIGHT", group: groupText, min: 1, max: 2, args: "text, [count]", desc: "The last characters of a text."},
	fnROUND:       {name: "ROUND", group: groupMath, min: 1, max: 2, args: "number, [digits]", desc: "The number rounded to a number of decimals."},
	fnROUNDDOWN:   {name: "ROUNDDOWN", group: groupMath, min: 1, max: 2, args: "number, [digits]", desc: "The number rounded toward zero."},
	fnROUNDUP:     {name: "ROUNDUP", group: groupMath, min: 1, max: 2, args: "number, [digits]", desc: "The number rounded away from zero."},
	fnROW:         {name: "ROW", group: groupLookup, min: 0, max: 1, args: "[reference]", desc: "The row number of the reference, or of this cell."},
	fnROWS:        {name: "ROWS", group: groupLookup, min: 1, max: 1, args: "range", desc: "The number of rows in the range."},
	fnSEARCH:      {name: "SEARCH", group: groupText, min: 2, max: 3, args: "search_for, text, [start]", desc: "The position of one text in another, capitals ignored, with wildcards."},
	fnSECOND:      {name: "SECOND", group: groupDate, min: 1, max: 1, args: "time", desc: "The second of a time."},
	fnSIGN:        {name: "SIGN", group: groupMath, min: 1, max: 1, args: "number", desc: "1, 0 or -1 for the sign of the number."},
	fnSMALL:       {name: "SMALL", group: groupStatistics, min: 2, max: 2, args: "range, k", desc: "The k-th smallest number of a range."},
	fnSQRT:        {name: "SQRT", group: groupMath, min: 1, max: 1, args: "number", desc: "The square root of the number."},
	fnSTDEV:       {name: "STDEV", group: groupStatistics, min: 1, max: anyCount, args: "number1, [number2, ...]", desc: "The standard deviation of a sample."},
	fnSTDEVP:      {name: "STDEVP", group: groupStatistics, min: 1, max: anyCount, args: "number1, [number2, ...]", desc: "The standard deviation of a whole population."},
	fnSUBSTITUTE:  {name: "SUBSTITUTE", group: groupText, min: 3, max: 4, args: "text, old_text, new_text, [occurrence]", desc: "The text with one text replaced by another."},
	fnSUM:         {name: "SUM", group: groupMath, min: 1, max: anyCount, args: "number1, [number2, ...]", desc: "The numbers added together."},
	fnSUMIF:       {name: "SUMIF", group: groupConditional, min: 2, max: 3, args: "range, criteria, [sum_range]", desc: "The total of the cells that meet the criteria."},
	fnSUMIFS:      {name: "SUMIFS", group: groupConditional, min: 3, max: anyCount, pairs: true, pairFrom: 1, args: "sum_range, range1, criteria1, [range2, criteria2, ...]", desc: "The total of the cells that meet every criteria."},
	fnSUMPRODUCT:  {name: "SUMPRODUCT", group: groupMath, min: 1, max: anyCount, args: "range1, [range2, ...]", desc: "The total of the products of ranges of the same size, cell by cell."},
	fnSWITCH:      {name: "SWITCH", group: groupLogic, min: 3, max: anyCount, args: "value, case1, result1, [case2, result2, ...], [default]", desc: "The result of the first case that equals the value."},
	fnTEXT:        {name: "TEXT", group: groupText, min: 2, max: 2, args: "number, pattern", desc: "The number as text in a number format pattern."},
	fnTEXTJOIN:    {name: "TEXTJOIN", group: groupText, min: 3, max: anyCount, args: "separator, skip_empty, text1, [text2, ...]", desc: "The texts and ranges joined with a separator."},
	fnTIME:        {name: "TIME", group: groupDate, min: 3, max: 3, args: "hour, minute, second", desc: "The time of an hour, a minute and a second."},
	fnTODAY:       {name: "TODAY", group: groupDate, min: 0, max: 0, args: "", desc: "The calculation date of the sheet."},
	fnTRIM:        {name: "TRIM", group: groupText, min: 1, max: 1, args: "text", desc: "The text without spaces at its ends and with single spaces inside."},
	fnTRUE:        {name: "TRUE", group: groupLogic, min: 0, max: 0, args: "", desc: "The value TRUE."},
	fnTRUNC:       {name: "TRUNC", group: groupMath, min: 1, max: 2, args: "number, [digits]", desc: "The number cut off at a number of decimals."},
	fnUPPER:       {name: "UPPER", group: groupText, min: 1, max: 1, args: "text", desc: "The text in capital letters."},
	fnVALUE:       {name: "VALUE", group: groupText, min: 1, max: 1, args: "text", desc: "The number that a text reads as."},
	fnVAR:         {name: "VAR", group: groupStatistics, min: 1, max: anyCount, args: "number1, [number2, ...]", desc: "The variance of a sample."},
	fnVARP:        {name: "VARP", group: groupStatistics, min: 1, max: anyCount, args: "number1, [number2, ...]", desc: "The variance of a whole population."},
	fnVLOOKUP:     {name: "VLOOKUP", group: groupLookup, min: 3, max: 4, args: "key, range, column_index, [sorted]", desc: "Finds a key in the first column of a range and returns the value beside it."},
	fnWEEKDAY:     {name: "WEEKDAY", group: groupDate, min: 1, max: 2, args: "date, [type]", desc: "The day of the week of a date as a number."},
	fnXLOOKUP:     {name: "XLOOKUP", group: groupLookup, min: 3, max: 4, args: "key, lookup_range, return_range, [if_not_found]", desc: "Finds a key in one range and returns the value at its place in another."},
	fnXOR:         {name: "XOR", group: groupLogic, min: 1, max: anyCount, args: "condition1, [condition2, ...]", desc: "TRUE when an odd number of conditions are true."},
	fnYEAR:        {name: "YEAR", group: groupDate, min: 1, max: 1, args: "date", desc: "The year of a date."},
}

// lookupFunc finds a function by its name without regard to capitals.
func lookupFunc(name string) (fnID, bool) {
	var buf [12]byte
	if len(name) > len(buf) {
		return fnUnknown, false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		buf[i] = c
	}
	key := string(buf[:len(name)])
	lo, hi := 0, int(fnCount)
	for lo < hi {
		mid := (lo + hi) / 2
		if funcTable[mid].name < key {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < int(fnCount) && funcTable[lo].name == key {
		return fnID(lo), true
	}
	return fnUnknown, false
}

// takes reports whether the function takes n arguments.
func (s *funcSpec) takes(n int) bool {
	if n < s.min || (s.max != anyCount && n > s.max) {
		return false
	}
	return !s.pairs || (n-s.pairFrom)%2 == 0
}

// FunctionInfo describes one function of spec rule 12.
type FunctionInfo struct {
	Name        string   // in capitals, such as "SUMIF"
	Group       string   // "Math", "Statistics", "Conditional totals", "Logic", "Text", "Lookup" or "Date and time"
	Args        []string // argument names: "[name]" is optional, and "..." ends a list that repeats
	Description string   // one sentence
}

// Functions returns the 104 functions of rule 12, sorted by name. The
// caller owns the slice.
func Functions() []FunctionInfo {
	out := make([]FunctionInfo, 0, fnCount)
	for i := range funcTable {
		s := &funcTable[i]
		out = append(out, FunctionInfo{Name: s.name, Group: s.group, Args: splitArgs(s.args), Description: s.desc})
	}
	return out
}

// splitArgs splits argument names at the commas outside brackets, so that
// "[value2, ...]" stays one entry.
func splitArgs(args string) []string {
	out := []string{}
	depth, start := 0, 0
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case '[':
			depth++
		case ']':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, args[start:i])
				start = i + 2
			}
		}
	}
	if start < len(args) {
		out = append(out, args[start:])
	}
	return out
}
