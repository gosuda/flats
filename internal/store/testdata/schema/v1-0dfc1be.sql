-- Metadata database as written by store.Open at commit 0dfc1be: the v1
-- schema plus the pagepaths analytics table, still user_version 1. The
-- schema below is the verbatim DDL of that commit.
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
  PRIMARY KEY (flat, number)
);
CREATE TABLE IF NOT EXISTS deployments (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
  version INTEGER NOT NULL,
  previous INTEGER NOT NULL,
  kind TEXT NOT NULL,
  at INTEGER NOT NULL
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
  decided_at INTEGER
);
CREATE TABLE IF NOT EXISTS previews (
  host TEXT PRIMARY KEY,
  flat TEXT NOT NULL REFERENCES flats(slug) ON DELETE CASCADE ON UPDATE CASCADE,
  version INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  last_access INTEGER NOT NULL
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

INSERT INTO flats(slug,name,visibility,live_version,created_at,updated_at,old_slug,old_slug_until) VALUES
  ('blog','Blog','public-listed',2,1700000000000,1700000300000,'oldblog',4102444800000),
  ('notes','Notes','public-unlisted',0,1700000000000,1700000000000,NULL,NULL),
  ('docs','Docs','private',1,1700000000000,1700000200000,'blog',4102444800000),
  ('shop','Shop','private',0,1700000000000,1700000000000,NULL,NULL);
INSERT INTO versions(flat,number,hash,size,files,kind,manifest,git_sha,git_dirty,message,created_at,screenshot,pruned) VALUES
  ('blog',1,'h-blog-1',100,2,'static','{"files":2}','abc123',0,'first',1700000010000,NULL,0),
  ('blog',2,'h-blog-2',120,3,'static','{"files":3}',NULL,1,NULL,1700000020000,'shot.png',0),
  ('blog',3,'h-blog-3',130,3,'server','{"files":3}',NULL,0,'saved only',1700000030000,NULL,0),
  ('docs',1,'h-docs-1',50,1,'static','{}',NULL,0,NULL,1700000040000,NULL,0),
  ('notes',1,'h-notes-1',10,1,'static','{}',NULL,0,NULL,1700000050000,NULL,1);
INSERT INTO deployments(flat,version,previous,kind,at) VALUES
  ('blog',1,0,'deploy',1700000100000),
  ('blog',2,1,'deploy',1700000300000),
  ('docs',1,0,'deploy',1700000200000);
INSERT INTO events(flat,at,level,kind,message,data) VALUES
  ('blog',1700000100000,'info','deploy','deployed v1',NULL),
  ('blog',1700000300000,'info','deploy','deployed v2','{"version":2}');
INSERT INTO approvals(id,flat,action,params,status,via,reason,result,requested_at,decided_at) VALUES
  ('a-vis','shop','set_visibility','{"visibility":"public-listed","from":"private"}','pending','mcp','launch',NULL,1700000400000,NULL),
  ('a-del','notes','delete','{}','rejected','api',NULL,'kept',1700000400000,1700000500000),
  ('a-old','blog','set_visibility','{"visibility":"public-unlisted"}','approved','cli',NULL,'done',1700000000000,1700000001000);
INSERT INTO previews(host,flat,version,created_at,last_access) VALUES
  ('p-blog-3','blog',3,1700000030000,1700000600000);
INSERT INTO settings(key,value) VALUES ('portal.relays','https://relay.example.com');
INSERT INTO pageviews(flat,day,count) VALUES ('blog','2023-11-14',42);
INSERT INTO secrets(flat,name,nonce,ciphertext,updated_at) VALUES ('blog','API_KEY',X'0102',X'A1B2C3',1700000000000);
INSERT INTO redirects(old,flat,until) VALUES
  ('oldblog','blog',4102444800000),
  ('ancient','docs',4102444800000);
INSERT INTO pagepaths(flat,day,path,count) VALUES
  ('blog','2023-11-14','/',30),
  ('blog','2023-11-14','/about',12);
PRAGMA user_version = 1;
