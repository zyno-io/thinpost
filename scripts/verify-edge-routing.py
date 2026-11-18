"""Exercise candidate Edge CNI binaries in a disposable local Docker lab.

This uses synthetic IPs and static IPAM. It does not change a cluster, router,
or public network, and does not validate Whereabouts or node-failure fencing.
The gateways are RFC 5737 documentation addresses; this lab path applies no Thinpost address validation.
The disposable local test container is privileged for Linux namespace setup;
the Thinpost workload itself must not be privileged or have these capabilities.
"""

import argparse
import re
import shutil
import subprocess
import tempfile
from pathlib import Path


IMAGE = "ghcr.io/siderolabs/install-cni:v1.9.0@sha256:53bdeb6c8d238dd593ee75f265baf4625925a034d8d18f10ffeae88d7fcf2c3e"
LAB = r"""
set -eu
apk add --no-cache iproute2 >/dev/null
ip link add thinpost-parent type dummy
ip link set thinpost-parent up
ip netns add thinpost-test
ip -n thinpost-test link set lo up
ip -n thinpost-test link add eth0 type dummy
ip -n thinpost-test addr add 10.254.0.2/24 dev eth0
ip -n thinpost-test link set eth0 up
ip -n thinpost-test route add default via 10.254.0.1 dev eth0
cat > /tmp/ipvlan.json <<'JSON'
{"cniVersion":"1.1.0","name":"thinpost-test","type":"@IPVLAN@","master":"thinpost-parent","mode":"l2","ipam":{"type":"static","addresses":[{"address":"198.51.100.42/24","gateway":"192.0.2.1"}],"routes":[{"dst":"192.0.2.1/32","gw":"0.0.0.0","table":100,"scope":253},{"dst":"0.0.0.0/0","gw":"192.0.2.1","table":100}]}}
JSON
# A second outbound address (an additionalSources entry) is a second attachment:
# its own interface, gateway and routing table.
cat > /tmp/ipvlan2.json <<'JSON'
{"cniVersion":"1.1.0","name":"thinpost-test-2","type":"@IPVLAN@","master":"thinpost-parent","mode":"l2","ipam":{"type":"static","addresses":[{"address":"192.0.2.43/24","gateway":"198.51.100.1"}],"routes":[{"dst":"198.51.100.1/32","gw":"0.0.0.0","table":101,"scope":253},{"dst":"0.0.0.0/0","gw":"198.51.100.1","table":101}]}}
JSON
export CNI_COMMAND=ADD CNI_CONTAINERID=thinpost-test CNI_NETNS=/var/run/netns/thinpost-test CNI_IFNAME=net1 CNI_PATH=/opt/cni/bin
if ! /opt/cni/bin/@IPVLAN@ < /tmp/ipvlan.json > /tmp/result.json; then cat /tmp/result.json; exit 1; fi
node -e "const fs=require('fs');fs.writeFileSync('/tmp/sbr.json',JSON.stringify({cniVersion:'1.1.0',name:'thinpost-test',type:'@SBR@',table:100,prevResult:JSON.parse(fs.readFileSync('/tmp/result.json'))}));"
/opt/cni/bin/@SBR@ < /tmp/sbr.json > /dev/null
CNI_IFNAME=net2 /opt/cni/bin/@IPVLAN@ < /tmp/ipvlan2.json > /tmp/result2.json || { cat /tmp/result2.json; exit 1; }
node -e "const fs=require('fs');fs.writeFileSync('/tmp/sbr2.json',JSON.stringify({cniVersion:'1.1.0',name:'thinpost-test-2',type:'@SBR@',table:101,prevResult:JSON.parse(fs.readFileSync('/tmp/result2.json'))}));"
CNI_IFNAME=net2 /opt/cni/bin/@SBR@ < /tmp/sbr2.json > /dev/null
ip -n thinpost-test route get 8.8.8.8 from 198.51.100.42 | tee /tmp/public-route
grep -q 'via 192.0.2.1 dev net1 table 100' /tmp/public-route
ip -n thinpost-test route get 8.8.8.8 from 192.0.2.43 | tee /tmp/public-route-2
grep -q 'via 198.51.100.1 dev net2 table 101' /tmp/public-route-2
# Every address also listens for SMTP, so a reply to a client that connected to
# the second address must leave from that address, through its own interface,
# gateway and table, not the primary's. A client is any remote address.
ip -n thinpost-test route get 203.0.113.9 from 192.0.2.43 | tee /tmp/inbound-reply-2
grep -q 'via 198.51.100.1 dev net2 table 101' /tmp/inbound-reply-2
ip -n thinpost-test route get 203.0.113.9 from 198.51.100.42 | tee /tmp/inbound-reply-1
grep -q 'via 192.0.2.1 dev net1 table 100' /tmp/inbound-reply-1
ip -n thinpost-test route get 8.8.8.8 from 10.254.0.2 | tee /tmp/private-route
grep -q 'via 10.254.0.1 dev eth0' /tmp/private-route
CNI_COMMAND=DEL CNI_IFNAME=net2 /opt/cni/bin/@SBR@ < /tmp/sbr2.json >/dev/null
CNI_COMMAND=DEL CNI_IFNAME=net2 /opt/cni/bin/@IPVLAN@ < /tmp/ipvlan2.json >/dev/null
CNI_COMMAND=DEL /opt/cni/bin/@SBR@ < /tmp/sbr.json >/dev/null
CNI_COMMAND=DEL /opt/cni/bin/@IPVLAN@ < /tmp/ipvlan.json >/dev/null
if ip -n thinpost-test link show net1 >/dev/null 2>&1 || ip -n thinpost-test link show net2 >/dev/null 2>&1; then exit 1; fi
if ip -n thinpost-test rule show | grep -q 'from 198.51.10[01]'; then exit 1; fi
printf '%s\n' 'Source routing for two attachments, replies leaving from the address a client connected to, primary default route, and teardown passed.'
"""


def parse_args():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--ipvlan-type", default="ipvlan", help="CNI binary/type name for ipvlan (default: ipvlan)")
    parser.add_argument("--sbr-type", default="sbr", help="CNI binary/type name for sbr (default: sbr)")
    parser.add_argument("--cni-image", default=IMAGE, help="image providing /opt/cni/bin (default: the pinned install-cni image)")
    return parser.parse_args()


def main():
    args = parse_args()
    names = (args.ipvlan_type, args.sbr_type)
    if any(not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]*", name) for name in names) or len(set(names)) != 2 \
            or args.ipvlan_type == "sbr" or args.sbr_type == "ipvlan":
        raise SystemExit("--ipvlan-type and --sbr-type must be distinct plain file names and must not swap the upstream names")
    lab = LAB.replace("@IPVLAN@", args.ipvlan_type).replace("@SBR@", args.sbr_type)
    with tempfile.TemporaryDirectory(prefix="thinpost-cni-", dir=Path.home()) as directory:
        binary_dir = Path(directory) / "bin"
        container = subprocess.check_output(["docker", "create", args.cni_image, "/bin/sh"], text=True).strip()
        try:
            subprocess.run(["docker", "cp", f"{container}:/opt/cni/bin", str(binary_dir)], check=True)
        finally:
            subprocess.run(["docker", "rm", container], check=True, stdout=subprocess.DEVNULL)
        # Clusters that install the plugins under another file name (for example a
        # versioned one) pass it as --ipvlan-type/--sbr-type; the upstream image
        # ships plain "ipvlan" and "sbr", so provide the requested name.
        for upstream, name in (("ipvlan", args.ipvlan_type), ("sbr", args.sbr_type)):
            if name != upstream:
                shutil.copy2(binary_dir / upstream, binary_dir / name)
        subprocess.run(
            [
                "docker", "run", "--rm", "--privileged", "-v", f"{binary_dir}:/opt/cni/bin:ro",
                "node:26-alpine", "sh", "-c", lab,
            ],
            check=True,
        )


if __name__ == "__main__":
    main()
