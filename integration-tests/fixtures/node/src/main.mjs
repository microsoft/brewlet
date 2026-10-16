import http from 'node:http';
import { message } from '../dependencies/greeting.mjs';

function report() {
  return {
    language: 'node',
    runtime: process.versions.node,
    dependency: message(),
    arguments: process.argv.slice(2),
    cwd: process.cwd(),
    uid: process.getuid(),
    gid: process.getgid(),
    path: process.env.PATH ?? null,
    home: process.env.HOME ?? null,
    value: process.env.BREWLET_TEST_VALUE ?? null,
  };
}

if (process.argv.includes('--probe')) {
  console.log(JSON.stringify(report()));
} else {
  http.createServer((_request, response) => {
    response.writeHead(200, { 'Content-Type': 'application/json' });
    response.end(JSON.stringify(report()));
  }).listen(8000, '0.0.0.0');
}
