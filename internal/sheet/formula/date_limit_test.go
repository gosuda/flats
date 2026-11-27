package formula

import "testing"

// The last date is 9999-12-31, day 2,958,465 (rule 12, T-02). A date
// function whose result comes after it, or that reads a date or time of a
// later day or before day 0, gives #NUM!. A fraction of the last day is
// still the last day. Arithmetic on a date is no date function.
func TestDateLimitIsNumError(t *testing.T) {
	sheet := &testSheet{tabs: []*testTab{newTab(t, "Sheet1", nil)}}
	num := ErrorValue(ErrNum)
	for formula, want := range map[string]Value{
		`=DATE(9999,12,31)`:             NumberValue(lastDateDay),
		`=DATE(9999,12,31)+1`:           NumberValue(lastDateDay + 1),
		`=DATE(10000,1,1)`:              num,
		`=DATE(9999,12,32)`:             num,
		`=DATE(9999,13,1)`:              num,
		`=EDATE(DATE(9999,10,31),2)`:    NumberValue(lastDateDay),
		`=EDATE(DATE(9999,12,31),1)`:    num,
		`=EOMONTH(DATE(9999,12,1),0)`:   NumberValue(lastDateDay),
		`=EOMONTH(DATE(9999,12,1),1)`:   num,
		`=YEAR(2958465.5)`:              NumberValue(9999),
		`=MONTH(2958465.5)`:             NumberValue(12),
		`=DAY(2958465.5)`:               NumberValue(31),
		`=HOUR(2958465.5)`:              NumberValue(12),
		`=WEEKDAY(2958465)`:             NumberValue(6),
		`=YEAR(2958466)`:                num,
		`=MONTH(2958466)`:               num,
		`=DAY(2958466)`:                 num,
		`=WEEKDAY(2958466)`:             num,
		`=HOUR(2958466)`:                num,
		`=MINUTE(2958466)`:              num,
		`=SECOND(2958466)`:              num,
		`=DATEDIF(1,2958466,"d")`:       num,
		`=DATEDIF(1,2958465.5,"d")`:     NumberValue(lastDateDay - 1),
		`=DAYS(2958466,1)`:              num,
		`=DAYS(1,2958466)`:              num,
		`=DAYS(2958465.5,0)`:            NumberValue(lastDateDay),
		`=YEAR(-1)`:                     num,
		`=HOUR(-0.5)`:                   num,
		`=DAYS(1,-1)`:                   num,
		`=DAYS(-1,1)`:                   num,
		`=DAYS(2,1)`:                    NumberValue(1),
		`=EDATE(DATE(1900,1,1),-1)`:     num,
		`=EDATE(2958466,-1)`:            num,
		`=EOMONTH(2958466,-1)`:          num,
		`=DATEDIF(2958466,2958467,"d")`: num,
	} {
		if got := evalOn(t, sheet, formula); got != want {
			t.Errorf("%s = %v, want %v", formula, got, want)
		}
	}
}
