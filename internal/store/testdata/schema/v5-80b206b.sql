-- Metadata database as written by store.Open at commit 80b206b, the
-- approval-gated lifecycle release (user_version 5). The schema below is
-- the verbatim DDL of that commit, followed by the objects its migrate
-- created on every database: legacy_private_upgrade and approvals_open_key.
CREATE TABLE IF NOT EXISTS flats (
  slug TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  visibility TEXT NOT NULL DEFAULT 'private',
  live_version INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  old_slug TEXT,
  old_slug_until INTEGER
);
CREATE TABLE IF NOT EXISTS versions (
  flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
  number INTEGER NOT NULL,
  hash TEXT NOT NULL,
  size INTEGER NOT NULL,
  files INTEGER NOT NULL,
  kind TEXT NOT NULL,
  manifest TEXT NOT NULL,
  git_sha TEXT,
  git_dirty INTEGER NOT NULL DEFAULT 0,
  message TEXT,
  created_at INTEGER NOT NULL,
  screenshot TEXT,
  pruned INTEGER NOT NULL DEFAULT 0,
  published INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (flat, number)
);
CREATE TABLE IF NOT EXISTS deployments (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
  version INTEGER NOT NULL,
  previous INTEGER NOT NULL,
  kind TEXT NOT NULL,
  at INTEGER NOT NULL,
  approval_id TEXT
);
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  flat TEXT NOT NULL,
  at INTEGER NOT NULL,
  level TEXT NOT NULL,
  kind TEXT NOT NULL,
  message TEXT NOT NULL,
  data TEXT
);
CREATE INDEX IF NOT EXISTS events_flat ON events(flat, id);
CREATE TABLE IF NOT EXISTS approvals (
  id TEXT PRIMARY KEY,
  flat TEXT NOT NULL,
  action TEXT NOT NULL,
  params TEXT NOT NULL,
  status TEXT NOT NULL,
  via TEXT NOT NULL,
  reason TEXT,
  result TEXT,
  requested_at INTEGER NOT NULL,
  decided_at INTEGER,
  idempotency_key TEXT,
  result_data TEXT,
  decided_by TEXT,
  authorized_at INTEGER
);
CREATE TABLE IF NOT EXISTS previews (
  host TEXT PRIMARY KEY,
  flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
  version INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  last_access INTEGER NOT NULL,
  target TEXT NOT NULL DEFAULT 'version',
  revision INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS drafts (
  flat TEXT PRIMARY KEY REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
  revision INTEGER NOT NULL,
  hash TEXT NOT NULL,
  base_version INTEGER NOT NULL DEFAULT 0,
  dirty INTEGER NOT NULL DEFAULT 1,
  size INTEGER NOT NULL,
  files INTEGER NOT NULL,
  kind TEXT NOT NULL,
  manifest TEXT NOT NULL,
  git_sha TEXT,
  git_dirty INTEGER NOT NULL DEFAULT 0,
  message TEXT,
  updated_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS draft_revisions (
  flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
  revision INTEGER NOT NULL,
  hash TEXT NOT NULL,
  size INTEGER NOT NULL,
  files INTEGER NOT NULL,
  kind TEXT NOT NULL,
  manifest TEXT NOT NULL,
  git_sha TEXT,
  git_dirty INTEGER NOT NULL DEFAULT 0,
  message TEXT,
  screenshot TEXT,
  created_at INTEGER NOT NULL,
  pruned INTEGER NOT NULL DEFAULT 0,
  legacy_number INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (flat, revision)
);
CREATE TABLE IF NOT EXISTS flat_providers (
  flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
  provider TEXT NOT NULL,
  permitted INTEGER NOT NULL,
  PRIMARY KEY (flat, provider)
);
CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS pageviews (
  flat TEXT NOT NULL,
  day TEXT NOT NULL,
  count INTEGER NOT NULL,
  PRIMARY KEY (flat, day)
);
CREATE TABLE IF NOT EXISTS pagepaths (
  flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
  day TEXT NOT NULL,
  path TEXT NOT NULL,
  count INTEGER NOT NULL,
  PRIMARY KEY (flat, day, path)
);
CREATE TABLE IF NOT EXISTS redirects (
  old TEXT PRIMARY KEY,
  flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
  until INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS secrets (
  flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
  name TEXT NOT NULL,
  nonce BLOB NOT NULL,
  ciphertext BLOB NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (flat, name)
);
CREATE TABLE IF NOT EXISTS legacy_private_upgrade (flat TEXT PRIMARY KEY REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE, pending INTEGER NOT NULL DEFAULT 1);
CREATE UNIQUE INDEX IF NOT EXISTS approvals_open_key ON approvals(idempotency_key) WHERE idempotency_key IS NOT NULL AND status IN ('pending','applying');

INSERT INTO flats(slug,name,visibility,live_version,created_at,updated_at,old_slug,old_slug_until) VALUES
  ('blog','Blog','public',2,1700000000000,1700000300000,NULL,NULL),
  ('notes','Notes','private',0,1700000000000,1700000000000,NULL,NULL),
  ('docs','Docs','private',1,1700000000000,1700000200000,'oldblog',4102444800000);
INSERT INTO versions(flat,number,hash,size,files,kind,manifest,git_sha,git_dirty,message,created_at,screenshot,pruned,published) VALUES
  ('blog',1,'h-blog-1',100,2,'static','{"files":2}','abc123',0,'first',1700000010000,NULL,0,1),
  ('blog',2,'h-blog-2',120,3,'static','{"files":3}',NULL,1,NULL,1700000020000,'shot.png',0,1),
  ('docs',1,'h-docs-1',50,1,'static','{}',NULL,0,NULL,1700000040000,NULL,0,1);
INSERT INTO deployments(flat,version,previous,kind,at,approval_id) VALUES
  ('blog',1,0,'deploy',1700000100000,NULL),
  ('blog',2,1,'publish',1700000300000,'a-pub'),
  ('docs',1,0,'deploy',1700000200000,NULL);
INSERT INTO drafts(flat,revision,hash,base_version,dirty,size,files,kind,manifest,git_sha,git_dirty,message,updated_at,created_at) VALUES
  ('blog',3,'h-blog-3',2,1,130,3,'server','{"files":3}',NULL,0,'saved only',1700000030000,1700000030000);
INSERT INTO draft_revisions(flat,revision,hash,size,files,kind,manifest,git_sha,git_dirty,message,screenshot,created_at,pruned,legacy_number) VALUES
  ('blog',3,'h-blog-3',130,3,'server','{"files":3}',NULL,0,'saved only',NULL,1700000030000,0,3);
INSERT INTO events(flat,at,level,kind,message,data) VALUES
  ('blog',1700000300000,'info','deploy','published v2','{"version":2}');
INSERT INTO approvals(id,flat,action,params,status,via,reason,result,requested_at,decided_at,idempotency_key,result_data,decided_by,authorized_at) VALUES
  ('a-pub','blog','publish','{"version":2}','approved','mcp',NULL,'published',1700000250000,1700000300000,'k-pub','{"version":2}','operator',1700000290000),
  ('a-open','notes','set_visibility','{"visibility":"public","from":"private"}','pending','api','launch',NULL,1700000400000,NULL,'k-open',NULL,NULL,NULL);
INSERT INTO previews(host,flat,version,created_at,last_access,target,revision) VALUES
  ('p-blog-draft','blog',0,1700000030000,1700000600000,'draft',3);
INSERT INTO flat_providers(flat,provider,permitted) VALUES
  ('blog','tailscale',1),
  ('blog','portal',0);
INSERT INTO legacy_private_upgrade(flat,pending) VALUES
  ('blog',0),
  ('docs',1);
INSERT INTO settings(key,value) VALUES ('portal.relays','https://relay.example.com');
INSERT INTO pageviews(flat,day,count) VALUES ('blog','2023-11-14',42);
INSERT INTO pagepaths(flat,day,path,count) VALUES ('blog','2023-11-14','/',30);
INSERT INTO redirects(old,flat,until) VALUES ('oldblog','docs',4102444800000);
INSERT INTO secrets(flat,name,nonce,ciphertext,updated_at) VALUES ('blog','API_KEY',X'0102',X'A1B2C3',1700000000000);
PRAGMA user_version = 5;
