"""Additional actual-binary lifecycle populations; no product interfaces or mocks."""
import hashlib
import json
import re
from pathlib import Path
import sqlite3


def helpers():
    # Lazy import avoids a gate/populations import cycle when gate is executable.
    from gate import Host, archive, require, server, static
    return Host, archive, require, server, static


def durable_server(version):
    _, _, _, server, _ = helpers()
    files = server(version)
    files['server.js'] = files['server.js'].replace(
        'return Response.json({version: VERSION, hits:c});'.replace('VERSION', str(version)),
        'if (path === "/put") env.FILES.put("record", "DISPOSABLE-FILE");\n'
        f'return Response.json({{version:{version}, hits:c, file:env.FILES.get("record"), '
        'secret:env.GATE_SECRET === "DISPOSABLE-SECRET"});')
    return files


def seed_mixed(h):
    Host, _, require, _, static = helpers()
    require(h.legacy_binary and h.legacy_adapter_binary, 'mixed seed needs archived real binary and archived-core provider fixture')
    h.stop()
    old = Host(h.legacy_adapter_binary, h.work / 'legacy-mixed')
    try:
        old.start()
        old.save('mixed', static('UNDEPLOYED-ONE'))
        old.ok('PUT', '/console/api/flats/mixed/secrets/GATE_SECRET',
               {'value': 'DISPOSABLE-SECRET'}, console=True)
        old.save('mixed', durable_server(2))
        old.ok('POST', '/api/flats/mixed/deploy', {'version': 2})
        require(old.traffic('mixed', '/hit')['hits'] == 1, 'seed data absent')
        old.traffic('mixed', '/put')
        old.save('mixed', durable_server(3))
        old.ok('POST', '/api/flats/mixed/deploy', {'version': 3})
        live = old.traffic('mixed', '/hit')
        require(live == {'version': 3, 'hits': 2, 'file': 'DISPOSABLE-FILE', 'secret': True}, live)
        old.save('mixed', static('UNDEPLOYED-FOUR'))
        old.save('never', static('NEVER-DEPLOYED'))
        previews = [old.ok('POST', '/api/flats/mixed/previews', {'version': n}, status=201)
                    for n in (2, 4)]
        require(old.traffic(previews[0]['host'])['version'] == 2, 'deployed preview bytes')
        require('UNDEPLOYED-FOUR' in old.traffic(previews[1]['host']), 'saved preview bytes')
        pending = old.approval(old.ok('DELETE', '/api/flats/mixed', status=202))
        old.save('legacy-public', static('LEGACY-PUBLIC-CURRENT'))
        old.ok('POST', '/api/flats/legacy-public/deploy', {'version': 1})
        public = old.approval(old.ok('POST', '/api/flats/legacy-public/visibility',
                                   {'visibility': 'public-listed'}, status=202))
        require(old.decide(public)['status'] == 'approved', 'legacy Public seed failed')
        require('LEGACY-PUBLIC-CURRENT' in old.traffic('pub-legacy-public'), 'legacy Public seed not serving')
        old.save('legacy-pending', static('LEGACY-PENDING-PRIVATE'))
        old.ok('POST', '/api/flats/legacy-pending/deploy', {'version': 1})
        visibility_pending = old.approval(old.ok('POST', '/api/flats/legacy-pending/visibility',
                                                {'visibility': 'public-unlisted'}, status=202))
        versions = old.versions('mixed')
        manifest = {'versions': versions, 'live': live, 'previews': previews,
                    'deployments': old.ok('GET', '/api/flats/mixed/deployments')['deployments'],
                    'public_flat': old.flat('legacy-public'),
                    'visibility_pending': old.ok('GET', '/api/approvals/' + visibility_pending),
                    'pending': old.ok('GET', '/api/approvals/' + pending),
                    'files': {p.relative_to(old.data).as_posix(): hashlib.sha256(p.read_bytes()).hexdigest()
                              for p in old.data.glob('flats/*/versions/**/*') if p.is_file()
                              and p.relative_to(old.data).parts[1] in ('mixed', 'never')}}
        require(len(versions) == 4 and len(manifest['files']) >= 8, 'mixed seed incomplete')
        (h.work / 'mixed-seed.json').write_text(json.dumps(manifest, indent=2) + '\n')
    finally:
        old.stop()
        h.trace.extend(old.trace)
    h.data = old.data
    return manifest


def mixed_migration(h):
    _, _, require, _, _ = helpers()
    seed = seed_mixed(h)
    h.start()
    expected = {v['number']: v['hash'] for v in seed['versions'] if v['number'] in (2, 3)}
    for restart in range(2):
        require({v['number']: v['hash'] for v in h.versions('mixed')} == expected,
                'deployed identities renumbered or changed')
        require(h.flat('mixed')['live_version'] == 3, 'live identity lost')
        require(h.traffic('mixed') == seed['live'], 'live data/files/secrets changed')
        for rel, digest in seed['files'].items():
            path = h.data / rel
            if '/versions/2/' in rel or '/versions/3/' in rel:
                require(path.is_file() and hashlib.sha256(path.read_bytes()).hexdigest() == digest,
                        'deployed files moved or changed: ' + rel)
            else:
                slug = Path(rel).parts[1]
                require(any(hashlib.sha256(p.read_bytes()).hexdigest() == digest
                            for p in (h.data / 'flats' / slug / 'draft-revs').rglob('*') if p.is_file()),
                        'never-deployed file not preserved as draft: ' + rel)
        require(h.versions('never') == [] and h.flat('never')['publication'] == 'unpublished',
                'never-deployed content fabricated publication')
        row = h.ok('GET', '/api/approvals/' + seed['pending']['id'])
        require(row['id'] == seed['pending']['id'] and row['status'] in ('pending', 'failed'),
                'historical approval lost or silently executed')
        require(row['action'] == 'delete' and row['flat'] == 'mixed', 'historical reference changed')
        if row['status'] == 'failed':
            require(row.get('result'), 'safely classified approval has no failure explanation')
        if row['status'] == 'pending':
            params = row['params']
            if isinstance(params, str):
                params = json.loads(params)
            require(row['action'] == 'delete' and row['flat'] == 'mixed',
                    'legacy pending delete reference changed')
        require(h.flat('mixed')['visibility'] == 'private', 'migration exposed pending flat')
        require(h.ok('GET', '/api/flats/mixed/deployments')['deployments'] == seed['deployments'],
                'legacy deployment history changed')
        legacy_public = h.flat('legacy-public')
        require(legacy_public['live_version'] == 1 and legacy_public['publication'] == 'published',
                'legacy Public identity lost')
        require(legacy_public['visibility'] in ('private', 'public'), 'legacy visibility not normalized')
        require(h.request('GET', '/', local_host='pub-legacy-public')[0] == 404,
                'migration opened ungranted legacy Public provider')
        visibility_row = h.ok('GET', '/api/approvals/' + seed['visibility_pending']['id'])
        require(visibility_row['status'] in ('pending', 'failed') and visibility_row['flat'] == 'legacy-pending',
                'legacy visibility approval lost or automatically applied')
        active = h.ok('GET', '/api/flats/mixed/previews')['previews'] or []
        for p in seed['previews']:
            if any(q['host'] == p['host'] for q in active):
                body = h.traffic(p['host'])
                require(body['version'] == 2 if p['version'] == 2 else 'UNDEPLOYED-FOUR' in body,
                        'preserved preview points at different bytes')
            else:
                require(h.request('GET', '/', local_host=p['host'])[0] == 404,
                        'closed ephemeral preview still serves')
        h.trace.append({'kind': 'migration-preview-classification', 'restart': restart,
                        'active': active, 'historical': seed['previews']})
        if restart == 0:
            h.stop()
            h.start()
    p = h.ok('POST', '/api/flats/mixed/previews', {'target': 'draft'}, status=201)
    require('UNDEPLOYED-FOUR' in h.traffic(p['host']), 'newest historical working content lost')
    # Discriminate new publication identity from an alias of saved legacy v4.
    h.save('mixed', static('NEW-PUBLISH-DISTINCT-FROM-LEGACY-FOUR'))
    a = h.publish('mixed')
    require(h.decide(a)['status'] == 'approved', 'migrated draft publish failed')
    require(h.flat('mixed')['live_version'] == 4, 'next identity not max published + 1')
    require('NEW-PUBLISH-DISTINCT-FROM-LEGACY-FOUR' in h.traffic('mixed'), 'new publication aliased legacy saved bytes')
    for previous in seed['previews']:
        if previous['version'] == 4:
            code, body = h.request('GET', '/', local_host=previous['host'])
            require(code == 404 or (code == 200 and 'UNDEPLOYED-FOUR' in body),
                    'legacy v4 preview was stolen by new published v4')
    rollback = h.approval(h.ok('POST', '/api/flats/mixed/rollback', {'version': 2}, status=202))
    require(h.decide(rollback)['status'] == 'approved', 'rollback to migrated v2 failed')
    require(h.traffic('mixed') == {**seed['live'], 'version': 2}, 'migrated rollback changed data')
    # Decide legacy pending references after the preservation observations.
    visibility_row = h.ok('GET', '/api/approvals/' + seed['visibility_pending']['id'])
    decision = h.decide(visibility_row['id'])
    require(decision['status'] == 'failed' and decision.get('result'), 'ungranted historical visibility silently executed')
    pending_row = h.ok('GET', '/api/approvals/' + seed['pending']['id'])
    decision = h.decide(pending_row['id'])
    require(decision['status'] in ('approved', 'failed'), 'historical delete decision did not settle')
    if decision['status'] == 'approved':
        require(h.request('GET', '/api/flats/mixed')[0] == 404, 'approved migrated delete did not delete')
    else:
        require(decision.get('result') and h.flat('mixed')['live_version'] == 2,
                'refused historical delete corrupted migrated flat')


def restore_success(h):
    _, _, require, server, _ = helpers()
    h.activate('restore', server(1))
    h.traffic('restore', '/hit')
    h.activate('restore', server(2))
    require(h.traffic('restore', '/hit')['hits'] == 2, 'pre-restore data setup')
    a = h.approval(h.ok('POST', '/api/flats/restore/rollback',
                        {'version': 1, 'restore_data': True}, status=202))
    params = h.ok('GET', '/api/approvals/' + a)['params']
    if isinstance(params, str):
        params = json.loads(params)
    require(params.get('restore_data') is True, 'restore consent not frozen')
    # Data may change while waiting; approval must still restore the pre-v2 snapshot.
    require(h.traffic('restore', '/hit') == {'version': 2, 'hits': 3}, 'pending restore ran')
    require(h.decide(a)['status'] == 'approved', 'approved restore failed')
    require(h.traffic('restore') == {'version': 1, 'hits': 1}, 'frozen restore data mismatch')
    require(sorted(v['number'] for v in h.versions('restore')) == [1, 2], 'restore allocated code number')
    backup_counts = []
    names = h.ok('GET', '/console/api/flats/restore/snapshots', console=True)['snapshots'] or []
    for name in names:
        require(isinstance(name, str) and re.fullmatch(r'(?:before|failed)-[A-Za-z0-9-]+\.sqlite', name),
                'snapshot API returned an unrecognized contract filename')
        path = h.data / 'flats/restore/snapshots' / name
        if path.is_file():
            with sqlite3.connect(f'file:{path}?mode=ro', uri=True) as db:
                backup_counts.append(db.execute('SELECT count(*) FROM hits').fetchone()[0])
    require(3 in backup_counts, 'pre-restore data backup not preserved')
    h.decide(a)
    h.stop()
    h.start()
    require(h.traffic('restore') == {'version': 1, 'hits': 1}, 'retry/restart restored twice or lost data')


def startup_server(broken=False):
    # env is passed to fetch, not normally exported at module scope. Probe only
    # public JS globals; do not reach into the runtime's private host ABI.
    return {'flats.json': '{"kind":"server","health":"/health"}', 'server.js': '''
const topEnv = typeof env;
if (topEnv !== "undefined") {
  env.DB.exec("CREATE TABLE IF NOT EXISTS startup (stage TEXT)");
  env.DB.exec("INSERT INTO startup VALUES ('module')");
  env.FILES.put("startup", "module-write");
}
BROKEN
let initialized = false;
export default {fetch(request, env) {
  env.DB.exec("CREATE TABLE IF NOT EXISTS startup (stage TEXT)");
  if (!initialized) {
    initialized = true;
    env.DB.exec("INSERT INTO startup VALUES ('first-request')");
  }
  return Response.json({topEnv, stages:env.DB.query("SELECT stage FROM startup"),
                        file:env.FILES.get("startup")});
}};
'''.replace('BROKEN', 'throw new Error("GATE-MODULE-INITIALIZATION-FAILURE");' if broken else '')}


def probe_runtime(h):
    _, _, require, _, _ = helpers()
    h.save('startup', startup_server())
    h.ok('POST', '/api/flats/startup/deploy', {'version': 1})
    observed = h.traffic('startup')
    require(observed['topEnv'] in ('undefined', 'object'), 'unexpected runtime capability')
    h.trace.append({'kind': 'runtime-capability', 'observed': observed,
                    'top_level_data_supported': observed['topEnv'] == 'object',
                    'first_request_is_not_startup': True})
    h.stop()
    h.start()
    restarted = h.traffic('startup')
    require(len(restarted['stages']) > len(observed['stages']), 'instance initialization probe did not persist')


def runtime_initialization(h):
    _, _, require, _, _ = helpers()
    h.activate('startup', startup_server())
    before = h.traffic('startup')
    h.save('startup', startup_server(broken=True))
    a = h.publish('startup')
    require(h.traffic('startup') == before, 'candidate initialized before approval')
    from gate import failed, data_impact
    failure = failed(h.decide(a), 'module-init')
    after = h.traffic('startup')
    data_impact(failure, changed=before != after)
    if before['topEnv'] == 'undefined':
        require(after == before, 'unsupported top-level data probe changed live data')
    h.trace.append({'kind': 'runtime-startup-effects', 'before': before, 'after': after,
                    'top_level_data_supported': before['topEnv'] == 'object',
                    'module_scope_data_isolation': 'unproven' if before['topEnv'] == 'undefined' else 'observed'})
    # Compare impact reporting against live data immediately after successful
    # start and BEFORE the first serving request (health ran on the trial copy).
    h.save('startup', startup_server())
    a = h.publish('startup')
    dbpaths = list((h.data / 'flats/startup/data').glob('db.sqlite'))
    require(len(dbpaths) == 1, 'missing runtime data oracle')
    with sqlite3.connect(f'file:{dbpaths[0]}?mode=ro', uri=True) as db:
        before_start = db.execute('SELECT stage FROM startup').fetchall()
    success = h.decide(a)
    require(success['status'] == 'approved', 'startup candidate failed')
    with sqlite3.connect(f'file:{dbpaths[0]}?mode=ro', uri=True) as db:
        after_start = db.execute('SELECT stage FROM startup').fetchall()
    h.trace.append({'kind': 'live-start-before-serving-request',
                    'before': before_start, 'after': after_start})
    data_impact(success, changed=before_start != after_start)
    h.traffic('startup')
    h.stop()
    h.start()
    require(h.flat('startup')['live_version'] == 2, 'startup restart lost code identity')


def current_live_drift(h):
    _, _, require, _, static = helpers()
    h.activate('live-drift', static('LIVE-ONE'))
    h.activate('live-drift', static('LIVE-TWO'))
    h.save('live-drift', static('PENDING-THREE'))
    pending = h.publish('live-drift')
    rollback = h.approval(h.ok('POST', '/api/flats/live-drift/rollback', {'version': 1}, status=202))
    require(h.decide(rollback)['status'] == 'approved', 'drift setup rollback failed')
    from gate import failed
    failed(h.decide(pending), 'live-drift')
    require(h.flat('live-drift')['live_version'] == 1 and 'LIVE-ONE' in h.traffic('live-drift'),
            'stale approval changed current bytes')
    require(sorted(v['number'] for v in h.versions('live-drift')) == [1, 2], 'stale approval allocated number')
    require(h.decide(h.publish('live-drift'))['status'] == 'approved', 'fresh live policy approval poisoned')
    require('PENDING-THREE' in h.traffic('live-drift'), 'fresh live policy did not publish expected bytes')


def visibility_drift(h):
    _, _, require, _, static = helpers()
    h.ok('POST', '/__gate/host-permission', {'permitted': ['portal']})
    h.activate('visibility-drift', static('CURRENT'))
    h.ok('POST', '/console/api/flats/visibility-drift/providers',
         {'provider': 'portal', 'permitted': True}, console=True)
    h.save('visibility-drift', static('STALE-CANDIDATE'))
    pending = h.publish('visibility-drift')
    public = h.approval(h.ok('POST', '/api/flats/visibility-drift/visibility',
                            {'visibility': 'public'}, status=202))
    require(h.decide(public)['status'] == 'approved', 'visibility drift setup failed')
    from gate import failed
    failed(h.decide(pending), 'visibility-drift')
    require('CURRENT' in h.traffic('pub-visibility-drift'), 'stale visibility approval changed public bytes')
    require([v['number'] for v in h.versions('visibility-drift')] == [1], 'stale visibility allocated number')
    require(h.decide(h.publish('visibility-drift'))['status'] == 'approved', 'fresh visibility approval poisoned')
    require('STALE-CANDIDATE' in h.traffic('pub-visibility-drift'), 'fresh visibility candidate did not serve')
