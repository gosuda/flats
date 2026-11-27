package formula

// truths calls fn for each truth value of the arguments of AND, OR and
// XOR: numbers and booleans in a range, where text and blank cells are
// passed over, a referenced cell in the same way, and each direct value as
// a boolean. It stops when fn returns false. Arguments that hold no truth
// value at all are #VALUE!, as Excel documents.
func (ev *evaluator) truths(a []*node, fn func(b bool) bool) ErrorCode {
	found := false
	for _, arg := range a {
		if arg.kind != nodeRef {
			b, err := ev.truth(arg)
			if err != ErrNone {
				return err
			}
			found = true
			if !fn(b) {
				return ErrNone
			}
			continue
		}
		rg, err := ev.rangeOf(arg)
		if err != ErrNone {
			return err
		}
		stopped := false
		done := ev.scan(&rg, func(_, _ int32, v Value) bool {
			switch v.kind {
			case KindError:
				err = v.Err()
				return false
			case KindNumber, KindBool:
				found = true
				if !fn(v.num != 0) {
					stopped = true
					return false
				}
			}
			return true
		})
		switch {
		case err != ErrNone:
			return err
		case stopped:
			return ErrNone
		case !done:
			return ErrLimit
		}
	}
	if !found {
		return ErrValue
	}
	return ErrNone
}

// fnAndOr is AND and OR. Both stop at the first value that decides the
// result, so a later error is not evaluated.
func (ev *evaluator) fnAndOr(a []*node, or bool) Value {
	result := !or
	err := ev.truths(a, func(b bool) bool {
		if b == or {
			result = or
			return false
		}
		return true
	})
	if err != ErrNone {
		return errValue(err)
	}
	return BoolValue(result)
}

func (ev *evaluator) fnXor(a []*node) Value {
	odd := false
	err := ev.truths(a, func(b bool) bool {
		odd = odd != b
		return true
	})
	if err != ErrNone {
		return errValue(err)
	}
	return BoolValue(odd)
}

func (ev *evaluator) fnIf(a []*node) Value {
	b, err := ev.truth(a[0])
	switch {
	case err != ErrNone:
		return errValue(err)
	case b:
		return ev.eval(a[1])
	case len(a) > 2:
		return ev.eval(a[2])
	}
	return BoolValue(false)
}

// fnIfError is IFERROR, and IFNA when onlyNA is set. Without a second
// argument the replacement is the blank value.
func (ev *evaluator) fnIfError(a []*node, onlyNA bool) Value {
	v := ev.eval(a[0])
	if v.kind != KindError || (onlyNA && v.Err() != ErrNA) {
		return v
	}
	if len(a) > 1 {
		return ev.eval(a[1])
	}
	return Value{}
}

func (ev *evaluator) fnIfs(a []*node) Value {
	for i := 0; i+1 < len(a); i += 2 {
		b, err := ev.truth(a[i])
		if err != ErrNone {
			return errValue(err)
		}
		if b {
			return ev.eval(a[i+1])
		}
	}
	return errValue(ErrNA)
}

// fnSwitch compares as the = operator does.
func (ev *evaluator) fnSwitch(a []*node) Value {
	key := ev.eval(a[0])
	if key.kind == KindError {
		return key
	}
	rest := a[1:]
	for ; len(rest) >= 2; rest = rest[2:] {
		c := ev.eval(rest[0])
		if c.kind == KindError {
			return c
		}
		if ev.compare(key, c) == 0 {
			return ev.eval(rest[1])
		}
	}
	if len(rest) == 1 {
		return ev.eval(rest[0])
	}
	return errValue(ErrNA)
}

// fnIsBlank tells a blank cell from a formula that returns blank: a
// referenced cell is blank when it is not filled.
func (ev *evaluator) fnIsBlank(a []*node) Value {
	if a[0].kind == nodeRef && ev.f.slots[a[0].slot].kind == slotCell {
		rg, err := ev.rangeOf(a[0])
		if err != ErrNone {
			return errValue(err)
		}
		return BoolValue(!ev.filled(&rg, 0, 0))
	}
	return BoolValue(ev.eval(a[0]).kind == KindBlank)
}
