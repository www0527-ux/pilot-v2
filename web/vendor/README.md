# Browser dependencies

These pinned browser builds are served by Pilot so Markdown rendering works
without access to an external CDN. They are unchanged files from the npm
packages; package integrity was verified against the npm registry metadata.

| Dependency | Version | npm package file | Upstream |
| --- | --- | --- | --- |
| Marked | 18.1.0 | `lib/marked.umd.js` | https://github.com/markedjs/marked |
| DOMPurify | 3.4.16 | `dist/purify.min.js` | https://github.com/cure53/DOMPurify |

Licenses are included alongside the scripts. When updating, keep the filenames
and the script references in `index.html` in sync and run the frontend tests.
