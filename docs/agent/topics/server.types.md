## Server flat types

`flats-runtime-v1.d.ts` below is the typed contract of runtime API v1 for
JavaScript server flats: the default export, the request and response
objects, `env.DB`, `env.FILES`, ordinary variables and secrets, outbound
`fetch`, and the other globals the runtime provides. Where prose topics and
these declarations disagree, the declarations win. Host tests check them
against the real runtime. A comment at the top lists globals that are absent.

How to use it:

- Save the block as `flats-runtime-v1.d.ts` in your project. Type-check
  with `"lib": ["ES2022"]` and `"types": []`, without the DOM library. Its
  Request, Response, Headers, URL and fetch are smaller than the browser ones.
- In JavaScript, add `// @ts-check` and `/** @type {Flats.ServerModule} */`
  above `export default {...}`. In TypeScript, write
  `export default {...} satisfies Flats.ServerModule`.
- Declare your environment names with `Flats.Env<"API_KEY" | "MODE">`;
  each is `string | undefined`.
- Upload only compiled JavaScript ES modules. The declaration file itself is
  not needed on the host.

Behavior and limits are explained in `topic.server`, `topic.server.db`,
`topic.server.files`, `topic.server.fetch` and `topic.server.limits`.
