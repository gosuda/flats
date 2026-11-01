package notebook

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type ContentKind string

const (
	Paragraph      ContentKind = "paragraph"
	Heading1       ContentKind = "heading1"
	Heading2       ContentKind = "heading2"
	Heading3       ContentKind = "heading3"
	Bullet         ContentKind = "bullet"
	Numbered       ContentKind = "numbered"
	Todo           ContentKind = "todo"
	Toggle         ContentKind = "toggle"
	Quote          ContentKind = "quote"
	Callout        ContentKind = "callout"
	Code           ContentKind = "code"
	Divider        ContentKind = "divider"
	Image          ContentKind = "image"
	PageLink       ContentKind = "page_link"
	InlineDatabase ContentKind = "database"
)

func (k ContentKind) IsText() bool {
	switch k {
	case Paragraph, Heading1, Heading2, Heading3, Bullet, Numbered, Todo, Toggle, Quote, Callout:
		return true
	}
	return false
}
func (k ContentKind) HoldsChildren() bool {
	return k == Bullet || k == Numbered || k == Todo || k == Toggle
}

type Mark string

const (
	Bold       Mark = "bold"
	Italic     Mark = "italic"
	Strike     Mark = "strike"
	InlineCode Mark = "code"
)

var markOrder = []Mark{Bold, Italic, Strike, InlineCode}

type Link struct {
	URL  string `json:"url,omitempty"`
	Page ID     `json:"page,omitempty"`
}

func (l Link) Validate() error {
	if (l.URL == "") == (l.Page == "") {
		return fmt.Errorf("link needs exactly one url or page")
	}
	if l.Page != "" {
		return l.Page.ValidateKind(PageKind)
	}
	return validateURL(l.URL, true)
}
func (l Link) MarshalJSON() ([]byte, error) {
	if err := l.Validate(); err != nil {
		return nil, err
	}
	type wire Link
	return json.Marshal(wire(l))
}
func (l *Link) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := strictDecode(data, &fields); err != nil {
		return err
	}
	if len(fields) != 1 {
		return fmt.Errorf("link needs exactly one field")
	}
	type wire Link
	var v wire
	if err := strictDecode(data, &v); err != nil {
		return err
	}
	if err := Link(v).Validate(); err != nil {
		return err
	}
	*l = Link(v)
	return nil
}

type Run struct {
	Text  string `json:"text"`
	Marks []Mark `json:"marks"`
	Link  *Link  `json:"link,omitempty"`
}

func (r *Run) UnmarshalJSON(data []byte) error {
	if err := requiredFields(data, "text", "marks"); err != nil {
		return err
	}
	type wire Run
	var v wire
	if err := strictDecode(data, &v); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := strictDecode(data, &fields); err != nil {
		return err
	}
	if raw, ok := fields["link"]; ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("link cannot be null")
	}
	*r = Run(v)
	return nil
}

type RichText []Run

func CanonicalizeRichText(text RichText) (RichText, error) {
	result := make(RichText, 0, len(text))
	for _, run := range text {
		if !utf8.ValidString(run.Text) {
			return nil, fmt.Errorf("invalid rich text UTF-8")
		}
		marks := map[Mark]bool{}
		for _, m := range run.Marks {
			switch m {
			case Bold, Italic, Strike, InlineCode:
				marks[m] = true
			default:
				return nil, fmt.Errorf("unknown mark %q", m)
			}
		}
		run.Marks = make([]Mark, 0, len(marks))
		for _, m := range markOrder {
			if marks[m] {
				run.Marks = append(run.Marks, m)
			}
		}
		if run.Link != nil {
			if err := run.Link.Validate(); err != nil {
				return nil, err
			}
			copy := *run.Link
			run.Link = &copy
		}
		if run.Text == "" {
			continue
		}
		if len(result) > 0 && reflect.DeepEqual(result[len(result)-1].Marks, run.Marks) && reflect.DeepEqual(result[len(result)-1].Link, run.Link) {
			result[len(result)-1].Text += run.Text
		} else {
			result = append(result, run)
		}
	}
	return result, nil
}
func (text RichText) MarshalJSON() ([]byte, error) {
	canonical, err := CanonicalizeRichText(text)
	if err != nil {
		return nil, err
	}
	return json.Marshal([]Run(canonical))
}
func (text *RichText) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("rich text must be an array")
	}
	var runs []Run
	if err := strictDecode(data, &runs); err != nil {
		return err
	}
	canonical, err := CanonicalizeRichText(runs)
	if err != nil {
		return err
	}
	*text = canonical
	return nil
}

type Tone string

const (
	NeutralTone Tone = "neutral"
	InfoTone    Tone = "info"
	SuccessTone Tone = "success"
	WarningTone Tone = "warning"
	DangerTone  Tone = "danger"
)

type Content struct {
	Kind        ContentKind `json:"-"`
	Text        RichText    `json:"-"`
	Checked     bool        `json:"-"`
	OpenDefault bool        `json:"-"`
	Tone        Tone        `json:"-"`
	Code        string      `json:"-"`
	Language    string      `json:"-"`
	Asset       string      `json:"-"`
	Alt         string      `json:"-"`
	Caption     RichText    `json:"-"`
	Target      ID          `json:"-"`
	Database    ID          `json:"-"`
}

func DecodeContent(data []byte) (Content, error) {
	var c Content
	err := c.UnmarshalJSON(data)
	return c, err
}
func (c Content) Validate() error {
	if !c.Kind.IsText() && c.Text != nil {
		return fmt.Errorf("text runs are invalid for %s", c.Kind)
	}
	if c.Kind != Todo && c.Checked {
		return fmt.Errorf("checked is invalid for %s", c.Kind)
	}
	if c.Kind != Toggle && c.OpenDefault {
		return fmt.Errorf("open_default is invalid for %s", c.Kind)
	}
	if c.Kind != Callout && c.Tone != "" {
		return fmt.Errorf("tone is invalid for %s", c.Kind)
	}
	if c.Kind != Code && (c.Code != "" || c.Language != "") {
		return fmt.Errorf("code fields are invalid for %s", c.Kind)
	}
	if c.Kind != Image && (c.Asset != "" || c.Alt != "" || c.Caption != nil) {
		return fmt.Errorf("image fields are invalid for %s", c.Kind)
	}
	if c.Kind != PageLink && c.Target != "" {
		return fmt.Errorf("target is invalid for %s", c.Kind)
	}
	if c.Kind != InlineDatabase && c.Database != "" {
		return fmt.Errorf("database is invalid for %s", c.Kind)
	}
	if c.Kind.IsText() {
		if _, err := CanonicalizeRichText(c.Text); err != nil {
			return err
		}
		if c.Kind == Callout {
			switch c.Tone {
			case NeutralTone, InfoTone, SuccessTone, WarningTone, DangerTone:
			default:
				return fmt.Errorf("invalid callout tone %q", c.Tone)
			}
		}
		return nil
	}
	switch c.Kind {
	case Code:
		if err := validateText(c.Code); err != nil {
			return err
		}
		return validateText(c.Language)
	case Divider:
		return nil
	case Image:
		if err := validateAsset(c.Asset); err != nil {
			return err
		}
		if err := validateText(c.Alt); err != nil {
			return err
		}
		_, err := CanonicalizeRichText(c.Caption)
		return err
	case PageLink:
		return c.Target.ValidateKind(PageKind)
	case InlineDatabase:
		return c.Database.ValidateKind(DatabaseKind)
	default:
		return fmt.Errorf("unknown content kind %q", c.Kind)
	}
}
func (c Content) MarshalJSON() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	v := map[string]any{"kind": c.Kind}
	if c.Kind.IsText() {
		v["text"] = c.Text
		switch c.Kind {
		case Todo:
			v["checked"] = c.Checked
		case Toggle:
			v["open_default"] = c.OpenDefault
		case Callout:
			v["tone"] = c.Tone
		}
	} else {
		switch c.Kind {
		case Code:
			v["text"] = c.Code
			v["language"] = c.Language
		case Image:
			v["asset"] = c.Asset
			v["alt"] = c.Alt
			v["caption"] = c.Caption
		case PageLink:
			v["target"] = c.Target
		case InlineDatabase:
			v["database"] = c.Database
		}
	}
	return json.Marshal(v)
}
func (c *Content) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := strictDecode(data, &fields); err != nil {
		return err
	}
	if err := requiredFields(data, "kind"); err != nil {
		return err
	}
	var kind ContentKind
	if err := strictDecode(fields["kind"], &kind); err != nil {
		return err
	}
	allowed := []string{"kind"}
	required := []string{"kind"}
	v := Content{Kind: kind}
	decode := func(name string, out any) error { return strictDecode(fields[name], out) }
	if kind.IsText() {
		allowed = append(allowed, "text")
		required = append(required, "text")
		switch kind {
		case Todo:
			allowed = append(allowed, "checked")
		case Toggle:
			allowed = append(allowed, "open_default")
		case Callout:
			allowed = append(allowed, "tone")
			v.Tone = NeutralTone
		}
	} else {
		switch kind {
		case Code:
			allowed = append(allowed, "text", "language")
			required = append(required, "text", "language")
		case Divider:
		case Image:
			allowed = append(allowed, "asset", "alt", "caption")
			required = append(required, "asset", "alt", "caption")
		case PageLink:
			allowed = append(allowed, "target")
			required = append(required, "target")
		case InlineDatabase:
			allowed = append(allowed, "database")
			required = append(required, "database")
		default:
			return fmt.Errorf("unknown content kind %q", kind)
		}
	}
	if err := onlyFields(fields, allowed...); err != nil {
		return err
	}
	if err := requiredFields(data, required...); err != nil {
		return err
	}
	for name, raw := range fields {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("%s cannot be null", name)
		}
	}
	for _, name := range allowed[1:] {
		if _, ok := fields[name]; !ok {
			continue
		}
		var err error
		switch name {
		case "text":
			if kind == Code {
				err = decode(name, &v.Code)
			} else {
				err = decode(name, &v.Text)
			}
		case "checked":
			err = decode(name, &v.Checked)
		case "open_default":
			err = decode(name, &v.OpenDefault)
		case "tone":
			err = decode(name, &v.Tone)
		case "language":
			err = decode(name, &v.Language)
		case "asset":
			err = decode(name, &v.Asset)
		case "alt":
			err = decode(name, &v.Alt)
		case "caption":
			err = decode(name, &v.Caption)
		case "target":
			err = decode(name, &v.Target)
		case "database":
			err = decode(name, &v.Database)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if err := v.Validate(); err != nil {
		return err
	}
	*c = v
	return nil
}
func onlyFields(fields map[string]json.RawMessage, allowed ...string) error {
	for name := range fields {
		found := false
		for _, a := range allowed {
			if a == name {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("unknown field %q", name)
		}
	}
	return nil
}
func validateText(s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("invalid text UTF-8")
	}
	return nil
}
func validateAsset(s string) error {
	if len(s) != 64 || strings.ToLower(s) != s {
		return fmt.Errorf("invalid asset hash")
	}
	_, err := hex.DecodeString(s)
	return err
}
func validateURL(s string, mailto bool) error {
	if !utf8.ValidString(s) || strings.ContainsAny(s, "\\") {
		return fmt.Errorf("invalid URL")
	}
	for _, c := range s {
		if unicode.IsSpace(c) || unicode.IsControl(c) {
			return fmt.Errorf("invalid URL whitespace")
		}
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		if u.Host == "" || u.Hostname() == "" || u.Opaque != "" {
			return fmt.Errorf("URL needs a host")
		}
		if strings.Contains(u.Hostname(), ":") && net.ParseIP(strings.Split(u.Hostname(), "%")[0]) == nil {
			return fmt.Errorf("invalid IPv6 host")
		}
		if port := u.Port(); port != "" {
			if _, err := strconv.ParseUint(port, 10, 16); err != nil {
				return fmt.Errorf("invalid URL port")
			}
		}
		if strings.HasSuffix(u.Host, ":") {
			return fmt.Errorf("empty URL port")
		}
		return nil
	}
	if mailto && u.Scheme == "mailto" && u.Host == "" && (u.Opaque != "" || u.RawQuery != "") {
		if u.Opaque != "" && !strings.Contains(u.Opaque, "@") {
			return fmt.Errorf("invalid mailto address")
		}
		return nil
	}
	return fmt.Errorf("unsupported URL scheme")
}
