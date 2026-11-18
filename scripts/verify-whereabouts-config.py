"""Check the staging Whereabouts version's CNI 1.1 configuration handling.

This uses synthetic addresses and an ephemeral Go module, not a Kubernetes
cluster, IP allocation, router, or Thinpost deployment. Go must be installed
and may need network access to download the pinned public module and its
dependencies. It checks parsing and result serialization only.
"""

import subprocess
import tempfile
from pathlib import Path


PROGRAM = r'''package main

import (
    "encoding/json"
    "fmt"
    "net"

    current "github.com/containernetworking/cni/pkg/types/100"
    "github.com/k8snetworkplumbingwg/whereabouts/pkg/config"
)

func main() {
    input := []byte(`{"cniVersion":"1.1.0","name":"thinpost-lab","type":"ipvlan","ipam":{"type":"whereabouts","range":"198.51.100.0/24","range_start":"198.51.100.42","range_end":"198.51.100.42","gateway":"192.0.2.1","routes":[{"dst":"192.0.2.1/32","gw":"0.0.0.0","table":100,"scope":253},{"dst":"0.0.0.0/0","gw":"192.0.2.1","table":100}]}}`)
    parsed, version, err := config.LoadIPAMConfig(input, "K8S_POD_NAMESPACE=default;K8S_POD_NAME=lab", "whereabouts.conf")
    if err != nil { panic(err) }
    if version != "1.1.0" || len(parsed.IPRanges) != 1 || !parsed.IPRanges[0].RangeStart.Equal(net.ParseIP("198.51.100.42")) || !parsed.IPRanges[0].RangeEnd.Equal(net.ParseIP("198.51.100.42")) || !parsed.Gateway.Equal(net.ParseIP("192.0.2.1")) { panic("range or gateway changed") }
    if len(parsed.Routes) != 2 || parsed.Routes[0].Table == nil || *parsed.Routes[0].Table != 100 || parsed.Routes[0].Scope == nil || *parsed.Routes[0].Scope != 253 || parsed.Routes[1].Table == nil || *parsed.Routes[1].Table != 100 { panic("route fields changed") }
    output, err := json.Marshal(&current.Result{CNIVersion: version, Routes: parsed.Routes})
    if err != nil { panic(err) }
    var result struct { Routes []struct { Table *int `json:"table"`; Scope *int `json:"scope"` } `json:"routes"` }
    if err = json.Unmarshal(output, &result); err != nil { panic(err) }
    if len(result.Routes) != 2 || result.Routes[0].Table == nil || *result.Routes[0].Table != 100 || result.Routes[0].Scope == nil || *result.Routes[0].Scope != 253 { panic("result lost route fields") }
    // A second outbound address is a second attachment: its own range,
    // gateway and routing table, parsed independently of the first.
    second := []byte(`{"cniVersion":"1.1.0","name":"thinpost-lab-2","type":"ipvlan","ipam":{"type":"whereabouts","range":"203.0.113.0/24","range_start":"203.0.113.10","range_end":"203.0.113.10","gateway":"198.51.100.1","routes":[{"dst":"198.51.100.1/32","gw":"0.0.0.0","table":101,"scope":253},{"dst":"0.0.0.0/0","gw":"198.51.100.1","table":101}]}}`)
    parsed2, _, err := config.LoadIPAMConfig(second, "K8S_POD_NAMESPACE=default;K8S_POD_NAME=lab", "whereabouts.conf")
    if err != nil { panic(err) }
    if len(parsed2.IPRanges) != 1 || !parsed2.IPRanges[0].RangeStart.Equal(net.ParseIP("203.0.113.10")) || !parsed2.IPRanges[0].RangeEnd.Equal(net.ParseIP("203.0.113.10")) || !parsed2.Gateway.Equal(net.ParseIP("198.51.100.1")) { panic("second range or gateway changed") }
    if len(parsed2.Routes) != 2 || parsed2.Routes[0].Table == nil || *parsed2.Routes[0].Table != 101 || parsed2.Routes[1].Table == nil || *parsed2.Routes[1].Table != 101 { panic("second attachment route table changed") }
    fmt.Println("Whereabouts v0.9.4 preserves CNI 1.1 route table and scope for two attachments")
}
'''


def main():
    with tempfile.TemporaryDirectory(prefix="thinpost-whereabouts-") as directory:
        Path(directory, "main.go").write_text(PROGRAM)
        Path(directory, "whereabouts.conf").write_text('{"kubernetes":{"kubeconfig":"/dummy"}}')
        subprocess.run(
            ["go", "mod", "init", "thinpost-whereabouts-lab"], cwd=directory,
            check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        subprocess.run(
            ["go", "get", "github.com/k8snetworkplumbingwg/whereabouts/pkg/config@v0.9.4"],
            cwd=directory, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        subprocess.run(["go", "run", "."], cwd=directory, check=True)


if __name__ == "__main__":
    main()
