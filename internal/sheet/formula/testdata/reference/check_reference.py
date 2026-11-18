#!/usr/bin/env python3
# check_reference.py: independent cross-check of the sheet function reference
# table (the *.jsonl files next to this script; technical design section 9.6).
#
# It is test tooling, not the formula engine. It reads every row, recomputes
# the expected value, shown text and CSV field with its own small evaluator of
# the behavior that spec rules 4 to 6, 9 to 12 and 60 and design section 9
# describe, and compares as the contract does: text, booleans and errors
# exactly, numbers after rounding to 15 significant digits. It also checks
# the file format, the ids and the coverage of the 104 functions of rule 12.
# typed.jsonl holds typed entry cases (rule 5) with their own fields.
#
# Run:  python3 check_reference.py        (from any directory)
# Exit: 0 only when every row was recomputed and agrees. 1 on any mismatch,
#       on any row that could not be recomputed, and on any format problem.
# Needs only the Python standard library and no network.

import glob
import json
import math
import os
import re
import statistics
import sys
import unicodedata
from decimal import Decimal, ROUND_HALF_UP, ROUND_FLOOR, getcontext

getcontext().prec = 700

HERE = os.path.dirname(os.path.abspath(__file__))
MAX_TEXT = 32767
MAX_ROWS, MAX_COLS = 100000, 702
DEFAULT_AT = "Z1"                         # where the formula of a row is written
TAB_NAME = "Sheet1"                       # the tab that holds cells and the formula

GROUPS = {
    "math": "ABS CEILING EXP FLOOR INT LN LOG LOG10 MOD PI POWER PRODUCT QUOTIENT ROUND "
            "ROUNDDOWN ROUNDUP SIGN SQRT SUM SUMPRODUCT TRUNC",
    "statistics": "AVERAGE COUNT COUNTA COUNTBLANK LARGE MAX MEDIAN MIN RANK SMALL STDEV "
                  "STDEVP VAR VARP",
    "conditional": "AVERAGEIF AVERAGEIFS COUNTIF COUNTIFS MAXIFS MINIFS SUMIF SUMIFS",
    "logic": "AND FALSE IF IFERROR IFNA IFS ISBLANK ISERROR ISLOGICAL ISNA ISNUMBER ISTEXT "
             "NA NOT OR SWITCH TRUE XOR",
    "text": "CONCAT CONCATENATE EXACT FIND LEFT LEN LOWER MID PROPER REPLACE REPT RIGHT "
            "SEARCH SUBSTITUTE TEXT TEXTJOIN TRIM UPPER VALUE",
    "lookup": "CHOOSE COLUMN COLUMNS HLOOKUP INDEX MATCH ROW ROWS VLOOKUP XLOOKUP",
    "date": "DATE DATEDIF DAY DAYS EDATE EOMONTH HOUR MINUTE MONTH SECOND TIME TODAY "
            "WEEKDAY YEAR",
}
GROUPS = {k: v.split() for k, v in GROUPS.items()}
FN_FILE = {fn: f for f, fns in GROUPS.items() for fn in fns}
OTHER_FN = {"operators": {"operators"}, "typed": {"typed"}}
KEYS = ["id", "fn", "class", "date", "cells", "tabs", "formula", "at", "format", "value",
        "shown", "csv", "note", "differs"]
OPTIONAL_KEYS = {"tabs", "at", "differs"}
KEYS_TYPED = ["id", "fn", "class", "typed", "sets", "value", "shown", "csv", "note"]
TYPED_DATE = "2026-11-13"                 # typed rows carry no date; none reads TODAY
CLASSES = ("normal", "boundary", "error", "coercion")
DIFFERS = ("excel", "sheets", "both")
MIN_ROWS_PER_FUNCTION = 8
BANNED = {0x2014: "em dash", 0x2018: "curly quote", 0x2019: "curly quote",
          0x201C: "curly quote", 0x201D: "curly quote"}
CURRENCY_SIGNS = "$" + chr(0x20AC) + chr(0xA3) + chr(0xA5) + chr(0x20A9)

# minimum and maximum argument counts (None: no maximum)
ARITY = {
    "ABS": (1, 1), "CEILING": (1, 2), "EXP": (1, 1), "FLOOR": (1, 2), "INT": (1, 1),
    "LN": (1, 1), "LOG": (1, 2), "LOG10": (1, 1), "MOD": (2, 2), "PI": (0, 0),
    "POWER": (2, 2), "PRODUCT": (1, None), "QUOTIENT": (2, 2), "ROUND": (1, 2),
    "ROUNDDOWN": (1, 2), "ROUNDUP": (1, 2), "SIGN": (1, 1), "SQRT": (1, 1),
    "SUM": (1, None), "SUMPRODUCT": (1, None), "TRUNC": (1, 2),
    "AVERAGE": (1, None), "COUNT": (1, None), "COUNTA": (1, None), "COUNTBLANK": (1, 1),
    "LARGE": (2, 2), "MAX": (1, None), "MEDIAN": (1, None), "MIN": (1, None),
    "RANK": (2, 3), "SMALL": (2, 2), "STDEV": (1, None), "STDEVP": (1, None),
    "VAR": (1, None), "VARP": (1, None),
    "AVERAGEIF": (2, 3), "AVERAGEIFS": (3, None), "COUNTIF": (2, 2), "COUNTIFS": (2, None),
    "MAXIFS": (3, None), "MINIFS": (3, None), "SUMIF": (2, 3), "SUMIFS": (3, None),
    "AND": (1, None), "FALSE": (0, 0), "IF": (2, 3), "IFERROR": (1, 2), "IFNA": (1, 2),
    "IFS": (2, None), "ISBLANK": (1, 1), "ISERROR": (1, 1), "ISLOGICAL": (1, 1),
    "ISNA": (1, 1), "ISNUMBER": (1, 1), "ISTEXT": (1, 1), "NA": (0, 0), "NOT": (1, 1),
    "OR": (1, None), "SWITCH": (3, None), "TRUE": (0, 0), "XOR": (1, None),
    "CONCAT": (1, None), "CONCATENATE": (1, None), "EXACT": (2, 2), "FIND": (2, 3),
    "LEFT": (1, 2), "LEN": (1, 1), "LOWER": (1, 1), "MID": (3, 3), "PROPER": (1, 1),
    "REPLACE": (4, 4), "REPT": (2, 2), "RIGHT": (1, 2), "SEARCH": (2, 3),
    "SUBSTITUTE": (3, 4), "TEXT": (2, 2), "TEXTJOIN": (3, None), "TRIM": (1, 1),
    "UPPER": (1, 1), "VALUE": (1, 1),
    "CHOOSE": (2, None), "COLUMN": (0, 1), "COLUMNS": (1, 1), "HLOOKUP": (3, 4),
    "INDEX": (2, 3), "MATCH": (2, 3), "ROW": (0, 1), "ROWS": (1, 1), "VLOOKUP": (3, 4),
    "XLOOKUP": (3, 4),
    "DATE": (3, 3), "DATEDIF": (3, 3), "DAY": (1, 1), "DAYS": (2, 2), "EDATE": (2, 2),
    "EOMONTH": (2, 2), "HOUR": (1, 1), "MINUTE": (1, 1), "MONTH": (1, 1), "SECOND": (1, 1),
    "TIME": (3, 3), "TODAY": (0, 0), "WEEKDAY": (1, 2), "YEAR": (1, 1),
}
DATE_RESULT = {"DATE", "TODAY", "EDATE", "EOMONTH"}
MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"]


# ---------------------------------------------------------------- values

class Blank:
    def __repr__(self):
        return "BLANK"


BLANK = Blank()


class XErr(Exception):
    """A formula error value travelling up the evaluation."""
    def __init__(self, code):
        Exception.__init__(self, code)
        self.code = code


class Unsupported(Exception):
    """The checker cannot recompute this row; the reason is reported."""


class SyntaxProblem(Exception):
    pass


def is_num(v):
    return isinstance(v, float)


# ---------------------------------------------------------------- numbers

def dec15(x):
    """The number rounded to 15 significant digits, half away from zero."""
    d = Decimal(x)
    if d == 0:
        return Decimal(0)
    q = Decimal(1).scaleb(d.adjusted() - 14)
    return d.quantize(q, rounding=ROUND_HALF_UP)


def round_half_away(d, digits):
    return d.quantize(Decimal(1).scaleb(-digits), rounding=ROUND_HALF_UP)


def plain(d):
    s = format(d, "f")
    if "." in s:
        s = s.rstrip("0").rstrip(".")
    return "0" if s in ("-0", "") else s


def general(x):
    """General text: up to 15 significant digits, scientific for more than 15
    digits before the decimal point or more than 9 zeros after it."""
    d = dec15(x)
    if d == 0:
        return "0"
    e = d.adjusted()
    if e >= 15 or e <= -11:
        m = d.scaleb(-e).quantize(Decimal("0.00001"), rounding=ROUND_HALF_UP)
        if abs(m) >= 10:
            m = m / 10
            e += 1
        return "%sE%s%02d" % (plain(m), "+" if e >= 0 else "-", abs(e))
    return plain(d)


def group3(digits):
    out = []
    while len(digits) > 3:
        out.insert(0, digits[-3:])
        digits = digits[:-3]
    out.insert(0, digits)
    return ",".join(out)


def fixed(d, decimals, thousands):
    q = round_half_away(d, decimals)
    neg = q < 0
    s = format(abs(q), "f")
    whole, _, frac = s.partition(".")
    if thousands:
        whole = group3(whole)
    body = whole + ("." + frac if decimals > 0 else "")
    return neg, body


# ---------------------------------------------------------------- dates
# Day 0 is 1899-12-30 and day 2 is 1900-01-01 (rule 4). The proleptic
# Gregorian calendar is used throughout, so there is no 1900-02-29.

def days_from_civil(y, m, d):
    y -= m <= 2
    era = (y if y >= 0 else y - 399) // 400
    yoe = y - era * 400
    doy = (153 * (m + (-3 if m > 2 else 9)) + 2) // 5 + d - 1
    doe = yoe * 365 + yoe // 4 - yoe // 100 + doy
    return era * 146097 + doe - 719468


EPOCH = days_from_civil(1899, 12, 30)


def serial_of(y, m, d):
    return days_from_civil(y, m, d) - EPOCH


def civil_of(serial):
    z = serial + EPOCH + 719468
    era = (z if z >= 0 else z - 146096) // 146097
    doe = z - era * 146097
    yoe = (doe - doe // 1460 + doe // 36524 - doe // 146096) // 365
    y = yoe + era * 400
    doy = doe - (365 * yoe + yoe // 4 - yoe // 100)
    mp = (5 * doy + 2) // 153
    d = doy - (153 * mp + 2) // 5 + 1
    m = mp + (3 if mp < 10 else -9)
    return (y + (m <= 2), m, d)


def month_len(y, m):
    if m == 2:
        return 29 if (y % 4 == 0 and (y % 100 != 0 or y % 400 == 0)) else 28
    return 30 if m in (4, 6, 9, 11) else 31


def valid_date(y, m, d):
    return 1900 <= y <= 9999 and 1 <= m <= 12 and 1 <= d <= month_len(y, m)


def split_day_time(x):
    """Whole days and seconds of the day of a number that is not negative."""
    d = dec15(x)
    days = int(d.to_integral_value(rounding=ROUND_FLOOR))
    secs = int(((d - days) * 86400).quantize(Decimal(1), rounding=ROUND_HALF_UP))
    if secs >= 86400:
        days += 1
        secs = 0
    return days, secs


def date_text(serial, pattern):
    y, m, d = civil_of(serial)
    if pattern == "yyyy-mm-dd":
        return "%04d-%02d-%02d" % (y, m, d)
    if pattern == "m/d/yyyy":
        return "%d/%d/%04d" % (m, d, y)
    if pattern == "mmm d, yyyy":
        return "%s %d, %04d" % (MONTHS[m - 1], d, y)
    raise Unsupported("date pattern " + pattern)


def time_text(secs, pattern):
    h, mi, s = secs // 3600, secs // 60 % 60, secs % 60
    if pattern == "hh:mm":
        return "%02d:%02d" % (h, mi)
    if pattern == "hh:mm:ss":
        return "%02d:%02d:%02d" % (h, mi, s)
    if pattern == "h:mm AM/PM":
        return "%d:%02d %s" % ((h % 12) or 12, mi, "AM" if h < 12 else "PM")
    raise Unsupported("time pattern " + pattern)


DATE_PATTERNS = ("yyyy-mm-dd", "m/d/yyyy", "mmm d, yyyy")
TIME_PATTERNS = ("hh:mm", "hh:mm:ss", "h:mm AM/PM")


# ---------------------------------------------------------------- formats (rule 6)

def show_number(x, nf):
    kind = nf["kind"] if nf else "general"
    if kind in ("general", "text"):
        return general(x)
    d = dec15(x)
    if kind == "number":
        neg, body = fixed(d, nf["decimals"], nf["thousands"])
        return ("-" if neg else "") + body
    if kind == "percent":
        neg, body = fixed(d * 100, nf["decimals"], False)
        return ("-" if neg else "") + body + "%"
    if kind == "currency":
        neg, body = fixed(d, nf["decimals"], nf.get("thousands", True))
        return ("-" if neg else "") + nf["symbol"] + body
    if x < 0:
        return general(x)        # a date or time format on a negative number
    days, secs = split_day_time(x)
    if kind == "date":
        return date_text(days, nf["pattern"])
    if kind == "time":
        return time_text(secs, nf["pattern"])
    if kind == "datetime":
        return date_text(days, nf["date"]) + " " + time_text(secs, nf["time"])
    raise Unsupported("format kind " + kind)


def show(value, nf):
    if isinstance(value, XErr):
        return value.code
    if isinstance(value, bool):
        return "TRUE" if value else "FALSE"
    if isinstance(value, str):
        return value
    return show_number(value, nf)


# ---------------------------------------------------------------- typed entry (rule 5)

NUMBER_RE = re.compile(
    r"^(-)?([%s])?(\d{1,3}(?:,\d{3})+|\d+)(\.\d+)?(?:([Ee][+-]?\d+)|(%%))?$" % re.escape(CURRENCY_SIGNS))
ISO_RE = re.compile(r"^(\d{4})-(\d{2})-(\d{2})$")
MDY_RE = re.compile(r"^(\d{1,2})/(\d{1,2})/(\d{4})$")
MON_RE = re.compile(r"^([A-Za-z]{3}) (\d{1,2}), (\d{4})$")
T24_RE = re.compile(r"^(\d{1,2}):(\d{2})(?::(\d{2}))?$")
T12_RE = re.compile(r"^(\d{1,2}):(\d{2})(?::(\d{2}))? ([AaPp][Mm])$")


def read_number(text):
    """(value, format) for the number forms of rule 5, else None."""
    m = NUMBER_RE.match(text)
    if not m:
        return None
    sign, cur, whole, frac, exp, pct = m.groups()
    d = Decimal(whole.replace(",", "") + (frac or ""))
    if exp:
        d = d * (Decimal(10) ** int(exp[1:]))
    if pct:
        d = d / 100
    if sign:
        d = -d
    nf = None
    if pct:
        nf = {"kind": "percent", "decimals": 2 if frac else 0}
    elif cur:
        nf = {"kind": "currency", "symbol": cur, "decimals": 2}
    return float(d), nf


def read_date(text):
    m = ISO_RE.match(text)
    if m:
        y, mo, d = int(m.group(1)), int(m.group(2)), int(m.group(3))
        pat = "yyyy-mm-dd"
    else:
        m = MDY_RE.match(text)
        if m:
            mo, d, y = int(m.group(1)), int(m.group(2)), int(m.group(3))
            pat = "m/d/yyyy"
        else:
            m = MON_RE.match(text)
            if not m:
                return None
            name = m.group(1).capitalize()
            if name not in MONTHS:
                return None
            mo, d, y = MONTHS.index(name) + 1, int(m.group(2)), int(m.group(3))
            pat = "mmm d, yyyy"
    if not valid_date(y, mo, d):
        return None
    return serial_of(y, mo, d), pat


def read_time(text):
    m = T12_RE.match(text)
    if m:
        h, mi, s = int(m.group(1)), int(m.group(2)), int(m.group(3) or 0)
        if not (1 <= h <= 12 and mi <= 59 and s <= 59) or m.group(3):
            return None
        h = h % 12 + (12 if m.group(4).upper() == "PM" else 0)
        return h * 3600 + mi * 60, "h:mm AM/PM"
    m = T24_RE.match(text)
    if m:
        h, mi, s = int(m.group(1)), int(m.group(2)), int(m.group(3) or 0)
        if h > 23 or mi > 59 or s > 59:
            return None
        return h * 3600 + mi * 60 + s, "hh:mm:ss" if m.group(3) else "hh:mm"
    return None


def read_typed(text):
    """(kind, value, format) for text read as rule 5 reads a typed entry.
    kind is one of blank, formula, text, bool, number, date, time, datetime."""
    if text == "":
        return "blank", BLANK, None
    if text.startswith("'"):
        return "text", text[1:], None
    if text.startswith("=") and len(text) > 1:
        return "formula", text, None
    if text.upper() in ("TRUE", "FALSE"):
        return "bool", text.upper() == "TRUE", None
    n = read_number(text)
    if n:
        return "number", n[0], n[1]
    d = read_date(text)
    if d:
        return "date", float(d[0]), {"kind": "date", "pattern": d[1]}
    t = read_time(text)
    if t:
        return "time", t[0] / 86400.0, {"kind": "time", "pattern": t[1]}
    for i, ch in enumerate(text):
        if ch == " ":
            d, t = read_date(text[:i]), read_time(text[i + 1:])
            if d and t:
                return ("datetime", d[0] + t[0] / 86400.0,
                        {"kind": "datetime", "date": d[1], "time": t[1]})
    return "text", text, None


def text_to_number(text):
    kind, value, _ = read_typed(text) if not text.startswith(("'", "=")) else ("text", text, None)
    if kind in ("number", "date", "time", "datetime"):
        return value
    return None


# ---------------------------------------------------------------- CSV (rule 60)

CSV_NUMBER_RE = re.compile(
    r"^-[%s]?(\d{1,3}(,\d{3})+|\d+)(\.\d+)?([Ee][+-]?\d+|%%)?$" % re.escape(CURRENCY_SIGNS))


def csv_field(shown):
    t = shown.lstrip(" ")
    apostrophe = t[:1] in ("=", "+", "@", "\t", "\r", "\n") and t != ""
    if t.startswith("-") and not CSV_NUMBER_RE.match(shown):
        apostrophe = True
    out = ("'" if apostrophe else "") + shown
    if any(c in out for c in ',;\t"\r\n'):
        out = '"' + out.replace('"', '""') + '"'
    return out


# ---------------------------------------------------------------- parser (section 9.2)

REF_RE = re.compile(r"""
    (?:(?P<tab>'(?:[^']|'')+'|[A-Za-z_][A-Za-z0-9_.]*)!)?
    (?:
       \$?(?P<c1>[A-Za-z]{1,3})\$?(?P<r1>\d+)(?::\$?(?P<c2>[A-Za-z]{1,3})\$?(?P<r2>\d+))?
     | \$?(?P<wc1>[A-Za-z]{1,3}):\$?(?P<wc2>[A-Za-z]{1,3})
     | \$?(?P<wr1>\d+):\$?(?P<wr2>\d+)
    )(?![A-Za-z0-9_(.!])
""", re.X)
NUM_TOKEN_RE = re.compile(r"(\d+(\.\d*)?|\.\d+)([Ee][+-]?\d+)?")
NAME_RE = re.compile(r"[A-Za-z_][A-Za-z0-9_.]*")
ERROR_LITERALS = ["#DIV/0!", "#VALUE!", "#NUM!", "#N/A", "#NAME?", "#REF!", "#ERROR!",
                  "#CYCLE!", "#LIMIT!"]


def col_number(letters):
    n = 0
    for ch in letters.upper():
        n = n * 26 + ord(ch) - 64
    return n


class Parser:
    def __init__(self, text):
        self.s = text
        self.i = 1                       # after the leading "="

    def ws(self):
        while self.i < len(self.s) and self.s[self.i] in " \t\r\n":
            self.i += 1

    def peek(self, *ops):
        self.ws()
        for op in ops:
            if self.s.startswith(op, self.i):
                return op
        return None

    def take(self, *ops):
        op = self.peek(*ops)
        if op:
            self.i += len(op)
        return op

    def parse(self):
        node = self.expr()
        self.ws()
        if self.i != len(self.s):
            raise SyntaxProblem("text after the formula")
        return node

    def expr(self):
        node = self.concat()
        while True:
            op = self.take("<>", "<=", ">=", "=", "<", ">")
            if not op:
                return node
            node = ("cmp", op, node, self.concat())

    def concat(self):
        node = self.sum()
        while self.take("&"):
            node = ("cat", node, self.sum())
        return node

    def sum(self):
        node = self.product()
        while True:
            op = self.take("+", "-")
            if not op:
                return node
            node = ("bin", op, node, self.product())

    def product(self):
        node = self.power()
        while True:
            op = self.take("*", "/")
            if not op:
                return node
            node = ("bin", op, node, self.power())

    def power(self):
        node = self.percent()
        while self.take("^"):
            node = ("bin", "^", node, self.percent())
        return node

    def percent(self):
        node = self.unary()
        while self.take("%"):
            node = ("pct", node)
        return node

    def unary(self):
        op = self.take("+", "-")
        if op:
            return ("un", op, self.unary())
        return self.primary()

    def primary(self):
        self.ws()
        s, i = self.s, self.i
        if i >= len(s):
            raise SyntaxProblem("something is missing")
        m = REF_RE.match(s, i)
        if m:
            self.i = m.end()
            return self.ref(m)
        m = NUM_TOKEN_RE.match(s, i)
        if m:
            self.i = m.end()
            return ("num", float(m.group(0)))
        if s[i] == '"':
            j, out = i + 1, []
            while True:
                if j >= len(s):
                    raise SyntaxProblem("text is not closed")
                if s[j] == '"':
                    if s.startswith('""', j):
                        out.append('"')
                        j += 2
                        continue
                    break
                out.append(s[j])
                j += 1
            self.i = j + 1
            return ("str", "".join(out))
        if s[i] == "#":
            for lit in ERROR_LITERALS:
                if s.startswith(lit, i):
                    self.i = i + len(lit)
                    return ("err", lit)
            raise SyntaxProblem("unknown error name")
        if s[i] == "(":
            self.i = i + 1
            node = self.expr()
            if not self.take(")"):
                raise SyntaxProblem("a bracket is missing")
            return node
        m = NAME_RE.match(s, i)
        if m:
            name = m.group(0).upper()
            self.i = m.end()
            if self.take("("):
                args = []
                if not self.take(")"):
                    while True:
                        if self.peek(",", ")"):
                            args.append(("empty",))
                        else:
                            args.append(self.expr())
                        if self.take(")"):
                            break
                        if not self.take(","):
                            raise SyntaxProblem("a bracket is missing")
                return ("call", name, args)
            if name in ("TRUE", "FALSE"):
                return ("bool", name == "TRUE")
            return ("name", name)
        raise SyntaxProblem("unexpected character")

    def ref(self, m):
        tab = m.group("tab")
        if tab and tab.startswith("'"):
            tab = tab[1:-1].replace("''", "'")
        if m.group("c1"):
            c1, r1 = col_number(m.group("c1")), int(m.group("r1"))
            single = m.group("c2") is None
            c2 = c1 if single else col_number(m.group("c2"))
            r2 = r1 if single else int(m.group("r2"))
        elif m.group("wc1"):
            c1, c2, r1, r2, single = col_number(m.group("wc1")), col_number(m.group("wc2")), 1, None, False
        else:
            r1, r2, c1, c2, single = int(m.group("wr1")), int(m.group("wr2")), 1, None, False
        return ("ref", tab, r1, c1, r2, c2, single)


# ---------------------------------------------------------------- sheet model

def parse_address(a1):
    m = re.match(r"^([A-Z]{1,3})(\d+)$", a1)
    if not m:
        raise Unsupported("cell address " + a1)
    return int(m.group(2)), col_number(m.group(1))


class Cell:
    __slots__ = ("kind", "value", "ast", "error")


def cell_from_json(v):
    """Content by the JSON typing of write cells (rule 43)."""
    c = Cell()
    c.ast = None
    c.error = None
    if isinstance(v, bool):
        c.kind, c.value = "bool", v
    elif isinstance(v, (int, float)):
        c.kind, c.value = "number", float(v)
    elif isinstance(v, str):
        if v.startswith("=") and len(v) > 1:
            c.kind, c.value = "formula", v
        elif v.startswith("'"):
            c.kind, c.value = "text", v[1:]
        else:
            c.kind, c.value = "text", v
    elif isinstance(v, dict) and "formula" in v:
        # a formula object as a read returns it: only the formula text is written
        text = v["formula"]
        if set(v) - {"formula", "value", "error"} or not (isinstance(text, str) and text.startswith("=")
                                                         and len(text) > 1):
            raise Unsupported("formula object %r" % (v,))
        c.kind, c.value = "formula", text
    elif isinstance(v, dict) and len(v) == 1:
        (k, s), = v.items()
        if k == "date":
            y, mo, d = (int(p) for p in s.split("-"))
            c.kind, c.value = "date", float(serial_of(y, mo, d))
        elif k == "time":
            h, mi, sec = (int(p) for p in s.split(":"))
            c.kind, c.value = "time", (h * 3600 + mi * 60 + sec) / 86400.0
        elif k == "datetime":
            ds, ts = s.split("T")
            y, mo, d = (int(p) for p in ds.split("-"))
            h, mi, sec = (int(p) for p in ts.split(":"))
            c.kind, c.value = "datetime", serial_of(y, mo, d) + (h * 3600 + mi * 60 + sec) / 86400.0
        else:
            raise Unsupported("cell object " + k)
    else:
        raise Unsupported("cell value %r" % (v,))
    return c


def wrong_argument_count(node):
    """True when some call has a number of arguments its function does not take."""
    if node[0] == "call":
        if node[1] in ARITY:
            lo, hi = ARITY[node[1]]
            n = len(node[2])
            if n < lo or (hi is not None and n > hi):
                return True
            if node[1] in PAIRED and (n - PAIRED[node[1]]) % 2:
                return True
        return any(wrong_argument_count(a) for a in node[2])
    return any(wrong_argument_count(p) for p in node[1:] if isinstance(p, tuple))


# functions whose arguments after the first few come in pairs
PAIRED = {"IFS": 0, "COUNTIFS": 0, "SUMIFS": 1, "AVERAGEIFS": 1, "MAXIFS": 1, "MINIFS": 1}


class Tab:
    def __init__(self, cells_json):
        self.cells = {}
        for a1, v in cells_json.items():
            if v is not None:
                self.cells[parse_address(a1)] = cell_from_json(v)

    def measure(self):
        self.rows = max([1000] + [r for r, _ in self.cells])
        self.cols = max([26] + [c for _, c in self.cells])


class Sheet:
    """A workbook: the tab Sheet1 with the row's cells and formula, and the
    other tabs the row names."""
    def __init__(self, cells_json, tabs_json, formula_text, at, today):
        self.tabs = {TAB_NAME.casefold(): Tab(cells_json)}
        for name, cells in (tabs_json or {}).items():
            if name.casefold() in self.tabs:
                raise Unsupported("two tabs named " + name)
            self.tabs[name.casefold()] = Tab(cells)
        self.home = TAB_NAME.casefold()
        self.at = parse_address(at)
        if formula_text is not None:
            self.tabs[self.home].cells[self.at] = cell_from_json(formula_text)
        self.today = today
        self.memo = {}
        self.formulas = []
        for key, tab in self.tabs.items():
            tab.measure()
            for pos, cell in tab.cells.items():
                if cell.kind != "formula":
                    continue
                try:
                    cell.ast = Parser(cell.value).parse()
                    if wrong_argument_count(cell.ast):
                        cell.error = "#ERROR!"      # section 9.3
                    else:
                        self.formulas.append((key,) + pos)
                except SyntaxProblem:
                    cell.error = "#ERROR!"
        self.cyclic = self.find_cycles()

    # static dependencies cover every argument (section 9.3)
    def bounds(self, node, home):
        """(tab key, r1, c1, r2, c2) of a reference, or None for a missing tab."""
        _, name, r1, c1, r2, c2, _ = node
        key = home if name is None else name.casefold()
        tab = self.tabs.get(key)
        if tab is None:
            return None
        r2 = tab.rows if r2 is None else r2
        c2 = tab.cols if c2 is None else c2
        r1, r2 = min(r1, r2), max(r1, r2)
        c1, c2 = min(c1, c2), max(c1, c2)
        return key, r1, c1, r2, c2

    def static_refs(self, node, home, out):
        if node[0] == "ref":
            b = self.bounds(node, home)
            if b:
                out.append(b)
        elif node[0] == "call":
            for a in node[2]:
                self.static_refs(a, home, out)
        else:
            for part in node[1:]:
                if isinstance(part, tuple):
                    self.static_refs(part, home, out)

    def find_cycles(self):
        edges = {}
        for p in self.formulas:
            boxes = []
            self.static_refs(self.tabs[p[0]].cells[p[1:]].ast, p[0], boxes)
            edges[p] = [q for q in self.formulas
                        if any(b[0] == q[0] and b[1] <= q[1] <= b[3] and b[2] <= q[2] <= b[4]
                               for b in boxes)]
        cyclic = set()
        for start in self.formulas:
            seen, stack = set(), list(edges[start])
            while stack:
                q = stack.pop()
                if q == start:
                    cyclic.add(start)
                    break
                if q not in seen:
                    seen.add(q)
                    stack.extend(edges[q])
        return cyclic

    def cell_value(self, key, r, c):
        """Value of a cell; raises XErr when the cell holds an error."""
        cell = self.tabs[key].cells.get((r, c))
        if cell is None:
            return BLANK
        if cell.kind != "formula":
            return cell.value
        k = (key, r, c)
        if k not in self.memo:
            if cell.error:
                self.memo[k] = XErr(cell.error)
            elif k in self.cyclic:
                self.memo[k] = XErr("#CYCLE!")
            else:
                try:
                    self.memo[k] = Evaluator(self, k).scalar(cell.ast)
                except XErr as e:
                    self.memo[k] = e
        v = self.memo[k]
        if isinstance(v, XErr):
            raise XErr(v.code)
        return v


class ValueRange:
    """A direct value where a function reads a range: a range of one cell.
    The value is no error: an error there is the error of the argument."""
    nrows = ncols = 1

    def __init__(self, value):
        self.value = value

    def size(self):
        return 1, 1

    def filled(self, i, j):
        return self.value is not BLANK

    def get(self, i, j):
        return self.value

    def peek(self, i, j):
        return self.value

    def positions(self):
        yield 0, 0

    def values(self):
        yield self.get(0, 0)

    def peeks(self):
        yield self.peek(0, 0)


class Range:
    def __init__(self, sheet, tab, r1, c1, r2, c2):
        self.sheet, self.tab, self.r1, self.c1, self.r2, self.c2 = sheet, tab, r1, c1, r2, c2
        self.nrows, self.ncols = r2 - r1 + 1, c2 - c1 + 1

    def size(self):
        return self.nrows, self.ncols

    def filled(self, i, j):
        """True when the cell holds anything, a formula with a blank value included."""
        return (self.r1 + i, self.c1 + j) in self.sheet.tabs[self.tab].cells

    def get(self, i, j):
        """Value at zero-based offset; raises XErr for an error cell."""
        return self.sheet.cell_value(self.tab, self.r1 + i, self.c1 + j)

    def peek(self, i, j):
        """Value at zero-based offset with an error returned, not raised."""
        try:
            return self.get(i, j)
        except XErr as e:
            return e

    def positions(self):
        for i in range(self.nrows):
            for j in range(self.ncols):
                yield i, j

    def values(self):
        for i, j in self.positions():
            yield self.get(i, j)

    def peeks(self):
        for i, j in self.positions():
            yield self.peek(i, j)


# ---------------------------------------------------------------- evaluation

# Unicode simple case folding (CaseFolding.txt, statuses C and S) and simple
# case mapping (UnicodeData.txt), in the Unicode version of the Python that
# runs this file (unicodedata.unidata_version, printed in the summary).
#
# Folding is derived from Python's own data, with no listed exceptions:
# a code point whose full fold (str.casefold) is one code point folds to it
# (status C). Code points whose full fold is longer (status F) and equal
# form a group; a group of two or more is simple-fold equivalent (status S)
# and folds to its smallest member that is its own lower case. A code point
# alone in its group folds to itself. So the sharp s equals the capital
# sharp s and not "ss", and capital I with dot above equals only itself.
_FOLD_TABLE = None


def fold_table():
    """Code point to its simple fold, for the code points that change."""
    global _FOLD_TABLE
    if _FOLD_TABLE is None:
        table, groups = {}, {}
        for cp in range(0x110000):
            if 0xD800 <= cp <= 0xDFFF:
                continue
            ch = chr(cp)
            full = ch.casefold()
            if len(full) == 1:
                if full != ch:
                    table[cp] = ord(full)
            else:
                groups.setdefault(full, []).append(cp)
        for members in groups.values():
            if len(members) > 1:
                own_lower = [cp for cp in members if chr(cp).lower() == chr(cp)]
                target = min(own_lower or members)
                for cp in members:
                    if cp != target:
                        table[cp] = target
        _FOLD_TABLE = table
    return _FOLD_TABLE


def _block(first_from, first_to, count):
    return {first_from + k: first_to + k for k in range(count)}


# Simple case mapping: Python's full mapping of one code point, unchanged
# when that has several code points, except where UnicodeData gives a
# single code point all the same.
# the simple lower case of capital I with dot above is i
SIMPLE_LOWER = {0x0130: 0x0069}
# the simple upper case of the Greek small letters with ypogegrammeni
SIMPLE_UPPER = {0x1FB3: 0x1FBC, 0x1FC3: 0x1FCC, 0x1FF3: 0x1FFC}
for _first in (0x1F80, 0x1F90, 0x1FA0):
    SIMPLE_UPPER.update(_block(_first, _first + 8, 8))


def _map(text, table, full):
    out = []
    for ch in text:
        if ord(ch) in table:
            out.append(chr(table[ord(ch)]))
        else:
            m = full(ch)
            out.append(m if len(m) == 1 else ch)
    return "".join(out)


def fold(s):
    """Simple case folding: the two sigmas are equal, sharp s is not ss."""
    table = fold_table()
    return "".join(chr(table.get(ord(ch), ord(ch))) for ch in s)


def simple_upper(s):
    return _map(s, SIMPLE_UPPER, str.upper)


def simple_lower(s):
    return _map(s, SIMPLE_LOWER, str.lower)


def check_text(s):
    if len(s) > MAX_TEXT:
        raise XErr("#VALUE!")
    return s


def to_number(v):
    if is_num(v):
        return v
    if isinstance(v, bool):
        return 1.0 if v else 0.0
    if v is BLANK:
        return 0.0
    n = text_to_number(v)
    if n is None:
        raise XErr("#VALUE!")
    return n


def to_text(v):
    if isinstance(v, str):
        return v
    if isinstance(v, bool):
        return "TRUE" if v else "FALSE"
    if v is BLANK:
        return ""
    return general(v)


def to_bool(v):
    if isinstance(v, bool):
        return v
    if is_num(v):
        return v != 0
    if v is BLANK:
        return False
    if v.upper() in ("TRUE", "FALSE"):
        return v.upper() == "TRUE"
    raise XErr("#VALUE!")


def finite(x):
    if isinstance(x, float) and (math.isinf(x) or math.isnan(x)):
        raise XErr("#NUM!")
    return float(x)


def kind_rank(v):
    return 0 if is_num(v) else (1 if isinstance(v, str) else 2)


def compare(a, b):
    """-1, 0 or 1 by section 9.3; None when the kinds differ and are unordered equal."""
    if a is BLANK and b is BLANK:
        return 0
    if a is BLANK:
        a = 0.0 if is_num(b) else ("" if isinstance(b, str) else False)
    if b is BLANK:
        b = 0.0 if is_num(a) else ("" if isinstance(a, str) else False)
    ra, rb = kind_rank(a), kind_rank(b)
    if ra != rb:
        return -1 if ra < rb else 1
    if ra == 0:
        a, b = dec15(a), dec15(b)
    elif ra == 1:
        a, b = [ord(ch) for ch in fold(a)], [ord(ch) for ch in fold(b)]
    return (a > b) - (a < b)


def values_equal(a, b):
    return compare(a, b) == 0


MAX_PATTERN = 255       # code points of a pattern that holds a wildcard


def has_wildcard(pattern):
    """True when the pattern holds a * or ? that no ~ stands before."""
    i = 0
    while i < len(pattern):
        if pattern[i] == "~" and i + 1 < len(pattern) and pattern[i + 1] in "*?~":
            i += 2
            continue
        if pattern[i] in "*?":
            return True
        i += 1
    return False


def pattern_regex(pattern):
    """The compiled pattern. One that holds a wildcard and is too long is #VALUE!."""
    if has_wildcard(pattern) and len(pattern) > MAX_PATTERN:
        raise XErr("#VALUE!")
    return re.compile(wildcard_regex(fold(pattern)), re.S)


def wildcard_regex(pattern):
    out, i = [], 0
    while i < len(pattern):
        ch = pattern[i]
        if ch == "~" and i + 1 < len(pattern) and pattern[i + 1] in "*?~":
            out.append(re.escape(pattern[i + 1]))
            i += 2
            continue
        out.append(".*" if ch == "*" else ("." if ch == "?" else re.escape(ch)))
        i += 1
    return "".join(out)


def criterion(c):
    """A predicate over cell values for the criteria of the conditional totals."""
    op, operand = "=", c
    if isinstance(c, str):
        for cand in ("<>", "<=", ">=", "=", "<", ">"):
            if c.startswith(cand):
                op, operand = cand, c[len(cand):]
                break
        if operand == "":
            if op == "=":
                return lambda v: v is BLANK or v == ""
            if op == "<>":
                return lambda v: not (v is BLANK or v == "")
            return lambda v: False
        kind, value, _ = read_typed(operand) if operand[:1] not in ("'", "=") else ("text", operand, None)
        if kind in ("number", "date", "time", "datetime", "bool"):
            operand = value
    elif c is BLANK:
        # empty text against text and blank cells, 0 against numbers
        return lambda v: v is BLANK or v == "" or (is_num(v) and dec15(v) == 0)

    if is_num(operand):
        n = dec15(operand)

        def eq(v):
            if is_num(v):
                return dec15(v) == n
            if isinstance(v, str) and not isinstance(v, bool):
                t = text_to_number(v)
                return t is not None and dec15(t) == n
            return False

        def rel(v):
            if not is_num(v):
                return None
            d = dec15(v)
            return (d > n) - (d < n)
    elif isinstance(operand, bool):
        def eq(v):
            return isinstance(v, bool) and v == operand

        def rel(v):
            return None
    else:
        # after = and <> the text is a pattern; after < and > it is compared
        rx = pattern_regex(operand) if op in ("=", "<>") else None
        key = [ord(ch) for ch in fold(operand)]

        def eq(v):
            return isinstance(v, str) and rx.fullmatch(fold(v)) is not None

        def rel(v):
            if not isinstance(v, str):
                return None
            k = [ord(ch) for ch in fold(v)]
            return (k > key) - (k < key)

    def pred(v):
        if isinstance(v, XErr):
            return False
        if op == "=":
            return eq(v)
        if op == "<>":
            return not eq(v)
        r = rel(v)
        if r is None:
            return False
        return {"<": r < 0, "<=": r <= 0, ">": r > 0, ">=": r >= 0}[op]
    return pred


def parse_text_pattern(p):
    """The number format a TEXT pattern names (rule 6), else None."""
    m = re.match(r"^(#,##)?0(?:\.(0{1,10}))?$", p)
    if m:
        return {"kind": "number", "decimals": len(m.group(2) or ""), "thousands": bool(m.group(1))}
    m = re.match(r"^0(?:\.(0{1,10}))?%$", p)
    if m:
        return {"kind": "percent", "decimals": len(m.group(1) or "")}
    m = re.match(r"^(.{1,3}?)(#,##)?0(?:\.(0{1,10}))?$", p, re.S)
    if m and all(ch.isalpha() or ch in CURRENCY_SIGNS for ch in m.group(1)):
        return {"kind": "currency", "symbol": m.group(1), "decimals": len(m.group(3) or ""),
                "thousands": bool(m.group(2))}
    if p in DATE_PATTERNS:
        return {"kind": "date", "pattern": p}
    if p in TIME_PATTERNS:
        return {"kind": "time", "pattern": p}
    for dp in DATE_PATTERNS:
        for tp in TIME_PATTERNS:
            if p == dp + " " + tp:
                return {"kind": "datetime", "date": dp, "time": tp}
    return None


class Evaluator:
    def __init__(self, sheet, at):
        self.sheet = sheet
        self.at = at

    # ---- nodes
    def is_ref(self, node):
        return node[0] == "ref"

    def range_of(self, node, strict=False):
        if node[0] != "ref":
            # A direct value that is an error is the error of the argument,
            # like a range on a tab that does not exist: scalar raises it.
            value = self.scalar(node)
            if strict:
                raise XErr("#VALUE!")
            return ValueRange(value)
        _, tab, r1, c1, r2, c2, _ = node
        for r in (r1, r2):
            if r is not None and not 1 <= r <= MAX_ROWS:
                raise XErr("#REF!")
        for c in (c1, c2):
            if c is not None and not 1 <= c <= MAX_COLS:
                raise XErr("#REF!")
        b = self.sheet.bounds(node, self.at[0])
        if b is None:
            raise XErr("#REF!")
        return Range(self.sheet, *b)

    def scalar(self, node):
        t = node[0]
        if t == "num" or t == "str" or t == "bool":
            return node[1]
        if t == "empty":
            return BLANK
        if t == "name":
            raise XErr("#NAME?")
        if t == "err":
            raise XErr(node[1])
        if t == "ref":
            rng = self.range_of(node)
            if not node[6]:
                raise XErr("#VALUE!")
            return rng.get(0, 0)
        if t == "un":
            x = to_number(self.scalar(node[2]))
            return -x if node[1] == "-" else x
        if t == "pct":
            return to_number(self.scalar(node[1])) / 100.0
        if t == "bin":
            a = to_number(self.scalar(node[2]))
            b = to_number(self.scalar(node[3]))
            return self.arith(node[1], a, b)
        if t == "cat":
            a = self.scalar(node[1])
            b = self.scalar(node[2])
            return check_text(to_text(a) + to_text(b))
        if t == "cmp":
            a = self.scalar(node[2])
            b = self.scalar(node[3])
            c = compare(a, b)
            return {"=": c == 0, "<>": c != 0, "<": c < 0, "<=": c <= 0,
                    ">": c > 0, ">=": c >= 0}[node[1]]
        if t == "call":
            return self.call(node[1], node[2])
        raise Unsupported("node " + t)

    def arith(self, op, a, b):
        try:
            if op == "+":
                return finite(a + b)
            if op == "-":
                return finite(a - b)
            if op == "*":
                return finite(a * b)
            if op == "/":
                if b == 0:
                    raise XErr("#DIV/0!")
                return finite(a / b)
            return self.power(a, b)
        except OverflowError:
            raise XErr("#NUM!")

    def power(self, a, b):
        if a == 0 and b == 0:
            return 1.0
        if a == 0 and b < 0:
            raise XErr("#DIV/0!")
        if a < 0 and b != math.floor(b):
            raise XErr("#NUM!")
        try:
            return finite(math.pow(a, b))
        except (OverflowError, ValueError):
            raise XErr("#NUM!")

    # ---- argument helpers
    def num(self, node):
        return to_number(self.scalar(node))

    def integer(self, node):
        return int(math.trunc(self.num(node)))

    def text(self, node):
        return to_text(self.scalar(node))

    def truth(self, node):
        return to_bool(self.scalar(node))

    def peek(self, node):
        try:
            return self.scalar(node)
        except XErr as e:
            return e

    def numbers(self, args):
        """Numbers of totals and statistics (section 9.4): text and booleans in a
        range are passed over, a direct boolean or numeric text counts."""
        out = []
        for a in args:
            if self.is_ref(a):
                out.extend(v for v in self.range_of(a).values() if is_num(v))
            else:
                out.append(to_number(self.scalar(a)))
        return out

    def call(self, name, args):
        if name not in ARITY:
            raise XErr("#NAME?")
        return getattr(self, "f_" + name)(args)      # argument counts are checked before evaluation

    # ---- math
    def f_ABS(self, a):
        return abs(self.num(a[0]))

    def ceil_floor(self, a, up):
        x = self.num(a[0])
        s = self.num(a[1]) if len(a) > 1 else 1.0
        if s == 0:
            if up:
                return 0.0
            raise XErr("#DIV/0!")
        if x == 0:
            return 0.0
        if x > 0 and s < 0:
            raise XErr("#NUM!")
        q = Decimal(x) / Decimal(s)
        # x/s > 0 when the signs agree: CEILING moves away from zero, FLOOR toward it.
        # x < 0 with s > 0: CEILING moves toward zero (up), FLOOR away from it (down).
        n = q.to_integral_value(rounding="ROUND_CEILING" if up else "ROUND_FLOOR")
        return float(n * Decimal(s))

    def f_CEILING(self, a):
        return self.ceil_floor(a, True)

    def f_FLOOR(self, a):
        return self.ceil_floor(a, False)

    def f_EXP(self, a):
        try:
            return finite(math.exp(self.num(a[0])))
        except OverflowError:
            raise XErr("#NUM!")

    def f_INT(self, a):
        return float(math.floor(self.num(a[0])))

    def log(self, x, base):
        if x <= 0 or base <= 0:
            raise XErr("#NUM!")
        if base == 1:
            raise XErr("#DIV/0!")
        return math.log(x) / math.log(base)

    def f_LN(self, a):
        return self.log(self.num(a[0]), math.e)

    def f_LOG(self, a):
        return self.log(self.num(a[0]), self.num(a[1]) if len(a) > 1 else 10.0)

    def f_LOG10(self, a):
        return self.log(self.num(a[0]), 10.0)

    def f_MOD(self, a):
        x, y = self.num(a[0]), self.num(a[1])
        if y == 0:
            raise XErr("#DIV/0!")
        return x - y * math.floor(x / y)

    def f_PI(self, a):
        return math.pi

    def f_POWER(self, a):
        x, y = self.num(a[0]), self.num(a[1])
        return self.power(x, y)

    def f_PRODUCT(self, a):
        nums = self.numbers(a)
        if not nums:
            return 0.0
        p = 1.0
        for n in nums:
            p *= n
        return finite(p)

    def f_QUOTIENT(self, a):
        x, y = self.num(a[0]), self.num(a[1])
        if y == 0:
            raise XErr("#DIV/0!")
        return float(math.trunc(x / y))

    def rounding(self, a, mode):
        d = dec15(self.num(a[0]))
        digits = self.integer(a[1]) if len(a) > 1 else 0
        return float(d.quantize(Decimal(1).scaleb(-digits), rounding=mode))

    def f_ROUND(self, a):
        return self.rounding(a, ROUND_HALF_UP)

    def f_ROUNDUP(self, a):
        return self.rounding(a, "ROUND_UP")

    def f_ROUNDDOWN(self, a):
        return self.rounding(a, "ROUND_DOWN")

    def f_TRUNC(self, a):
        return self.rounding(a, "ROUND_DOWN")

    def f_SIGN(self, a):
        x = self.num(a[0])
        return float((x > 0) - (x < 0))

    def f_SQRT(self, a):
        x = self.num(a[0])
        if x < 0:
            raise XErr("#NUM!")
        return math.sqrt(x)

    def f_SUM(self, a):
        return finite(math.fsum(self.numbers(a)))

    def f_SUMPRODUCT(self, a):
        grids = []
        for node in a:
            if self.is_ref(node):
                rng = self.range_of(node)
                grids.append((rng.size(), [v if is_num(v) else 0.0 for v in rng.values()]))
            else:
                v = self.scalar(node)
                grids.append(((1, 1), [v if is_num(v) else 0.0]))
        if any(g[0] != grids[0][0] for g in grids):
            raise XErr("#VALUE!")
        total = 0.0
        for k in range(len(grids[0][1])):
            p = 1.0
            for g in grids:
                p *= g[1][k]
            total += p
        return finite(total)

    # ---- statistics
    def f_AVERAGE(self, a):
        nums = self.numbers(a)
        if not nums:
            raise XErr("#DIV/0!")
        return math.fsum(nums) / len(nums)

    def f_COUNT(self, a):
        n = 0
        for node in a:
            if self.is_ref(node):
                n += sum(1 for v in self.range_of(node).peeks() if is_num(v))
            else:
                v = self.peek(node)
                if is_num(v) or isinstance(v, bool):
                    n += 1
                elif isinstance(v, str) and text_to_number(v) is not None:
                    n += 1
        return float(n)

    def f_COUNTA(self, a):
        n = 0
        for node in a:
            if self.is_ref(node):
                rng = self.range_of(node)
                n += sum(1 for p in rng.positions() if rng.filled(*p))
            else:
                n += 1
        return float(n)

    def f_COUNTBLANK(self, a):
        return float(sum(1 for v in self.range_of(a[0]).peeks() if v is BLANK or v == ""))

    def kth(self, a, largest):
        nums = sorted(v for v in self.range_of(a[0]).values() if is_num(v))
        k = self.integer(a[1])
        if k < 1 or k > len(nums):
            raise XErr("#NUM!")
        return nums[-k] if largest else nums[k - 1]

    def f_LARGE(self, a):
        return self.kth(a, True)

    def f_SMALL(self, a):
        return self.kth(a, False)

    def f_MAX(self, a):
        nums = self.numbers(a)
        return max(nums) if nums else 0.0

    def f_MIN(self, a):
        nums = self.numbers(a)
        return min(nums) if nums else 0.0

    def f_MEDIAN(self, a):
        nums = self.numbers(a)
        if not nums:
            raise XErr("#NUM!")
        return float(statistics.median(nums))

    def f_RANK(self, a):
        v = dec15(self.num(a[0]))
        nums = [dec15(x) for x in self.range_of(a[1]).values() if is_num(x)]
        ascending = len(a) > 2 and self.num(a[2]) != 0
        if v not in nums:
            raise XErr("#N/A")
        return float(1 + sum(1 for x in nums if (x < v if ascending else x > v)))

    def spread(self, a, sample, root):
        nums = self.numbers(a)
        if len(nums) < (2 if sample else 1):
            raise XErr("#DIV/0!")
        mean = math.fsum(nums) / len(nums)
        ss = math.fsum((x - mean) ** 2 for x in nums)
        var = ss / (len(nums) - 1 if sample else len(nums))
        return math.sqrt(var) if root else var

    def f_STDEV(self, a):
        return self.spread(a, True, True)

    def f_STDEVP(self, a):
        return self.spread(a, False, True)

    def f_VAR(self, a):
        return self.spread(a, True, False)

    def f_VARP(self, a):
        return self.spread(a, False, False)

    # ---- conditional totals
    def cond(self, pairs, value_node=None, value_first=False):
        """Offsets where every (range, criteria) pair matches, and the range of
        the numbers. The arguments are read in the order they are written, so
        the leftmost one that is an error is the result: a range on a tab that
        does not exist, a direct value that is an error where a range is read,
        or criteria that are an error. Ranges of different sizes come after. A
        direct value that is no error is a range of one cell."""
        values = None
        if value_node is not None and value_first:
            values = self.range_of(value_node)
        ranges, preds = [], []
        for r, c in pairs:
            ranges.append(self.range_of(r))
            preds.append(criterion(self.scalar(c)))
        if value_node is not None and not value_first:
            values = self.range_of(value_node)
        size = ranges[0].size()
        if any(r.size() != size for r in ranges) or (values is not None and values.size() != size):
            raise XErr("#VALUE!")
        hits = [p for p in ranges[0].positions()
                if all(pred(r.peek(*p)) for r, pred in zip(ranges, preds))]
        return hits, values

    def cond_values(self, value_node, pairs, value_first):
        hits, values = self.cond(pairs, value_node, value_first)
        return [v for v in (values.get(*p) for p in hits) if is_num(v)]

    def pairs(self, a):
        return [(a[i], a[i + 1]) for i in range(0, len(a), 2)]

    def f_COUNTIF(self, a):
        return float(len(self.cond([(a[0], a[1])])[0]))

    def f_COUNTIFS(self, a):
        return float(len(self.cond(self.pairs(a))[0]))

    def f_SUMIF(self, a):
        return math.fsum(self.cond_values(a[2] if len(a) > 2 else a[0], [(a[0], a[1])], False))

    def f_SUMIFS(self, a):
        return math.fsum(self.cond_values(a[0], self.pairs(a[1:]), True))

    def average_of(self, nums):
        if not nums:
            raise XErr("#DIV/0!")
        return math.fsum(nums) / len(nums)

    def f_AVERAGEIF(self, a):
        return self.average_of(self.cond_values(a[2] if len(a) > 2 else a[0], [(a[0], a[1])], False))

    def f_AVERAGEIFS(self, a):
        return self.average_of(self.cond_values(a[0], self.pairs(a[1:]), True))

    def f_MAXIFS(self, a):
        nums = self.cond_values(a[0], self.pairs(a[1:]), True)
        return max(nums) if nums else 0.0

    def f_MINIFS(self, a):
        nums = self.cond_values(a[0], self.pairs(a[1:]), True)
        return min(nums) if nums else 0.0

    # ---- logic
    def truths(self, a, stop_at=None):
        """Truth values in order; stops early when stop_at is seen (section 9.3).
        Arguments without any logical value are #VALUE!, as Excel documents."""
        out = []
        for node in a:
            for v in self.truth_values(node):
                out.append(v)
                if stop_at is not None and v == stop_at:
                    return out
        if not out:
            raise XErr("#VALUE!")
        return out

    def truth_values(self, node):
        """The truth values of one argument, one at a time. A range is read
        row by row and raises at its first error cell only when the reading
        gets there, so a deciding value before it ends the reading first."""
        if self.is_ref(node) and not node[6]:
            for v in self.range_of(node).values():
                if isinstance(v, bool) or is_num(v):
                    yield to_bool(v)
        elif self.is_ref(node):
            v = self.scalar(node)
            if not (isinstance(v, str) or v is BLANK):
                yield to_bool(v)
        else:
            yield self.truth(node)

    def f_AND(self, a):
        return all(self.truths(a, stop_at=False))

    def f_OR(self, a):
        return any(self.truths(a, stop_at=True))

    def f_XOR(self, a):
        return sum(self.truths(a)) % 2 == 1

    def f_NOT(self, a):
        return not self.truth(a[0])

    def f_TRUE(self, a):
        return True

    def f_FALSE(self, a):
        return False

    def f_NA(self, a):
        raise XErr("#N/A")

    def f_IF(self, a):
        if self.truth(a[0]):
            return self.scalar(a[1])
        return self.scalar(a[2]) if len(a) > 2 else False

    def f_IFERROR(self, a):
        try:
            return self.scalar(a[0])
        except XErr:
            return self.scalar(a[1]) if len(a) > 1 else BLANK

    def f_IFNA(self, a):
        try:
            return self.scalar(a[0])
        except XErr as e:
            if e.code != "#N/A":
                raise
            return self.scalar(a[1]) if len(a) > 1 else BLANK

    def f_IFS(self, a):
        for cond, value in self.pairs(a):
            if self.truth(cond):
                return self.scalar(value)
        raise XErr("#N/A")

    def f_SWITCH(self, a):
        key = self.scalar(a[0])
        rest = a[1:]
        while len(rest) >= 2:
            if values_equal(key, self.scalar(rest[0])):
                return self.scalar(rest[1])
            rest = rest[2:]
        if rest:
            return self.scalar(rest[0])
        raise XErr("#N/A")

    def f_ISBLANK(self, a):
        if self.is_ref(a[0]) and a[0][6]:
            return not self.range_of(a[0]).filled(0, 0)
        return self.peek(a[0]) is BLANK

    def f_ISERROR(self, a):
        return isinstance(self.peek(a[0]), XErr)

    def f_ISNA(self, a):
        v = self.peek(a[0])
        return isinstance(v, XErr) and v.code == "#N/A"

    def f_ISLOGICAL(self, a):
        return isinstance(self.peek(a[0]), bool)

    def f_ISNUMBER(self, a):
        return is_num(self.peek(a[0]))

    def f_ISTEXT(self, a):
        return isinstance(self.peek(a[0]), str)

    # ---- text
    def pieces(self, args, ranges_ok=True):
        for node in args:
            if self.is_ref(node) and not node[6]:
                if not ranges_ok:
                    raise XErr("#VALUE!")
                for v in self.range_of(node).values():
                    yield to_text(v)
            else:
                yield self.text(node)

    def join(self, parts, sep=""):
        out, n = [], 0
        for k, p in enumerate(parts):
            n += len(p) + (len(sep) if k else 0)
            if n > MAX_TEXT:
                raise XErr("#VALUE!")
            out.append(p)
        return sep.join(out)

    def f_CONCAT(self, a):
        return self.join(self.pieces(a))

    def f_CONCATENATE(self, a):
        return self.join(self.pieces(a, ranges_ok=False))

    def f_TEXTJOIN(self, a):
        sep = self.text(a[0])
        skip = self.truth(a[1])
        parts = [p for p in self.pieces(a[2:]) if not (skip and p == "")]
        return self.join(parts, sep)

    def f_EXACT(self, a):
        x, y = self.text(a[0]), self.text(a[1])
        return x == y

    def find(self, a, wild):
        needle, hay = self.text(a[0]), self.text(a[1])
        start = self.integer(a[2]) if len(a) > 2 else 1
        if start < 1 or start > max(len(hay), 1) or (hay == "" and needle != ""):
            raise XErr("#VALUE!")
        if wild:
            m = pattern_regex(needle).search(fold(hay), start - 1)
            pos = m.start() if m else -1
        else:
            pos = hay.find(needle, start - 1)
        if pos < 0:
            raise XErr("#VALUE!")
        return float(pos + 1)

    def f_FIND(self, a):
        return self.find(a, False)

    def f_SEARCH(self, a):
        return self.find(a, True)

    def count_arg(self, a, i, default):
        n = self.integer(a[i]) if len(a) > i else default
        if n < 0:
            raise XErr("#VALUE!")
        return n

    def f_LEFT(self, a):
        s = self.text(a[0])
        return s[:self.count_arg(a, 1, 1)]

    def f_RIGHT(self, a):
        s = self.text(a[0])
        n = self.count_arg(a, 1, 1)
        return s[len(s) - n:] if n < len(s) else s

    def f_MID(self, a):
        s, start = self.text(a[0]), self.integer(a[1])
        n = self.count_arg(a, 2, 0)
        if start < 1:
            raise XErr("#VALUE!")
        return s[start - 1:start - 1 + n]

    def f_LEN(self, a):
        return float(len(self.text(a[0])))

    def f_LOWER(self, a):
        return simple_lower(self.text(a[0]))

    def f_UPPER(self, a):
        return simple_upper(self.text(a[0]))

    def f_PROPER(self, a):
        out, prev_letter = [], False
        for ch in self.text(a[0]):
            out.append((simple_lower if prev_letter else simple_upper)(ch))
            prev_letter = ch.isalpha()
        return "".join(out)

    def f_REPLACE(self, a):
        s, start = self.text(a[0]), self.integer(a[1])
        n = self.count_arg(a, 2, 0)
        new = self.text(a[3])
        if start < 1:
            raise XErr("#VALUE!")
        return check_text(s[:start - 1] + new + s[start - 1 + n:])

    def f_REPT(self, a):
        s = self.text(a[0])
        n = self.count_arg(a, 1, 0)
        if len(s) * n > MAX_TEXT:
            raise XErr("#VALUE!")
        return s * n

    def f_SUBSTITUTE(self, a):
        s, old, new = self.text(a[0]), self.text(a[1]), self.text(a[2])
        which = None
        if len(a) > 3:
            which = self.integer(a[3])
            if which < 1:
                raise XErr("#VALUE!")
        if old == "":
            return s
        if which is None:
            if len(s) + s.count(old) * (len(new) - len(old)) > MAX_TEXT:
                raise XErr("#VALUE!")
            return s.replace(old, new)
        pos = -1
        for _ in range(which):
            pos = s.find(old, pos + 1 if pos < 0 else pos + len(old))
            if pos < 0:
                return s
        return check_text(s[:pos] + new + s[pos + len(old):])

    def f_TRIM(self, a):
        return " ".join(p for p in self.text(a[0]).split(" ") if p != "")

    def f_TEXT(self, a):
        v = self.scalar(a[0])
        nf = parse_text_pattern(self.text(a[1]))
        if nf is None:
            raise XErr("#VALUE!")
        if isinstance(v, str) and not isinstance(v, bool) and text_to_number(v) is None:
            return v                  # text that is no number is returned unchanged
        return show_number(to_number(v), nf)

    def f_VALUE(self, a):
        v = self.scalar(a[0])
        if isinstance(v, bool):
            raise XErr("#VALUE!")
        return to_number(v)

    # ---- lookup
    def f_CHOOSE(self, a):
        k = self.integer(a[0])
        if k < 1 or k > len(a) - 1:
            raise XErr("#NUM!")
        return self.scalar(a[k])

    def f_ROW(self, a):
        return float(self.range_of(a[0], strict=True).r1 if a else self.at[1])

    def f_COLUMN(self, a):
        return float(self.range_of(a[0], strict=True).c1 if a else self.at[2])

    def f_ROWS(self, a):
        return float(self.range_of(a[0]).nrows)

    def f_COLUMNS(self, a):
        return float(self.range_of(a[0]).ncols)

    def f_INDEX(self, a):
        rng = self.range_of(a[0])
        row = self.integer(a[1])
        if len(a) > 2:
            col = self.integer(a[2])
        elif rng.ncols == 1:
            col = 1
        elif rng.nrows == 1:
            row, col = 1, row             # one index on one row is the column
        else:
            raise XErr("#VALUE!")         # one index on a rectangle would be a whole row
        if row == 0 or col == 0:
            raise XErr("#VALUE!")            # INDEX gives one value (section 9.4)
        if row < 0 or col < 0 or row > rng.nrows or col > rng.ncols:
            raise XErr("#NUM!")              # as Google Sheets; Excel gives #REF! or #VALUE!
        return rng.get(row - 1, col - 1)

    def find_exact(self, key, vals, wild):
        if key is BLANK:
            for k, v in enumerate(vals):
                if v == "" or (is_num(v) and dec15(v) == 0):
                    return k
            return None
        rx = None
        if wild and isinstance(key, str):
            rx = pattern_regex(key)
        for k, v in enumerate(vals):
            if isinstance(v, XErr) or v is BLANK:
                continue
            if rx is not None:
                if isinstance(v, str) and rx.fullmatch(fold(v)):
                    return k
            elif kind_rank(v) == kind_rank(key) and compare(v, key) == 0:
                return k
        return None

    def find_sorted(self, key, vals):
        """The last value not above the key, on ascending data of the key's kind."""
        return self.find_ordered(key, vals, lambda c: c <= 0)

    def find_ordered(self, key, vals, keep):
        if key is BLANK:
            key = 0.0
        best = None
        for k, v in enumerate(vals):
            if isinstance(v, XErr) or v is BLANK or kind_rank(v) != kind_rank(key):
                continue
            if keep(compare(v, key)):
                best = k
        return best

    def lookup(self, key, vals, sorted_mode):
        if sorted_mode == -1:             # descending data: the last value not under the key
            k = self.find_ordered(key, vals, lambda c: c >= 0)
        else:
            k = self.find_sorted(key, vals) if sorted_mode else self.find_exact(key, vals, True)
        if k is None:
            raise XErr("#N/A")
        return k

    def f_MATCH(self, a):
        key = self.scalar(a[0])
        rng = self.range_of(a[1])
        mode = self.num(a[2]) if len(a) > 2 else 1.0
        if rng.nrows != 1 and rng.ncols != 1:
            raise XErr("#N/A")
        mode = 0 if mode == 0 else (1 if mode > 0 else -1)
        return float(self.lookup(key, list(rng.peeks()), mode) + 1)

    def table_lookup(self, a, vertical):
        key = self.scalar(a[0])
        rng = self.range_of(a[1])
        index = self.integer(a[2])
        sorted_mode = self.truth(a[3]) if len(a) > 3 else True
        if index < 1:
            raise XErr("#VALUE!")
        if index > (rng.ncols if vertical else rng.nrows):
            raise XErr("#REF!")
        n = rng.nrows if vertical else rng.ncols
        keys = [rng.peek(k, 0) if vertical else rng.peek(0, k) for k in range(n)]
        k = self.lookup(key, keys, sorted_mode)
        return rng.get(k, index - 1) if vertical else rng.get(index - 1, k)

    def f_VLOOKUP(self, a):
        return self.table_lookup(a, True)

    def f_HLOOKUP(self, a):
        return self.table_lookup(a, False)

    def f_XLOOKUP(self, a):
        key = self.scalar(a[0])
        look, ret = self.range_of(a[1]), self.range_of(a[2])
        if (look.nrows != 1 and look.ncols != 1) or (ret.nrows != 1 and ret.ncols != 1):
            raise XErr("#VALUE!")
        if look.size() != ret.size():
            raise XErr("#VALUE!")
        k = self.find_exact(key, list(look.peeks()), False)
        if k is None:
            if len(a) > 3:
                return self.scalar(a[3])
            raise XErr("#N/A")
        return ret.get(*list(ret.positions())[k])

    # ---- date and time
    def day_number(self, node):
        x = self.num(node)
        if x < 0:
            raise XErr("#NUM!")
        return int(math.floor(x))

    def make_date(self, y, m, d):
        if y < 0:
            raise XErr("#NUM!")
        if y < 1900:
            y += 1900
        y += (m - 1) // 12
        m = (m - 1) % 12 + 1
        s = serial_of(y, m, 1) + d - 1
        if s < 0:
            raise XErr("#NUM!")
        return s

    def f_DATE(self, a):
        return float(self.make_date(self.integer(a[0]), self.integer(a[1]), self.integer(a[2])))

    def f_YEAR(self, a):
        return float(civil_of(self.day_number(a[0]))[0])

    def f_MONTH(self, a):
        return float(civil_of(self.day_number(a[0]))[1])

    def f_DAY(self, a):
        return float(civil_of(self.day_number(a[0]))[2])

    def f_DAYS(self, a):
        end, start = self.num(a[0]), self.num(a[1])
        return float(math.floor(end) - math.floor(start))

    def f_WEEKDAY(self, a):
        s = self.day_number(a[0])
        kind = self.integer(a[1]) if len(a) > 1 else 1
        monday0 = (s - 2) % 7              # day 2, 1900-01-01, is a Monday
        if kind == 1:
            return float((monday0 + 1) % 7 + 1)
        if kind == 2:
            return float(monday0 + 1)
        if kind == 3:
            return float(monday0)
        raise XErr("#NUM!")

    def shift_months(self, serial, months, to_end):
        y, m, d = civil_of(serial)
        total = y * 12 + (m - 1) + months
        y, m = total // 12, total % 12 + 1
        last = month_len(y, m)
        s = serial_of(y, m, last if to_end else min(d, last))
        if s < 0:
            raise XErr("#NUM!")
        return float(s)

    def f_EDATE(self, a):
        return self.shift_months(self.day_number(a[0]), self.integer(a[1]), False)

    def f_EOMONTH(self, a):
        return self.shift_months(self.day_number(a[0]), self.integer(a[1]), True)

    def f_DATEDIF(self, a):
        start, end = self.day_number(a[0]), self.day_number(a[1])
        unit = self.text(a[2]).upper()
        if start > end or unit not in ("Y", "M", "D", "MD", "YM", "YD"):
            raise XErr("#NUM!")
        sy, sm, sd = civil_of(start)
        ey, em, ed = civil_of(end)
        months = (ey - sy) * 12 + (em - sm) - (1 if ed < sd else 0)
        if unit == "D":
            return float(end - start)
        if unit == "M":
            return float(months)
        if unit == "Y":
            return float(months // 12)
        if unit == "YM":
            return float(months % 12)
        if unit == "MD":
            if ed >= sd:
                return float(ed - sd)
            py, pm = (ey, em - 1) if em > 1 else (ey - 1, 12)
            return float(end - (serial_of(py, pm, 1) + sd - 1))
        anchor = serial_of(ey, sm, 1) + sd - 1
        if anchor > end:
            anchor = serial_of(ey - 1, sm, 1) + sd - 1
        return float(end - anchor)

    def seconds_of(self, node):
        x = self.num(node)
        if x < 0:
            raise XErr("#NUM!")
        return split_day_time(x)[1]

    def f_HOUR(self, a):
        return float(self.seconds_of(a[0]) // 3600)

    def f_MINUTE(self, a):
        return float(self.seconds_of(a[0]) // 60 % 60)

    def f_SECOND(self, a):
        return float(self.seconds_of(a[0]) % 60)

    def f_TIME(self, a):
        total = self.integer(a[0]) * 3600 + self.integer(a[1]) * 60 + self.integer(a[2])
        if total < 0:
            raise XErr("#NUM!")
        return (total % 86400) / 86400.0

    def f_TODAY(self, a):
        return float(self.sheet.today)


# ---------------------------------------------------------------- one row

def json_value(v):
    if isinstance(v, XErr):
        return {"error": v.code}
    if v is BLANK:
        return None               # a blank value (section 9.3)
    return v


def evaluate_formula(row, formula, today):
    """(value, date_result) of the formula of a row."""
    sheet = Sheet(row["cells"], row.get("tabs"), formula, row.get("at", DEFAULT_AT), today)
    cell = sheet.tabs[sheet.home].cells[sheet.at]
    if cell.kind != "formula":
        raise Unsupported("the formula field holds no formula")
    try:
        v = sheet.cell_value(sheet.home, *sheet.at)
    except XErr as e:
        v = e
    ast = cell.ast
    date_result = bool(ast) and ast[0] == "call" and ast[1] in DATE_RESULT
    return v, date_result


def shown_of(v, nf, date_result):
    if v is BLANK:
        return ""                 # a blank value shows nothing
    if nf is None and date_result and is_num(v):
        nf = {"kind": "date", "pattern": "yyyy-mm-dd"}      # rule 11
    return show(v, nf)


def compute(row):
    """(value in get_sheet typing, shown, csv, set format) recomputed for a row."""
    if "typed" not in row:
        y, m, d = (int(p) for p in row["date"].split("-"))
        v, date_result = evaluate_formula(row, row["formula"], serial_of(y, m, d))
        shown = shown_of(v, row["format"], date_result)
        return json_value(v), shown, csv_field(shown), None
    kind, value, sets = read_typed(row["typed"])
    if kind == "formula":
        y, m, d = (int(p) for p in TYPED_DATE.split("-"))
        v, date_result = evaluate_formula({"cells": {}}, value, serial_of(y, m, d))
        shown = shown_of(v, None, date_result)
        return json_value(v), shown, csv_field(shown), None
    shown = show(value, sets)
    if kind == "date":
        jv = {"date": date_text(int(value), "yyyy-mm-dd")}
    elif kind == "time":
        jv = {"time": time_text(split_day_time(value)[1], "hh:mm:ss")}
    elif kind == "datetime":
        days, secs = split_day_time(value)
        jv = {"datetime": date_text(days, "yyyy-mm-dd") + "T" + time_text(secs, "hh:mm:ss")}
    elif kind == "text" and value[:1] in ("=", "'"):
        jv = "'" + value                  # a read returns such text with one more apostrophe
    else:
        jv = value
    return jv, shown, csv_field(shown), sets


def agree_15(want, got):
    """Numbers agree after rounding to 15 significant digits (section 9.6)."""
    return dec15(float(want)) == dec15(float(got))


def same_value(want, got):
    if isinstance(want, bool) or isinstance(got, bool):
        return isinstance(want, bool) and isinstance(got, bool) and want == got
    if isinstance(want, (int, float)) and isinstance(got, (int, float)):
        return agree_15(want, got)        # the comparison of the contract (section 9.6)
    return type(want) == type(got) and want == got


# ---------------------------------------------------------------- files

def check_format(name, lineno, line, row, problems, ids):
    where = "%s.jsonl:%d" % (name, lineno)
    for ch in line:
        if ord(ch) in BANNED:
            problems.append("%s: %s (U+%04X)" % (where, BANNED[ord(ch)], ord(ch)))
            break
    typed = name == "typed"
    want = KEYS_TYPED if typed else [k for k in KEYS if k in row or k not in OPTIONAL_KEYS]
    if list(row) != want:
        problems.append("%s: keys are %s, expected %s" % (where, list(row), want))
        return False
    rid, fn = row["id"], row["fn"]
    if not (isinstance(fn, str) and rid == "%s-%s" % (fn.lower(), rid[-3:]) and rid[-3:].isdigit()):
        problems.append("%s: id %r is not the function name in lower case, a hyphen and three digits" % (where, rid))
    if rid in ids:
        problems.append("%s: id %r is used twice" % (where, rid))
    ids.add(rid)
    if name in GROUPS:
        if fn not in GROUPS[name]:
            problems.append("%s: function %r does not belong to the group %s" % (where, fn, name))
    elif fn not in OTHER_FN.get(name, ()):
        problems.append("%s: fn %r is not expected in this file" % (where, fn))
    if row["class"] not in CLASSES:
        problems.append("%s: class %r" % (where, row["class"]))
    if "differs" in row and row["differs"] not in DIFFERS:
        problems.append("%s: differs %r" % (where, row["differs"]))
    if typed:
        if not isinstance(row["typed"], str):
            problems.append("%s: typed is no text" % where)
        if not (row["sets"] is None or isinstance(row["sets"], dict)):
            problems.append("%s: sets is neither a format object nor null" % where)
    else:
        if not re.match(r"^\d{4}-\d{2}-\d{2}$", row["date"]):
            problems.append("%s: date %r" % (where, row["date"]))
        if not isinstance(row["cells"], dict) or not isinstance(row.get("tabs", {}), dict):
            problems.append("%s: cells or tabs is no object" % where)
        if "at" in row and not re.match(r"^[A-Z]{1,2}\d+$", row["at"]):
            problems.append("%s: at %r is no address" % (where, row["at"]))
        if not (isinstance(row["formula"], str) and row["formula"].startswith("=")):
            problems.append("%s: formula does not start with =" % where)
    for k in ("shown", "csv", "note"):
        if not isinstance(row[k], str):
            problems.append("%s: %s is no text" % (where, k))
    if not row["note"]:
        problems.append("%s: empty note" % where)
    return True


def main():
    # Rows hold text of many scripts, and a report of a mismatch prints it.
    # Output that goes to a pipe takes the encoding of the locale, which on
    # Windows is no UTF-8 and cannot hold that text.
    for stream in (sys.stdout, sys.stderr):
        if hasattr(stream, "reconfigure"):
            stream.reconfigure(encoding="utf-8", errors="backslashreplace")
    problems, mismatches, unchecked, remarks = [], [], [], []
    ids, per_fn, per_file, classes = set(), {}, {}, {}
    checked = total = 0
    with open(os.path.abspath(__file__), encoding="utf-8") as f:
        for ch in f.read():
            if ord(ch) in BANNED:
                problems.append("check_reference.py holds a %s" % BANNED[ord(ch)])
                break
    names = sorted(os.path.basename(p)[:-6] for p in glob.glob(os.path.join(HERE, "*.jsonl")))
    for expected in list(GROUPS) + ["operators", "typed"]:
        if expected not in names:
            problems.append("file %s.jsonl is missing" % expected)
    for name in names:
        if name not in GROUPS and name not in OTHER_FN:
            problems.append("unexpected file %s.jsonl" % name)
        raw = open(os.path.join(HERE, name + ".jsonl"), "rb").read()
        try:
            content = raw.decode("utf-8")
        except UnicodeDecodeError as e:
            problems.append("%s.jsonl is not UTF-8: %s" % (name, e))
            continue
        if raw.startswith(b"\xef\xbb\xbf") or "\r\n" in content or not content.endswith("\n"):
            problems.append("%s.jsonl: needs no byte order mark, LF line ends and a final line end" % name)
        for lineno, line in enumerate(content.split("\n")[:-1], 1):
            total += 1
            per_file[name] = per_file.get(name, 0) + 1
            try:
                row = json.loads(line, object_pairs_hook=lambda pairs: dict(pairs))
            except ValueError as e:
                problems.append("%s.jsonl:%d: not JSON: %s" % (name, lineno, e))
                continue
            if not check_format(name, lineno, line, row, problems, ids):
                continue
            per_fn[row["fn"]] = per_fn.get(row["fn"], 0) + 1
            classes.setdefault(row["fn"], set()).add(row["class"])
            try:
                value, shown, csv, sets = compute(row)
            except Unsupported as e:
                unchecked.append((row["id"], str(e)))
                continue
            except RecursionError:
                unchecked.append((row["id"], "the formula nests deeper than the checker follows"))
                continue
            checked += 1
            diffs = []
            if not same_value(row["value"], value):
                diffs.append("value: table %r, checker %r" % (row["value"], value))
            if row["shown"] != shown:
                diffs.append("shown: table %r, checker %r" % (row["shown"], shown))
            if row["csv"] != csv:
                diffs.append("csv: table %r, checker %r" % (row["csv"], csv))
            if name == "typed" and row["sets"] != sets:
                diffs.append("sets: table %r, checker %r" % (row["sets"], sets))
            if diffs:
                mismatches.append((row["id"], row.get("formula", row.get("typed")), diffs))
    for fn in sorted(FN_FILE):
        if per_fn.get(fn, 0) < MIN_ROWS_PER_FUNCTION:
            problems.append("function %s has %d rows, fewer than %d"
                            % (fn, per_fn.get(fn, 0), MIN_ROWS_PER_FUNCTION))
        for cls in ("normal", "error"):
            if cls not in classes.get(fn, ()):
                problems.append("function %s has no %s case" % (fn, cls))
    for cls in ("boundary", "coercion"):
        without = [fn for fn in sorted(FN_FILE) if cls not in classes.get(fn, ())]
        if without:
            remarks.append("functions without a %s case: %s" % (cls, ", ".join(without)))

    print("Sheet function reference table check")
    print("  Python %s, Unicode %s" % (sys.version.split()[0], unicodedata.unidata_version))
    for name in names:
        print("  %-20s %5d rows" % (name + ".jsonl", per_file.get(name, 0)))
    print("  rows total            %5d" % total)
    print("  rows cross-checked    %5d" % checked)
    print("  rows in agreement     %5d" % (checked - len(mismatches)))
    print("  rows not cross-checked %4d" % len(unchecked))
    print("  functions of rule 12 with %d rows or more: %d of %d" %
          (MIN_ROWS_PER_FUNCTION,
           sum(1 for fn in FN_FILE if per_fn.get(fn, 0) >= MIN_ROWS_PER_FUNCTION), len(FN_FILE)))
    for remark in remarks:
        print("  note: " + remark)
    for rid, reason in unchecked:
        print("  not checked: %s: %s" % (rid, reason))
    for p in problems:
        print("  PROBLEM: " + p)
    for rid, formula, diffs in mismatches:
        print("  MISMATCH %s  %s" % (rid, formula))
        for d in diffs:
            print("      " + d)
    ok = not problems and not mismatches and not unchecked
    print("RESULT: " + ("OK" if ok else "FAILED (%d mismatches, %d rows not checked, %d problems)"
                        % (len(mismatches), len(unchecked), len(problems))))
    return 0 if ok else 1


if __name__ == "__main__":
    sys.setrecursionlimit(10000)
    sys.exit(main())
