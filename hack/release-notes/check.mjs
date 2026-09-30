// Reads a commit message on stdin and fails when release-please would drop it.
//
// release-please parses every commit on main with @conventional-commits/parser and catches a parse
// failure at DEBUG level, so a commit that does not parse disappears from the changelog and from the
// version bump without a word. That is how #388 and its three breaking changes went missing from
// 0.50.0: one body line opened with `^([0-9]+(\.[0-9]+)?...`, which the grammar reads as a
// `type(scope` token and cannot close.
//
// The parser is pinned to the version release-please 17 depends on, the major that
// googleapis/release-please-action v5 bundles. Bump it with the action, not on its own.
//
// Usage: git log --format='* %B' origin/main..HEAD | node hack/release-notes/check.mjs
import { readFileSync } from 'node:fs';
import { parser } from '@conventional-commits/parser';

const message = readFileSync(0, 'utf8');
try {
  parser(message);
  console.log('ok: release-please can parse this message');
} catch (err) {
  const match = /at (\d+):(\d+)/.exec(err.message);
  const line = match ? message.split('\n')[Number(match[1]) - 1] : undefined;
  console.log('## release-please would drop this commit from the changelog');
  console.log('');
  console.log(`The Conventional Commits parser fails: ${err.message}`);
  if (line !== undefined) {
    console.log('');
    console.log('```');
    console.log(line);
    console.log('```');
  }
  console.log('');
  console.log('A body line that starts with a word followed by nested parentheses, like `f(a(b))`, reads as');
  console.log('an unclosed `type(scope` token. Indent the line, or start it with other text, in the commit');
  console.log('message that holds it (the squash message is built from every commit on the branch).');
  process.exit(1);
}
