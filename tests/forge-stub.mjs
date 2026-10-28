// A stand-in Forgejo for tests/image-smoke.conf: the four API routes Renovate's
// forgejo platform reads before it clones, and git smart HTTP through the image's
// own `git http-backend`. Every request must carry the token, as a real instance
// requires for a private repository, so a git client that drops the credentials
// on a follow-up request fails here the way it fails against a real forge.
// Usage: node forge-stub.mjs <port> <token> <git-project-root> <state-dir>
import { spawn } from 'node:child_process';
import { writeFileSync } from 'node:fs';
import { createServer } from 'node:http';
import { join } from 'node:path';

const [port, token, projectRoot, stateDir] = process.argv.slice(2);
if (!port || !token || !projectRoot || !stateDir) {
  console.error('usage: forge-stub.mjs <port> <token> <git-project-root> <state-dir>');
  process.exit(2);
}

const origin = `http://127.0.0.1:${port}`;
const owner = { id: 1, login: 'smoke', full_name: 'Smoke Bot', email: 'smoke@example.com' };
const repo = {
  id: 1,
  full_name: 'smoke/repo',
  owner,
  clone_url: `${origin}/smoke/repo.git`,
  default_branch: 'main',
  permissions: { admin: false, push: true, pull: true },
  allow_merge_commits: true,
  default_merge_style: 'merge',
  has_issues: true,
  has_pull_requests: true,
  archived: false,
  mirror: false,
  empty: false,
  fork: false,
};
const api = new Map([
  ['/api/v1/user', owner],
  ['/api/v1/version', { version: '11.0.0+gitea-1.22.0' }],
  ['/api/v1/repos/smoke/repo', repo],
]);
// The owner is a user, so the organization lookup is a known 404.
const absent = new Set(['/api/v1/orgs/smoke']);

// Per route: requests answered, and requests refused for a missing token.
const stats = { served: {}, refused: {}, unknown: [] };
const statsPath = join(stateDir, 'stats.json');
function record(kind, route) {
  stats[kind][route] = (stats[kind][route] ?? 0) + 1;
  writeFileSync(statsPath, JSON.stringify(stats));
}

// Forgejo takes a token as the API's `token`/`Bearer` scheme, or in either half of
// HTTP Basic, which is where git puts the credentials of a clone URL.
function authorized(header) {
  if (!header) {
    return false;
  }
  const [scheme, value = ''] = header.split(' ', 2);
  if (/^(token|bearer)$/i.test(scheme)) {
    return value === token;
  }
  if (/^basic$/i.test(scheme)) {
    const decoded = Buffer.from(value, 'base64').toString();
    const colon = decoded.indexOf(':');
    const user = colon < 0 ? decoded : decoded.slice(0, colon);
    const password = colon < 0 ? '' : decoded.slice(colon + 1);
    return user === token || password === token;
  }
  return false;
}

function sendJson(res, status, body) {
  res.writeHead(status, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify(body));
}

function serveGit(req, res, url, body) {
  const env = {
    ...process.env,
    GIT_PROJECT_ROOT: projectRoot,
    GIT_HTTP_EXPORT_ALL: '1',
    PATH_INFO: url.pathname,
    QUERY_STRING: url.search.slice(1),
    REQUEST_METHOD: req.method,
    CONTENT_TYPE: req.headers['content-type'] ?? '',
    CONTENT_LENGTH: String(body.length),
    // Set only after the token check, so receive-pack accepts the push.
    REMOTE_USER: owner.login,
    REMOTE_ADDR: '127.0.0.1',
  };
  if (req.headers['content-encoding']) {
    env.HTTP_CONTENT_ENCODING = req.headers['content-encoding'];
  }
  if (req.headers['git-protocol']) {
    env.HTTP_GIT_PROTOCOL = req.headers['git-protocol'];
  }
  const cgi = spawn('git', ['http-backend'], { env });
  const chunks = [];
  cgi.stdout.on('data', (chunk) => chunks.push(chunk));
  cgi.stderr.on('data', (chunk) => process.stderr.write(chunk));
  cgi.on('close', () => {
    const out = Buffer.concat(chunks);
    const split = out.indexOf('\r\n\r\n');
    const end = split < 0 ? out.indexOf('\n\n') : split;
    if (end < 0) {
      res.writeHead(502).end();
      return;
    }
    let status = 200;
    const headers = {};
    for (const line of out.subarray(0, end).toString().split(/\r?\n/)) {
      const colon = line.indexOf(':');
      const name = line.slice(0, colon).trim();
      const value = line.slice(colon + 1).trim();
      if (name.toLowerCase() === 'status') {
        status = Number.parseInt(value, 10);
      } else if (name) {
        headers[name] = value;
      }
    }
    res.writeHead(status, headers);
    res.end(out.subarray(end + (split < 0 ? 2 : 4)));
  });
  cgi.stdin.end(body);
}

const server = createServer((req, res) => {
  const url = new URL(req.url, origin);
  const gitRoute = url.pathname.match(/\.git\/(info\/refs|git-upload-pack|git-receive-pack)$/);
  const route = gitRoute ? `git ${gitRoute[1]}` : `${req.method} ${url.pathname}`;
  const chunks = [];
  req.on('data', (chunk) => chunks.push(chunk));
  req.on('end', () => {
    if (!authorized(req.headers.authorization)) {
      record('refused', route);
      res.writeHead(401, { 'WWW-Authenticate': 'Basic realm="forge-stub"' }).end();
      return;
    }
    if (gitRoute) {
      record('served', route);
      serveGit(req, res, url, Buffer.concat(chunks));
      return;
    }
    if (req.method === 'GET' && api.has(url.pathname)) {
      record('served', route);
      sendJson(res, 200, api.get(url.pathname));
      return;
    }
    // Any other route is logged, so a Renovate release that starts to depend on a
    // new one names it in the failure output.
    if (!absent.has(url.pathname)) {
      stats.unknown.push(route);
      writeFileSync(statsPath, JSON.stringify(stats));
    }
    sendJson(res, 404, { message: 'not found' });
  });
});

server.listen(Number(port), '127.0.0.1', () => {
  writeFileSync(join(stateDir, 'ready'), origin);
});
