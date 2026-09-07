# NZB download fails with authentication required

## Description

Clicking the Download NZB button on a release detail page navigates directly to
GET /api/v1/releases/{guid}/nzb, which requires a session token. Because a plain
<a href> link cannot send the Authorization Bearer header, the server returns
401 with authentication required.

## Root cause

ReleaseDetail.svelte used an anchor tag which bypasses api.js request() function
that attaches the Bearer token. The NZB route is protected by sess middleware.

## Fix applied

1. web/src/lib/api.js - Added downloadNZB(guid) that uses fetch() with the
   Bearer token, extracts filename from Content-Disposition, and triggers
   a programmatic download via a temporary blob URL.
2. web/src/routes/ReleaseDetail.svelte - Replaced <a href> with a <button>
   calling downloadNZB().

## Verification

- Go build: pass
- Go tests (auth, nzb, metrics, rest): all pass
- Web tests: 75 passed across 8 files
- Container redeployed to Unraid: running healthy

## Status

Closed - fixed and deployed.
