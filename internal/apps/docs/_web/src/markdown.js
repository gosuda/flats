// The Markdown renderer and its link rules, shared by the rendered view and
// the live-preview editor so both agree on which links exist and where they
// lead.
import MarkdownIt from "markdown-it";

export function createMarkdown() {
  const md = new MarkdownIt({ html: false, linkify: true, typographer: false }),
    defaultValidate = md.validateLink,
    unsafeScheme = /^\s*(?:javascript|vbscript|file|data):/i;
  md.validateLink = (link) => defaultValidate(link) && !unsafeScheme.test(link);
  return md;
}

// linkRules answers the editor's link questions the way md renders them.
// destination maps a decoded link or image destination to the normalized URL
// md would link to, or "" when md leaves the syntax as text.
export function linkRules(md) {
  return {
    destination(href) {
      const normalized = md.normalizeLink(href);
      return md.validateLink(normalized) ? normalized : "";
    },
    // label normalizes a reference label as the renderer matches it.
    label(text) {
      return md.utils.normalizeReference(text);
    },
    // plainText is a Markdown label as the renderer writes it into alt.
    plainText(label) {
      const tokens = md.parseInline(label, {})[0]?.children || [];
      return md.renderer.renderInlineAsText(tokens, md.options, {});
    },
    // bare takes the line from a bare address onward and the length the
    // editor's parser gave the address. It returns the link markdown-it
    // makes there, {href, length}, or null when it leaves the text plain.
    bare(rest, length) {
      // A scheme link may run past the parser's end, e.g. a query string.
      const atStart = md.linkify.matchAtStart(rest);
      if (atStart) {
        const href = this.destination(atStart.url);
        return href ? { href, length: atStart.lastIndex } : null;
      }
      const text = rest.slice(0, length),
        m = md.linkify.match(text);
      if (!m || m.length !== 1 || m[0].index !== 0 || m[0].lastIndex !== length)
        return null;
      const href = this.destination(m[0].url);
      return href ? { href, length } : null;
    },
  };
}
