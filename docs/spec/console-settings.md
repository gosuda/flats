# Operator keys and console domains

Technical design: [console-settings-TECH.md](console-settings-TECH.md).

## Summary

The operator manages the keys Flats needs and the console's Tailscale
domains from the console's Settings, while the host runs. The same can be
done without the console, which matters on a remote server whose console is
not reachable until it has a key and a domain.

- **Keys.** Every key the operator enters (the Tailscale auth key, the
  NetBird setup key, the zrok account token) follows one model: it can be
  entered on the command line, in the environment or in the console, and the
  keys Flats keeps are stored in one place, `flats.db`.
- **Console domains.** The console's Tailscale names are a list the operator
  adds to and deletes from. There is no single "console domain" to edit: to
  change it, add the new one, move clients over, then delete the old one.
  Several may be used at once.

NetBird console domains and the NetBird setup key follow
[netbird-peers.md](netbird-peers.md), which builds on the rules here. Console
authentication is out of scope; see [Open questions](#open-questions).

## Keys

### Which keys

| Key | Use | Kept by Flats |
|---|---|---|
| Tailscale auth key | logs in every new tailnet node (flats, previews, console domains) | yes, in `flats.db` |
| NetBird setup key | registers every new NetBird peer (one per flat and console domain) | yes, in `flats.db` |
| zrok account token | registers this machine as a zrok environment | yes, inside the zrok environment directory (exception below) |

Identities that Flats or a provider creates itself, such as the node state in
`tsnet/`, `portal/` and `netbird/` and the zrok identity, are provider state,
not keys, and stay where they are.

### Rules

1. **One store.** A key Flats keeps is stored in `flats.db`, encrypted with
   `secret.key` as flat secrets are. It is never written to `config.json`,
   logs, events or any API response. `config.json` holds no key and no path to
   a key file.
2. **zrok exception.** The zrok client reads its environment directory, and
   that directory holds the account token. The zrok token therefore stays in
   `zrok-environment/` (mode 0600) as now. Only its input follows these rules.
3. **Inputs.** A key reaches Flats in one of three ways, in this order of
   priority when the host starts:
   1. a `flats serve` flag naming a file that holds the key (never the key
      itself on the command line, where `ps` and service files would show it);
   2. a Flats environment variable holding the key, or one naming a file that
      holds it. Flats removes the variable from its own environment after
      reading it, so child processes never inherit it;
   3. the key already stored in `flats.db`.

   The console can also register a key while the host runs. `TS_*` variables
   stay refused: the embedded Tailscale client would read them and bypass
   Flats.
4. **External values seed; they do not override.** At start, a flag or
   environment value is stored and used only when `flats.db` holds no key of
   that kind. This is the behavior of Tailscale's own `TS_AUTH_ONCE=true` and
   of seed settings such as Grafana's initial admin password.
5. **Runtime registration wins.** A key registered in the console is stored
   and used at once, without a restart, and stays after restarts even while
   the service still passes an older flag or variable. New tailnet nodes use
   the new Tailscale key; nodes already logged in are not affected.
6. **A differing external value is reported, not applied.** When a flag or
   environment value differs from the stored key, the host keeps the stored
   key and warns in the log and in the console. Nothing about the external
   value is stored.
7. **Enrollment keys.** For a key that creates an account link (the zrok
   token), a flag or environment value means "connect if not yet connected".
   It never replaces a connected account; the console's replace and disconnect
   actions do. The NetBird setup key is stored like the Tailscale key
   (rules 4–6; netbird-peers.md).
8. **Removing a key.** The console can remove the stored Tailscale key; new
   nodes then wait for an interactive login and show its link. It can remove
   the NetBird setup key; running peers keep working and new ones cannot
   register. If a flag or environment value is still present, the next start
   seeds it again, and the console says so. zrok keeps its disconnect action.
9. **What the operator sees.** For each key: whether one is set (for zrok:
   whether an account is connected, or a connection is waiting), where it
   came from (flag, environment, offline CLI, console, or unknown for a
   connection made before this feature), when, and non-secret identity where
   one exists (zrok environment ID, NetBird management server and running
   peers). A key is never shown again.
10. **Agents.** No MCP tool and no agent API route reads, sets or removes a
    key; keys are set only from the console, the offline CLI or the host's own
    start inputs. The console's routes are protected against cross-site
    browser requests, not authenticated: a program that can reach the
    management listener and imitate the console page could still call them,
    as it can call approvals today. Closing that gap is part of the deferred
    console authentication.
11. **Check on entry.** A Tailscale key must be an auth key (`tskey-auth-…`);
    other kinds, such as OAuth client secrets and API keys, are refused with an
    explanation. Whether it is reusable and untagged cannot be told from the
    key: that stays the operator's responsibility, and the console says so. A
    key is not otherwise verified until a node uses it.

### Containers

The image keeps working from mounted secret files: pass the key file as the
flag, or name it in the `_FILE` environment variable. Because external values
seed, a secret rotated by an orchestrator is not picked up automatically;
register the new key in the console. An option to apply the external value on
every start may be added later.

## Console domains

12. **A list.** The console's Tailscale names are an ordered list in
    `config.json`, in the order they were added. Each name is a DNS label and
    becomes its own tailnet node with its own MagicDNS name and HTTPS
    certificate.
13. **Independent of the private backend.** A non-empty list runs the console
    on Tailscale. `network.private_backend` only chooses the default network
    for new flats. Adding a domain requires the Tailscale provider to be on,
    and the provider cannot be turned off while the list is not empty.
14. **Add.** Adding a name starts its node at once, without a restart. A name
    is refused when it equals an existing flat slug, a preview host, a name
    kept by a rename redirect, another console domain or a node still being
    retired. New flats, renames and previews can never take a console
    domain's name, including one whose addition is still in progress. The
    name `flats` stays reserved for the console even when it is not listed. A name
    added outside the console (offline `flats config`, a file edit) that
    collides with any of these is not served; the console shows the conflict. When the node
    needs a login (no Tailscale key), the console shows the login link. The
    console shows the MagicDNS name the tailnet actually assigned and warns
    when it differs from the requested name (for example `flats-1`).
15. **Delete.** Deleting a name removes it from the list first, then logs its
    node out and retires it as other tailnet nodes are retired. A name removed
    outside the console is retired at the next start. A retirement that fails
    is retried later; the name cannot be added again until it finishes.
    Tailscale keeps a logged-out machine listed, so reusing its name needs
    that machine removed in the Tailscale admin console; otherwise the new
    node gets another name and the console warns (rule 14).
    - A console reached over Tailscale refuses to delete the last name.
      Emptying the list, which leaves the console on the loopback address
      only, is possible only from the loopback console or with offline
      `flats config`.
    - Deleting the name the console is open on asks for confirmation and
      lists the names that remain; the response sends the browser to one of
      them before that node is retired.
    - The console warns when approvals are pending, because their links name
      a domain.
16. **Links.** Approval links and the `console_url` and `mcp_url` in status
    use the console domain the request came in on. A request on any other
    address (loopback, or an unknown host) gets the first name in the list;
    with an empty list, the loopback address. Status also lists every console
    URL. There is no "main domain" setting.

## First install without the console

On a remote server the console is reachable only after the host is on the
tailnet. Everything above can therefore also be done before the first start:

- any key with the offline `flats key set` command (it refuses while a host
  runs), or through the `flats serve` flag or environment variable;
- turning the Tailscale provider on and adding console domains with offline
  `flats config set` (which refuses while a host runs);
- without a key, each node prints its login link to the log, as now.

After the first start the operator continues in the console.

## Upgrading

This feature bumps `schema_version` to 2. The migration replaces
`host.console_host` with a list that holds the old name when
`private_backend` was `tailscale` and the name is a valid lowercase DNS label,
and is empty otherwise (a name that was not a valid label never served a
console), so existing hosts behave as before. The first start then stores the
key named by `credentials.tailscale_authkey_file` in `flats.db`, leaving the
file alone and stopping with status 78 when it cannot be read, and removes
that path from `config.json`.

## Open questions

- Changing keys from the command line while a host runs. Offline commands
  refuse while it runs, and the agent-facing API must not carry keys; a local
  channel protected by OS permissions would be needed. Deferred: first
  installs use flags or the environment, and a running host uses the console.
- Console authentication and authorization.
