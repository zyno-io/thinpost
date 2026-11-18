#!/usr/bin/env python3
"""Public allow-list leak check.

Scans the repository's files for IPs, DNS names, e-mail addresses, MAC
addresses and SPIFFE IDs that are not listed in .github/leak-allow.json, and
for forbidden file paths. It is allow-list based on purpose: it does not
contain, and must never be given, a list of the names it is meant to catch.

  leak-check.py [--report] [--deny FILE] [--allow FILE] [--self-test] [ROOT]

--report     also print the unique inventory of what was found, for review.
--deny       optional file of regular expressions (one per line, "#" comments)
             matched case-insensitively against file paths and contents. It
             is used by a private layer and is never part of this repository.
--self-test  run built-in probe cases against the allow-list and exit.

Address policy: this public layer flags globally routable addresses and real
domains. It deliberately allows every non-global special-purpose address (the
IANA special-purpose registries: private, shared, loopback, link-local,
multicast, reserved, documentation, benchmarking, ULA, translation ranges and
so on), because those carry no information a reader could use. Telling an
organisation's own subnets apart from other private space is the private
layer's job and is not attempted here.

Source files (see code_extensions in the allow-list) only match bare host
names against a short list of hosting TLDs. The full IANA list matches
identifier chains such as row.id or f.app by the thousand, so a bare host name
with an uncommon TLD inside source code can slip through; URL hosts and e-mail
addresses are checked in full everywhere.

Exit status is 1 when anything fails the checks. Standard library only.

Known omissions, kept to avoid noise: bare 32-bit decimal integers are only
treated as IPs in obvious contexts (URL hosts and "ip/addr/host = N"
assignments); bare 12-hex strings are only treated as MACs on lines that
mention a MAC-like word.
"""
import argparse
import fnmatch
import ipaddress
import json
import os
import re
import subprocess
import sys
import urllib.parse

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.abspath(os.path.join(HERE, ".."))
DEFAULT_ALLOW = os.path.join(REPO, ".github", "leak-allow.json")

LABEL = r"[a-z0-9_](?:[a-z0-9_-]*[a-z0-9_])?"
IPV4_RE = re.compile(r"(?<![\w.])((?:\d{1,4}\.){3}\d{1,4})(?![\w])(?!\.\d)")
IPV4_PARTIAL_RE = re.compile(r"(?<![\w.])(\d{1,3}\.\d{1,3}\.\d{1,3})\.(?=$|[\s\"'`,)*/xX])")
IPV4_DASHED_RE = re.compile(r"(?<![\w-])(?:(?:ip|ec2)-)(\d{1,3})-(\d{1,3})-(\d{1,3})-(\d{1,3})(?![\w-])")
IPV4_DASHED_HOST_RE = re.compile(r"(?<![\w-])(\d{1,3})-(\d{1,3})-(\d{1,3})-(\d{1,3})(?=\.[a-z])")
IPV4_INT_CTX_RE = re.compile(r"\b(?:ip|addr|address|host)\w*[\"']?\s*[:=]\s*[\"']?(\d{8,10})(?![\w.])")
IPV6_RE = re.compile(
    r"(?<![\w:.])((?:[0-9a-f]{0,4}:){2,7}(?:\d{1,3}(?:\.\d{1,3}){3}|[0-9a-f]{0,4}))(?![\w:])(?!\.\d)"
)
IPV6_PARTIAL_RE = re.compile(r"(?<![\w:.])((?:[0-9a-f]{1,4}:){3,7})(?![\w:])")
EMAIL_RE = re.compile(r"[a-z0-9._%+-]+@((?:[a-z0-9-]+\.)+[a-z][a-z0-9-]+)")
DOMAIN_RE = re.compile(rf"(?<![a-z0-9-])((?:{LABEL}\.)+[a-z][a-z0-9-]*[a-z0-9])(?![a-z0-9-])")
URL_RE = re.compile(r"[a-z][a-z0-9+.-]*://[^\s\"'<>`)\]}]+")
MAC_RE = re.compile(r"(?<![\w:-])((?:[0-9a-f]{2}[:-]){5}[0-9a-f]{2})(?![\w:-])")
MAC_DOTTED_RE = re.compile(r"(?<![\w.])([0-9a-f]{4}\.[0-9a-f]{4}\.[0-9a-f]{4})(?![\w.])")
MAC_BARE_RE = re.compile(r"(?<![0-9a-z])([0-9a-f]{12})(?![0-9a-z])")
SPIFFE_RE = re.compile(r"spiffe://([^/\s\"'`)<>]+)")
HEX_RE = re.compile(r"(?<![0-9a-z])[0-9a-f]{32,}(?![0-9a-z])")
LOCK_HOST_RE = re.compile(r'"resolved"\s*:\s*"(?:[a-z+]+)://([^/"]+)')
QUOTED_RE = re.compile(r'"(?:[^"\\]|\\.)*"|\'(?:[^\'\\]|\\.)*\'')
PCT_RE = re.compile(r"%[0-9a-fA-F]{2}")
HOSTNAME_RE = re.compile(r"[a-z0-9_]([a-z0-9_.-]*[a-z0-9_])?")
STRINGS_RE = re.compile(rb"[\x20-\x7e]{6,}")

CHECKS = ("ipv4", "ipv6", "domain", "email", "mac", "spiffe", "url")


def glob_match(path, pattern):
    if pattern.endswith("/**"):
        return path.startswith(pattern[:-2])
    return fnmatch.fnmatchcase(path, pattern) or fnmatch.fnmatchcase(os.path.basename(path), pattern)


def any_glob(path, patterns):
    return any(glob_match(path, p) for p in patterns)


def list_files(root):
    try:
        out = subprocess.run(
            ["git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
            check=True, capture_output=True,
        ).stdout.decode("utf-8", "surrogateescape")
        files = [p for p in out.split("\0") if p]
    except (OSError, subprocess.CalledProcessError):
        files = []
        for base, dirs, names in os.walk(root):
            dirs[:] = [d for d in dirs if d not in (".git", "node_modules")]
            for n in names:
                files.append(os.path.relpath(os.path.join(base, n), root))
    return sorted(p for p in files if os.path.isfile(os.path.join(root, p)))


def decode_bytes(data):
    """Return (text, kind). kind is "text", "utf16" or "binary"."""
    if data.startswith((b"\xff\xfe", b"\xfe\xff")):
        return data.decode("utf-16", "replace"), "utf16"
    if b"\0" in data[:8192]:
        head = data[:8192]
        # UTF-16 without a BOM: NULs alternate with ASCII bytes.
        even, odd = head[0::2].count(0), head[1::2].count(0)
        half = len(head) // 2
        if half and (even > half * 0.8 or odd > half * 0.8):
            return data.decode("utf-16-le" if odd > even else "utf-16-be", "replace"), "utf16"
        strings = [m.group(0) for m in STRINGS_RE.finditer(data)]
        return "\n".join(s.decode("ascii") for s in strings), "binary"
    return data.decode("utf-8", "replace"), "text"


def ipv4_interpretations(text):
    """All plausible readings of a dotted quad: decimal, and octal/hex for
    zero-prefixed or 0x parts. Returns a list of IPv4Address, or []."""
    parts = text.split(".")
    if len(parts) != 4:
        return []
    readings = []
    for base in (10, 8, 16):
        vals = []
        for p in parts:
            try:
                if p.lower().startswith("0x"):
                    if base != 16:
                        raise ValueError
                    v = int(p, 16)
                elif base == 16:
                    v = int(p, 10)
                elif base == 8 and len(p) > 1 and p.startswith("0"):
                    v = int(p, 8)
                elif p.isdigit():
                    v = int(p, 10)
                else:
                    raise ValueError
            except ValueError:
                vals = None
                break
            if v > 255:
                vals = None
                break
            vals.append(v)
        if vals is not None:
            addr = ipaddress.IPv4Address(bytes(vals))
            if addr not in readings:
                readings.append(addr)
    return readings


class Allow:
    def __init__(self, cfg, repo=REPO):
        self.cfg = cfg
        self.v4_nets = [ipaddress.ip_network(c) for c in cfg["ipv4_cidrs"]]
        self.v4_exact = {ipaddress.ip_address(a) for a in cfg["ipv4_exact"]}
        self.v6_nets = [ipaddress.ip_network(c) for c in cfg["ipv6_cidrs"]]
        self.v6_exact = {ipaddress.ip_address(a) for a in cfg["ipv6_exact"]}
        self.domains = {d.lower() for d in cfg["domains"]}  # exact names only
        self.example_suffixes = [d.lower() for d in cfg["example_domains"]]
        self.domains_exact = {d.lower() for d in cfg["domains_exact"]}
        self.services = {s.lower() for s in cfg["service_names"]}
        self.namespaces = {n.lower() for n in cfg["namespaces"]}
        self.spellings = set(cfg.get("ipv4_spellings_exact", []))
        self.email_exact = {d.lower() for d in cfg.get("email_domains_exact", [])}
        self.url_hosts = {h.lower() for h in cfg["url_hosts_exact"]}
        self.reserved = {t.lower() for t in cfg["reserved_tlds"]}
        self.src_ext = {t.lower() for t in cfg["source_extensions"]}
        self.emails = {e.lower() for e in cfg["email_addresses"]}
        self.email_domains = [d.lower() for d in cfg["email_domains"]]
        self.code_ext = {e.lower() for e in cfg["code_extensions"]}
        self.code_tlds = {t.lower() for t in cfg["code_file_tlds"]}
        self.mac_prefixes = tuple(p.lower() for p in cfg["macs"]["prefixes"])
        self.mac_exact = {m.lower() for m in cfg["macs"]["exact"]}
        self.mac_words = [w.lower() for w in cfg["mac_context_words"]]
        self.spiffe = {d.lower() for d in cfg["spiffe_trust_domains"]}
        tld_path = os.path.join(repo, cfg["known_tlds_file"])
        with open(tld_path, encoding="ascii") as fh:
            self.tlds = {ln.strip().lower() for ln in fh if ln.strip() and not ln.startswith("#")}
        self.tlds |= {t.lower() for t in cfg["suspect_tlds"]}

    def ipv4_ok(self, addr):
        return addr in self.v4_exact or any(addr in n for n in self.v4_nets)

    def ipv6_ok(self, addr):
        if addr in self.v6_exact:
            return True
        if addr.ipv4_mapped is not None:
            return self.ipv4_ok(addr.ipv4_mapped)
        for embedded in embedded_ipv4(addr):
            if not self.ipv4_ok(embedded):
                return False
        return addr in self.v6_exact or any(addr in n for n in self.v6_nets) or bool(embedded_ipv4(addr))

    def ipv6_prefix_ok(self, groups):
        bits = 16 * len(groups)
        padded = ":".join(groups) + "::"
        try:
            net = ipaddress.ip_network(f"{padded}/{bits}", strict=False)
        except ValueError:
            return True
        return any(net.subnet_of(n) for n in self.v6_nets) or net.network_address in self.v6_exact

    def domain_ok(self, host):
        host = host.lower().rstrip(".")
        if host.rsplit(".", 1)[-1] in self.reserved:
            return True
        if host in self.domains_exact:
            return True
        m = re.fullmatch(r"([a-z0-9-]+\.[a-z0-9-]+)\.svc\.cluster\.local", host)
        if m and m.group(1) in self.services:
            return True
        # Short in-cluster forms: <service>.<namespace>.svc and <namespace>.svc[.cluster.local].
        m = re.fullmatch(r"(?:([a-z0-9-]+)\.)?([a-z0-9-]+)\.svc(?:\.cluster\.local)?", host)
        if m and ((m.group(1) and f"{m.group(1)}.{m.group(2)}" in self.services) or (not m.group(1) and m.group(2) in self.namespaces)):
            return True
        if host in self.domains:
            return True
        return any(host == d or host.endswith("." + d) for d in self.example_suffixes)

    def email_ok(self, addr, host):
        if addr.lower() in self.emails:
            return True
        host = host.lower()
        if host.rsplit(".", 1)[-1] in self.reserved:
            return True
        if host in self.email_exact:
            return True
        if host.endswith(".svc") or host.endswith(".cluster.local"):
            return self.domain_ok(host)
        return any(host == d or host.endswith("." + d) for d in self.email_domains)

    def is_code(self, path):
        return path.rsplit(".", 1)[-1].lower() in self.code_ext

    def domain_relevant(self, host, followed_by_paren=False, code=False, quoted=False):
        labels = host.split(".")
        tld = labels[-1]
        if tld not in self.tlds:
            return False
        # A quoted multi-label or hyphenated name is host-shaped; a bare
        # identifier chain such as row.id or README.md is not.
        host_shaped = len(labels) >= 3 or any("-" in l for l in labels)
        if tld in self.src_ext:
            return quoted and host_shaped
        if code and tld not in self.code_tlds and not host.endswith(".cluster.local"):
            # Identifier chains such as a.id or row.name look like hosts to
            # the full TLD list, so bare names in source files only use the
            # hosting TLDs. Quoted host-shaped names use the full list.
            return quoted and host_shaped
        if followed_by_paren and len(labels) <= 2:
            return False  # obj.method( is code, not a host
        return True


def embedded_ipv4(addr):
    """IPv4 addresses carried inside NAT64, Teredo and 6to4 addresses."""
    out = []
    n = int(addr)
    if addr in ipaddress.ip_network("64:ff9b::/96") or addr in ipaddress.ip_network("64:ff9b:1::/48"):
        out.append(ipaddress.IPv4Address(n & 0xFFFFFFFF))
    elif addr in ipaddress.ip_network("2001::/32"):
        out.append(ipaddress.IPv4Address((n >> 64) & 0xFFFFFFFF))  # Teredo server
        out.append(ipaddress.IPv4Address((n & 0xFFFFFFFF) ^ 0xFFFFFFFF))  # client
    elif addr in ipaddress.ip_network("2002::/16"):
        out.append(ipaddress.IPv4Address((n >> 80) & 0xFFFFFFFF))
    return out


def blank(m):
    """Replace a match with spaces of the same length, keeping offsets."""
    return " " * len(m.group(0))


def lowered_variants(line):
    """The raw line and, when it has percent escapes, the decoded line."""
    low = line.lower()
    variants = [(low, line)]
    if PCT_RE.search(low):
        decoded = urllib.parse.unquote(line)
        if decoded.lower() != low:
            variants.append((decoded.lower(), decoded))
    return variants


def aton_readings(host):
    """inet_aton-style readings of 1-4 numeric parts (decimal, octal, hex)."""
    parts = host.split(".")
    out = []
    for octal in (False, True):
        try:
            nums = []
            for p in parts:
                if p.startswith("0x"):
                    nums.append(int(p, 16))
                elif octal and len(p) > 1 and p.startswith("0"):
                    nums.append(int(p, 8))
                else:
                    nums.append(int(p, 10))
        except ValueError:
            continue
        *head, last = nums
        if any(v > 255 for v in head) or last >= 256 ** (4 - len(head)):
            continue
        value = 0
        for v in head:
            value = (value << 8) | v
        value = (value << (8 * (4 - len(head)))) | last
        addr = ipaddress.IPv4Address(value)
        if addr not in out:
            out.append(addr)
    return out


def check_ipv4_text(text, allow):
    """Return ["ipv4"] if any reading of this spelling is globally routable."""
    readings = ipv4_interpretations(text)
    if text in allow.spellings:
        return []
    return ["ipv4"] if any(not allow.ipv4_ok(a) for a in readings) else []


def check_host(host, allow):
    """Judge a URL host. Returns (check, ok)."""
    host = host.lower().rstrip(".")
    if not host or not (HOSTNAME_RE.fullmatch(host) or ":" in host):
        return None, True  # templated or unparsable; nothing concrete to judge
    if ":" in host:
        try:
            addr = ipaddress.ip_address(host)
        except ValueError:
            return None, True
        return "ipv6", allow.ipv6_ok(addr)
    if re.fullmatch(r"(?:0x[0-9a-f]+|\d+)(?:\.(?:0x[0-9a-f]+|\d+)){0,3}", host):
        return "ipv4", all(allow.ipv4_ok(a) for a in aton_readings(host))
    if host in allow.url_hosts or host.rsplit(".", 1)[-1] in allow.reserved:
        return "domain", True
    return "domain", allow.domain_ok(host)


def scan_line(line, allow, active, code=False):
    """Return (bad, inv): disallowed (check, value) items and inventory items."""
    bad, inv = [], []
    for low, orig in lowered_variants(line):
        _scan_one(low, allow, active, bad, inv, code, orig)
    seen = set()
    uniq = []
    for item in bad:
        if item not in seen:
            seen.add(item)
            uniq.append(item)
    return uniq, inv


def _scan_one(work, allow, active, bad, inv, code=False, orig=""):
    qspans = [(m.start(), m.end()) for m in QUOTED_RE.finditer(work)]
    work = re.sub(r"\$\{\{.*?\}\}", blank, work)  # GitHub Actions expressions
    for m in SPIFFE_RE.finditer(work):
        inv.append(("spiffe", m.group(0)))
        td = m.group(1).lower()
        # A trust domain under a reserved TLD (.example, .test, ...) or a shell
        # template (spiffe://$domain) cannot name a real environment.
        reserved_td = td.rsplit(".", 1)[-1] in allow.reserved and "." in td
        if "spiffe" in active and td not in allow.spiffe and not reserved_td and not td.startswith("$"):
            bad.append(("spiffe", m.group(0)))

    for m in URL_RE.finditer(work):
        url = m.group(0)
        inv.append(("url", url))
        if "url" not in active:
            continue
        try:
            host = urllib.parse.urlsplit(url).hostname
        except ValueError:
            continue
        if host:
            check, ok = check_host(host, allow)
            if check and not ok:
                bad.append((check, f"{host} (in URL)"))

    for m in EMAIL_RE.finditer(work):
        inv.append(("email", m.group(0)))
        if "email" in active and not allow.email_ok(m.group(0), m.group(1)):
            bad.append(("email", m.group(0)))
    work = EMAIL_RE.sub(blank, work)

    for m in MAC_RE.finditer(work):
        mac = m.group(1).replace("-", ":")
        inv.append(("mac", m.group(1)))
        if "mac" in active and mac not in allow.mac_exact and not mac.startswith(allow.mac_prefixes):
            bad.append(("mac", m.group(1)))
    mac_ctx = any(w in work for w in allow.mac_words)
    cands = [m.group(1) for m in MAC_DOTTED_RE.finditer(work)]
    if mac_ctx:
        cands += [m.group(1) for m in MAC_BARE_RE.finditer(work) if re.search(r"[a-f]", m.group(1))]
    for raw in cands:
        h = raw.replace(".", "")
        mac = ":".join(h[i:i + 2] for i in range(0, 12, 2))
        inv.append(("mac", raw))
        if "mac" in active and mac not in allow.mac_exact and not mac.startswith(allow.mac_prefixes):
            bad.append(("mac", raw))
    work = MAC_RE.sub(blank, work)

    if "ipv4" in active:
        for m in IPV4_RE.finditer(work):
            inv.append(("ipv4", m.group(1)))
            bad.extend((c, m.group(1)) for c in check_ipv4_text(m.group(1), allow))
        for m in IPV4_PARTIAL_RE.finditer(work):
            first = int(m.group(1).split(".")[0])
            inv.append(("ipv4-prefix", m.group(1) + "."))
            # Small first octets are almost always version numbers.
            if first >= 11 and check_ipv4_text(m.group(1) + ".0", allow):
                bad.append(("ipv4", m.group(1) + ". (partial)"))
        for rx in (IPV4_DASHED_RE, IPV4_DASHED_HOST_RE):
            for m in rx.finditer(work):
                dotted = ".".join(m.groups())
                inv.append(("ipv4-dashed", m.group(0)))
                bad.extend((c, m.group(0)) for c in check_ipv4_text(dotted, allow))
        for m in IPV4_INT_CTX_RE.finditer(work):
            n = int(m.group(1))
            if 16777216 <= n <= 0xFFFFFFFF:
                inv.append(("ipv4-int", m.group(1)))
                if not allow.ipv4_ok(ipaddress.IPv4Address(n)):
                    bad.append(("ipv4", m.group(1) + " (integer form)"))

    if "ipv6" in active:
        for m in IPV6_RE.finditer(work):
            try:
                addr = ipaddress.ip_address(m.group(1))
            except ValueError:
                continue
            inv.append(("ipv6", m.group(1)))
            if not allow.ipv6_ok(addr):
                bad.append(("ipv6", m.group(1)))
        for m in IPV6_PARTIAL_RE.finditer(work):
            groups = m.group(1).rstrip(":").split(":")
            if not any(len(g) >= 3 for g in groups) or all(g.isdigit() and len(g) <= 2 for g in groups):
                continue
            inv.append(("ipv6-prefix", m.group(1)))
            if not allow.ipv6_prefix_ok(groups):
                bad.append(("ipv6", m.group(1) + " (partial)"))

    if "domain" in active:
        for m in DOMAIN_RE.finditer(work):
            host = m.group(1)
            if re.fullmatch(r"[\d.]+", host):
                continue
            paren = work[m.end():m.end() + 1] == "("
            if m.start() and work[m.start() - 1] == "%":
                continue  # "%40host" in the raw form; the decoded form covers it
            quoted = any(a <= m.start() and m.end() <= b for a, b in qspans)
            if code and not quoted and len(orig) == len(work):
                tld = orig[m.start():m.end()].rsplit(".", 1)[-1]
                if tld[:1].isupper() and tld[1:] == tld[1:].lower() and len(tld) > 1:
                    continue  # Title-case last label: an exported identifier such as c.Net
            while host and host.split(".")[0].startswith("_"):
                host = host.split(".", 1)[1] if "." in host else ""
            if not host or "." not in host:
                continue
            if not allow.domain_relevant(host, paren, code, quoted):
                continue
            inv.append(("domain", host))
            if not allow.domain_ok(host):
                bad.append(("domain", host))

    for m in HEX_RE.finditer(work):
        inv.append(("hex", m.group(0)))


def scan_name(rel, allow):
    """Check a file or directory path for the same kinds of leaks."""
    parts = rel.split("/")
    if "." in parts[-1].lstrip("."):
        parts[-1] = parts[-1].rsplit(".", 1)[0]  # drop the file extension
    return scan_line(" ".join(parts), allow, set(CHECKS), False)[0]


def scan_text(text, allow, active=CHECKS, code=False):
    """Scan a whole text; returns [(lineno, check, value)] and inventory."""
    findings, inventory = [], []
    for n, line in enumerate(text.splitlines(), 1):
        bad, inv = scan_line(line, allow, set(active), code)
        findings.extend((n, c, v) for c, v in bad)
        inventory.extend(inv)
    return findings, inventory


# --- self-test -------------------------------------------------------------

MUST_FLAG = [
    ("URL host", "see https://internal.corp-example.net/path"),
    ("uppercase URL host", "HTTPS://HOST.EXAMPLE-CORP.NET/X"),
    ("uppercase bare host", "Host.Example-Corp.NET"),
    ("wildcard", "*.x.example-corp.net"),
    ("underscore label", "_dmarc.example-corp.net"),
    ("percent-encoded", "%2F%2Fhost.example-corp.net"),
    ("non-listed TLD", "host.example-corp.ovh"),
    ("call-like host", "host.example-corp.net()"),
    ("leading-zero octet", "addr 11.22.33.044"),
    ("dashed ip host", "ip-11-22-33-44"),
    ("dashed bare host", "11-22-33-44.example-corp.net"),
    ("NAT64", "64:ff9b::b16:212c"),
    ("Teredo client", "2001:0:4136:e378:8000:63bf:f4e9:de93"),
    ("6to4", "2002:0b16:212c::1"),
    ("dotted MAC", "bc24.1100.0001"),
    ("bare MAC with context", "mac=bc2411000001"),
    ("one-letter label with suffix", "s.dev"),
    ("partial ipv4 prefix", "range 203.1.2."),
    ("partial ipv6 prefix", "prefix 2a05:abcd:1234:"),
    ("integer URL host", "http://185273388/x"),
    ("integer in ip context", 'ip = "185273388"'),
    ("public IPv4 in URL", "https://11.22.33.44:8443/"),
    ("subdomain of exact-only domain", "internal.zyno.io"),
    ("unlisted cluster service", "db.private-ns.svc.cluster.local"),
    ("cluster.local subdomain", "x.cluster.local"),
    ("github.io subdomain", "someone.github.io"),
    ("non-allowed email", "user@example-corp.net"),
    ("percent-encoded email", "user%40example-corp.net"),
    ("spiffe", "spiffe://example-corp.net/ns/a"),
    ("unlisted /12 address", "172.19.7.7"),
    ("unlisted /10 address", "100.77.1.1"),
    ("EUI-64 link-local", "fe80::0211:22ff:fe33:4455"),
    ("ISATAP link-local", "fe80::5efe:0b16:212c"),
    ("ISATAP dotted", "fe80::5efe:11.22.33.44"),
    ("unicast-prefix multicast", "ff3e:40:2001:db8::1234"),
    ("NAT64 local-use prefix", "64:ff9b:1::b16:212c"),
    ("source-extension host in quotes", 'x = "host.example-corp.md"'),
    ("source-extension host in URL", "https://x.example-corp.sh/"),
    ("source-extension host in email", "user@example-corp.py"),
    ("zip host in quotes", '"a.example-corp.zip"'),
    ("percent-encoded dot", "host%2eexample-corp.net"),
    ("percent-encoded path after host", "https://host.example-corp.net%2Fpath"),
    ("four-digit padded octets", "0011.0022.0033.0044"),
    ("public IPv6", "2a05:abcd:1234::1"),
    ("real public IPv4", "9.9.9.9"),
    ("hex spelling of public", "http://0x0b.22.33.44/"),
    ("octal reading is public", "http://045.0.0.10/"),
    ("Teredo with public server", "2001:0:4136:e378:8000:63bf:f4e9:de93"),
]
MUST_PASS = [
    ("reserved .example", "host.example"),
    ("reserved .test", "a.b.test"),
    ("reserved .invalid", "x.y.invalid"),
    ("localhost", "http://localhost:8080/"),
    ("rfc5737", "192.0.2.10 198.51.100.7 203.0.113.9"),
    ("stand-in range 1", "45.0.1.2"),
    ("stand-in range 2", "11.42.51.9"),
    ("rfc1918", "10.1.2.3 192.168.1.1"),
    ("loopback", "127.0.0.1"),
    ("contact", "oss@zyno.io"),
    ("project site", "https://zyno-io.github.io/thinpost/ zyno.io"),
    ("vendor", "https://github.com/zyno-io/thinpost"),
    ("example domains", "foo.example.com bar.example.org"),
    ("doc ipv6", "2001:db8::1"),
    ("nat64 of doc address", "64:ff9b::c000:0201"),
    ("code", "fmt.Println(x); obj.method(); config.json; main.go; a.ts"),
    ("allowed service", "hooks.mail.svc.cluster.local"),
    ("sample MAC", "00:00:5e:00:53:01"),
    ("special-purpose v4", "10.1.2.3 192.168.9.9 169.254.1.1 224.0.0.1 240.0.0.1 198.19.0.1 0.1.2.3"),
    ("special-purpose v6", "2001:20::1 100::1 ::ffff:10.1.2.3"),
    ("listed exact literals", "172.16.0.0 100.64.0.0 fe80::1 fc00:: ff00:: fd00::"),
    ("README in quotes", 'see "README.md" and "config.json" and "run.sh"'),
    ("NAT64 local-use of doc address", "64:ff9b:1::c000:0201"),
    ("hex spelling of loopback", "http://0x7f.0.0.1/"),
    ("ULA prefix", "prefix fd00:10:96:"),
    ("version string", "v1.2.3.4 and go 1.26.0"),
    ("short time", "at 12:30: or 10:20:30"),
    ("hash without context", "bc2411000001"),
]


CODE_MUST_FLAG = [
    ("code: title-case label inside quotes", 'host := "mx1.example-corp.Net"'),
    ("code: quoted host-shaped full TLD", 'host := "mx1.example-corp.ovh"'),
    ("code: dotted host", 'dial("mx1.example-corp.net:25")'),
    ("code: URL with any TLD", 'url := "https://host.example-corp.ovh/"'),
    ("code: cluster host", 'addr := "db.other-ns.svc.cluster.local"'),
]
CODE_MUST_PASS = [
    ("code: member access", "row.id m.app tc.name f.app claim.event.id"),
    ("code: reserved name", 'host := "mx.example"'),
    ("code: title-case identifier", "c.Net = 1; volume.Dev"),
    ("code: quoted two-label identifier", 'x := "row.id"'),
]


NAME_MUST_FLAG = [
    ("file name", "docs/host.example-corp.net.md"),
    ("directory name", "files/11.22.33.44/readme"),
]
NAME_MUST_PASS = [("ordinary names", "docs/contributing/index.md"), ("example name", "tests/a.example/x.go")]


def self_test(allow):
    failures = 0
    for name, text in MUST_FLAG:
        found, _ = scan_text(text, allow)
        if not found:
            print(f"self-test FAIL (not flagged): {name}: {text!r}")
            failures += 1
    for name, text in MUST_PASS:
        found, _ = scan_text(text, allow)
        if found:
            print(f"self-test FAIL (wrongly flagged): {name}: {text!r} -> {found}")
            failures += 1
    for name, text in CODE_MUST_FLAG:
        if not scan_text(text, allow, code=True)[0]:
            print(f"self-test FAIL (code, not flagged): {name}: {text!r}")
            failures += 1
    for name, text in CODE_MUST_PASS:
        if scan_text(text, allow, code=True)[0]:
            print(f"self-test FAIL (code, wrongly flagged): {name}: {text!r}")
            failures += 1
    for name, rel in NAME_MUST_FLAG:
        if not scan_name(rel, allow):
            print(f"self-test FAIL (name not flagged): {name}: {rel!r}")
            failures += 1
    for name, rel in NAME_MUST_PASS:
        if scan_name(rel, allow):
            print(f"self-test FAIL (name wrongly flagged): {name}: {rel!r}")
            failures += 1
    # UTF-16 and binary handling.
    text, kind = decode_bytes("host.example-corp.net\n".encode("utf-16"))
    if kind != "utf16" or not scan_text(text, allow)[0]:
        print("self-test FAIL: UTF-16 content not scanned")
        failures += 1
    text, kind = decode_bytes(b"\x00\x01\x02 junk \x00 https://internal.corp-example.net/ \x00\x00")
    if kind != "binary" or not scan_text(text, allow)[0]:
        print("self-test FAIL: binary strings not scanned")
        failures += 1
    total = len(MUST_FLAG) + len(MUST_PASS) + len(CODE_MUST_FLAG) + len(CODE_MUST_PASS) + len(NAME_MUST_FLAG) + len(NAME_MUST_PASS) + 2
    print(f"self-test: {total - failures}/{total} probes behaved as expected")
    return 1 if failures else 0


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("root", nargs="?", default=REPO)
    ap.add_argument("--report", action="store_true", help="print the unique inventory for review")
    ap.add_argument("--deny", metavar="FILE", help="private regex deny-list (one per line)")
    ap.add_argument("--allow", metavar="FILE", default=DEFAULT_ALLOW, help="allow-list JSON")
    ap.add_argument("--self-test", action="store_true", help="run built-in probe cases and exit")
    args = ap.parse_args()

    root = os.path.abspath(args.root)
    with open(args.allow, encoding="utf-8") as fh:
        cfg = json.load(fh)
    allow = Allow(cfg, repo=REPO)
    if args.self_test:
        return self_test(allow)

    deny = []
    if args.deny:
        with open(args.deny, encoding="utf-8") as fh:
            for raw in fh:
                raw = raw.strip()
                if raw and not raw.startswith("#"):
                    deny.append(re.compile(raw, re.IGNORECASE))

    findings = []  # (path, line, check, value)
    inventory = {}
    binary_files = []

    for rel in list_files(root):
        if any_glob(rel, cfg["forbidden_paths"]):
            findings.append((rel, 0, "path", "forbidden file"))
            continue
        for rx in deny:
            if rx.search(rel):
                findings.append((rel, 0, "deny", "path matches a deny pattern"))
        for check, value in scan_name(rel, allow):
            findings.append((rel, 0, "name-" + check, value))
        full = os.path.join(root, rel)

        try:
            with open(full, "rb") as fh:
                data = fh.read()
        except OSError:
            continue

        if any_glob(rel, cfg["lockfile_globs"]):
            for n, line in enumerate(data.decode("utf-8", "replace").splitlines(), 1):
                m = LOCK_HOST_RE.search(line)
                if m:
                    inventory.setdefault("lockfile-host", set()).add(m.group(1))
                    if m.group(1) not in cfg["lockfile_hosts"]:
                        findings.append((rel, n, "lockfile", m.group(1)))
            continue
        if any_glob(rel, cfg["skip_files"]):
            continue

        text, kind = decode_bytes(data)
        if kind == "binary":
            if text.strip():
                binary_files.append(rel)
        exempt = set()
        for pattern, kinds in cfg.get("path_exemptions", {}).items():
            if rel == pattern:  # exact paths only, never directory globs
                exempt.update(kinds)
        active = set(CHECKS) - exempt

        for n, line in enumerate(text.splitlines(), 1):
            bad, inv = scan_line(line, allow, active, allow.is_code(rel))
            for check, value in bad:
                findings.append((rel, n, check, value))
            for k, value in inv:
                inventory.setdefault(k, set()).add(value)
            for rx in deny:
                mm = rx.search(line)
                if mm:
                    findings.append((rel, n, "deny", mm.group(0)))

    if args.report:
        print("== inventory ==")
        for kind in sorted(inventory):
            print(f"-- {kind} ({len(inventory[kind])})")
            for value in sorted(inventory[kind]):
                print(f"   {value}")
        print("-- binary files containing text")
        for rel in binary_files:
            print(f"   {rel}")
        print()

    print("== findings ==")
    for path, line, check, value in sorted(findings):
        where = f"{path}:{line}" if line else path
        print(f"{where}: [{check}] {value}")
    print(f"{len(findings)} finding(s) in {len({f[0] for f in findings})} file(s)")
    return 1 if findings else 0


if __name__ == "__main__":
    sys.exit(main())
