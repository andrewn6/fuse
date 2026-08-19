# vendored noVNC

`core/` and `vendor/` are noVNC v1.7.0 (https://github.com/novnc/noVNC),
unmodified, under the MPL-2.0 license in LICENSE.txt. Only the embeddable
RFB client library is vendored; the noVNC app UI, tests, and docs are not.

`viewer.html` is ours: the page `fuse desktop` serves, which embeds the
vendored RFB client against the CLI's local websocket bridge.

To update: replace `core/`, `vendor/`, and LICENSE.txt from a release
tarball, and update the version in this file.
