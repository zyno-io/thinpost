# Thinpost licensing

Copyright (c) 2026 Signal24 LLC dba Zyno Consulting.

Author: Zyno Consulting <oss@zyno.io>.

Thinpost is released under the [MIT License](LICENSE). Every first-party
component in this repository uses it: the control and edge services, the
operator console (`ui/`) and its styles, the API contracts (`api/`), the Helm
chart (`resources/helm/chart/`), the documentation, the webhook test samples
(written for this project; they mirror the structure of provider webhook
payloads with synthetic values) and the test tooling.

`internal/thirdparty/gosmtp/` is a patched copy of
[emersion/go-smtp](https://github.com/emersion/go-smtp) v0.26.0, which is also
MIT-licensed. It keeps its own copyright notice in
`internal/thirdparty/gosmtp/LICENSE`; the changes are listed in
`internal/thirdparty/gosmtp/PATCHES.md`.

Third-party dependencies retain their own licenses and notices. This policy
does not relicense third-party software.

## Contributions

Contributions are accepted under the MIT License. Contributors must have the
right to license what they submit and must retain applicable third-party
notices.
