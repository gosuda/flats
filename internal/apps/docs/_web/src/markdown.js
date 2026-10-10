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
// md would link to, or "" when md leaves the syntax as text. bare maps a bare
// address to its link URL, or "" when md does not linkify it.
export function linkRules(md) {
  return {
    destination(href) {
      const normalized = md.normalizeLink(href);
      return md.validateLink(normalized) ? normalized : "";
    },
    // plainText is a Markdown label as the renderer writes it into alt.
    plainText(label) {
      const tokens = md.parseInline(label, {})[0]?.children || [];
      return md.renderer.renderInlineAsText(tokens, md.options, {});
    },
    bare(text) {
      const m = md.linkify.match(text);
      if (
        !m ||
        m.length !== 1 ||
        m[0].index !== 0 ||
        m[0].lastIndex !== text.length
      )
        return "";
      return this.destination(m[0].url);
    },
  };
}
