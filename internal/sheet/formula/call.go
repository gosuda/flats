package formula

import "math"

// call evaluates a function call. The parser checked the number of
// arguments, so each function finds the arguments it needs.
func (ev *evaluator) call(n *node) Value {
	a := n.args
	switch n.fn {
	// math
	case fnABS:
		return ev.math1(a, math.Abs)
	case fnCEILING:
		return ev.fnCeilFloor(a, true)
	case fnFLOOR:
		return ev.fnCeilFloor(a, false)
	case fnEXP:
		return ev.math1(a, math.Exp)
	case fnINT:
		return ev.math1(a, math.Floor)
	case fnLN:
		return ev.fnLog(a, math.E, false)
	case fnLOG:
		return ev.fnLog(a, 10, true)
	case fnLOG10:
		return ev.fnLog(a, 10, false)
	case fnMOD:
		return ev.fnMod(a)
	case fnPI:
		return NumberValue(math.Pi)
	case fnPOWER:
		return ev.fnPower(a)
	case fnPRODUCT:
		return ev.fnProduct(a)
	case fnQUOTIENT:
		return ev.fnQuotient(a)
	case fnROUND:
		return ev.fnRound(a, roundHalfAway)
	case fnROUNDUP:
		return ev.fnRound(a, roundAway)
	case fnROUNDDOWN, fnTRUNC:
		return ev.fnRound(a, roundToZero)
	case fnSIGN:
		return ev.math1(a, sign)
	case fnSQRT:
		return ev.fnSqrt(a)
	case fnSUM:
		return ev.fnSum(a)
	case fnSUMPRODUCT:
		return ev.fnSumProduct(a)

	// statistics
	case fnAVERAGE:
		return ev.fnAverage(a)
	case fnCOUNT:
		return ev.fnCount(a)
	case fnCOUNTA:
		return ev.fnCountA(a)
	case fnCOUNTBLANK:
		return ev.fnCountBlank(a)
	case fnLARGE:
		return ev.fnKth(a, true)
	case fnSMALL:
		return ev.fnKth(a, false)
	case fnMAX:
		return ev.fnMaxMin(a, true)
	case fnMIN:
		return ev.fnMaxMin(a, false)
	case fnMEDIAN:
		return ev.fnMedian(a)
	case fnRANK:
		return ev.fnRank(a)
	case fnSTDEV:
		return ev.fnSpread(a, true, true)
	case fnSTDEVP:
		return ev.fnSpread(a, false, true)
	case fnVAR:
		return ev.fnSpread(a, true, false)
	case fnVARP:
		return ev.fnSpread(a, false, false)

	// conditional totals
	case fnCOUNTIF, fnCOUNTIFS:
		return ev.fnCond(condCount, nil, false, a)
	case fnSUMIF:
		return ev.fnCond(condSum, condValueArg(a), false, a[:2])
	case fnSUMIFS:
		return ev.fnCond(condSum, a[0], true, a[1:])
	case fnAVERAGEIF:
		return ev.fnCond(condAverage, condValueArg(a), false, a[:2])
	case fnAVERAGEIFS:
		return ev.fnCond(condAverage, a[0], true, a[1:])
	case fnMAXIFS:
		return ev.fnCond(condMax, a[0], true, a[1:])
	case fnMINIFS:
		return ev.fnCond(condMin, a[0], true, a[1:])

	// logic
	case fnAND:
		return ev.fnAndOr(a, false)
	case fnOR:
		return ev.fnAndOr(a, true)
	case fnXOR:
		return ev.fnXor(a)
	case fnNOT:
		b, err := ev.truth(a[0])
		if err != ErrNone {
			return errValue(err)
		}
		return BoolValue(!b)
	case fnTRUE:
		return BoolValue(true)
	case fnFALSE:
		return BoolValue(false)
	case fnNA:
		return errValue(ErrNA)
	case fnIF:
		return ev.fnIf(a)
	case fnIFERROR:
		return ev.fnIfError(a, false)
	case fnIFNA:
		return ev.fnIfError(a, true)
	case fnIFS:
		return ev.fnIfs(a)
	case fnSWITCH:
		return ev.fnSwitch(a)
	case fnISBLANK:
		return ev.fnIsBlank(a)
	case fnISERROR:
		return BoolValue(ev.eval(a[0]).kind == KindError)
	case fnISNA:
		return BoolValue(ev.eval(a[0]).Err() == ErrNA)
	case fnISLOGICAL:
		return BoolValue(ev.eval(a[0]).kind == KindBool)
	case fnISNUMBER:
		return BoolValue(ev.eval(a[0]).kind == KindNumber)
	case fnISTEXT:
		return BoolValue(ev.eval(a[0]).kind == KindText)

	// text
	case fnCONCAT:
		return ev.fnJoin(a, "", false, true, false)
	case fnCONCATENATE:
		return ev.fnJoin(a, "", false, false, false)
	case fnTEXTJOIN:
		return ev.fnTextJoin(a)
	case fnEXACT:
		return ev.fnExact(a)
	case fnFIND:
		return ev.fnFind(a, false)
	case fnSEARCH:
		return ev.fnFind(a, true)
	case fnLEFT:
		return ev.fnLeftRight(a, false)
	case fnRIGHT:
		return ev.fnLeftRight(a, true)
	case fnMID:
		return ev.fnMid(a)
	case fnLEN:
		return ev.fnLen(a)
	case fnLOWER:
		return ev.fnCase(a, caseLower)
	case fnUPPER:
		return ev.fnCase(a, caseUpper)
	case fnPROPER:
		return ev.fnCase(a, caseProper)
	case fnREPLACE:
		return ev.fnReplace(a)
	case fnREPT:
		return ev.fnRept(a)
	case fnSUBSTITUTE:
		return ev.fnSubstitute(a)
	case fnTRIM:
		return ev.fnTrim(a)
	case fnTEXT:
		return ev.fnText(a)
	case fnVALUE:
		return ev.fnValue(a)

	// lookup
	case fnCHOOSE:
		return ev.fnChoose(a)
	case fnROW:
		return ev.fnRowColumn(a, true)
	case fnCOLUMN:
		return ev.fnRowColumn(a, false)
	case fnROWS:
		return ev.fnRowsColumns(a, true)
	case fnCOLUMNS:
		return ev.fnRowsColumns(a, false)
	case fnINDEX:
		return ev.fnIndex(a)
	case fnMATCH:
		return ev.fnMatch(a)
	case fnVLOOKUP:
		return ev.fnTableLookup(a, true)
	case fnHLOOKUP:
		return ev.fnTableLookup(a, false)
	case fnXLOOKUP:
		return ev.fnXLookup(a)

	// date and time
	case fnDATE:
		return ev.fnDate(a)
	case fnDATEDIF:
		return ev.fnDateDif(a)
	case fnDAY:
		return ev.fnDatePart(a, 2)
	case fnMONTH:
		return ev.fnDatePart(a, 1)
	case fnYEAR:
		return ev.fnDatePart(a, 0)
	case fnDAYS:
		return ev.fnDays(a)
	case fnEDATE:
		return ev.fnShiftMonths(a, false)
	case fnEOMONTH:
		return ev.fnShiftMonths(a, true)
	case fnHOUR:
		return ev.fnTimePart(a, 0)
	case fnMINUTE:
		return ev.fnTimePart(a, 1)
	case fnSECOND:
		return ev.fnTimePart(a, 2)
	case fnTIME:
		return ev.fnTime(a)
	case fnTODAY:
		return NumberValue(float64(ev.env.Today))
	case fnWEEKDAY:
		return ev.fnWeekday(a)
	}
	return errValue(ErrName)
}
