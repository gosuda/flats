package config

import (
	"errors"
	"net"
	"regexp"
	"strings"
)

// ProxyTemplateKey is the address pattern a reverse proxy serves flats under.
const ProxyTemplateKey = "proxy.url_template"

const (
	templateHost = "{host}"
	// maxRouteLabel is the longest route host: a preview of a 54-character
	// slug, which fills one DNS label.
	maxRouteLabel = 63
	// maxTemplateSuffix keeps <label>.<suffix> within a 253-character DNS name.
	maxTemplateSuffix = 253 - maxRouteLabel - 1
	// templateLabel stands in for {host} while the URL is validated.
	templateLabel = "x-flats-route-label"
)

// routeLabelRE is one label in the slug character set: lowercase letters,
// digits and single hyphens.
var routeLabelRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Template is a parsed proxy.url_template. The zero Template matches no host
// and builds no address.
type Template struct {
	scheme string // "http" or "https"
	suffix string // the DNS name after "{host}."
	port   string // "" when the template has none
}

// ParseTemplate validates a proxy.url_template value such as
// https://{host}.site.example.org: an http or https URL whose host is {host},
// a dot and a lowercase DNS name of at most 189 characters, with an optional
// port other than the scheme's default and nothing else.
func ParseTemplate(s string) (Template, error) {
	const hint = "use http:// or https:// followed by {host}. and a lowercase DNS name, such as https://{host}.site.example.org"
	scheme, rest, ok := strings.Cut(s, "://")
	if !ok || (scheme != "http" && scheme != "https") ||
		!strings.HasPrefix(rest, templateHost+".") || strings.Count(s, templateHost) != 1 {
		return Template{}, fieldErr(ProxyTemplateKey, hint, "%q must start with http://{host}. or https://{host}.", s)
	}
	_, host, port, err := checkOriginURL(strings.Replace(s, templateHost, templateLabel, 1), ProxyTemplateKey)
	if err != nil {
		var fe *FieldError
		if errors.As(err, &fe) {
			fe.Msg = strings.ReplaceAll(fe.Msg, templateLabel, templateHost)
		}
		return Template{}, err
	}
	suffix := strings.TrimPrefix(host, templateLabel+".")
	if len(suffix) > maxTemplateSuffix {
		return Template{}, fieldErr(ProxyTemplateKey, "use a shorter domain",
			"the domain after {host}. is %d characters; at most %d leave room for a %d-character flat address",
			len(suffix), maxTemplateSuffix, maxRouteLabel)
	}
	if port == defaultPortOf(scheme) {
		return Template{}, fieldErr(ProxyTemplateKey, "omit the port; browsers leave it out of the address",
			"port %s is the default for %s", port, scheme)
	}
	return Template{scheme: scheme, suffix: suffix, port: port}, nil
}

// checkURLTemplate accepts "" (no template) or a valid template.
func checkURLTemplate(v any) (any, error) {
	s := v.(string)
	if s == "" {
		return s, nil
	}
	if _, err := ParseTemplate(s); err != nil {
		return nil, err
	}
	return s, nil
}

// URL returns the address of route host label, such as
// https://blog.site.example.org.
func (t Template) URL(label string) string {
	if t.suffix == "" {
		return ""
	}
	return t.scheme + "://" + t.Authority(label)
}

// Authority returns label, the template's domain and its port, if any.
func (t Template) Authority(label string) string {
	if t.suffix == "" {
		return ""
	}
	a := label + "." + t.suffix
	if t.port != "" {
		a += ":" + t.port
	}
	return a
}

// String returns the template as proxy.url_template spells it, or "" for
// the zero Template.
func (t Template) String() string { return t.URL(templateHost) }

// Scheme is "http" or "https", or "" for the zero Template.
func (t Template) Scheme() string { return t.scheme }

// Label returns the route host label of a request Host. It ignores case, a
// port and a trailing dot, and requires the template's domain after exactly
// one label of lowercase letters, digits and single hyphens.
func (t Template) Label(host string) (string, bool) {
	if t.suffix == "" {
		return "", false
	}
	host = strings.ToLower(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")
	label, ok := strings.CutSuffix(host, "."+t.suffix)
	if !ok || len(label) > maxRouteLabel || !routeLabelRE.MatchString(label) {
		return "", false
	}
	return label, true
}

// produces reports whether host is one DNS label followed by the template's
// domain, an address the template can produce.
func (t Template) produces(host string) bool {
	label, ok := strings.CutSuffix(host, "."+t.suffix)
	return t.suffix != "" && ok && label != "" && !strings.Contains(label, ".")
}
