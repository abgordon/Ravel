package iptables

import "testing"

var testData []byte = []byte(`# Generated by iptables-save v1.4.21 on Wed Mar 22 00:38:34 2017
*nat
:PREROUTING ACCEPT [7:420]
:KUBE-IPVS - [0:0]
:KUBE-MARK-MASQ - [0:0]
:KUBE-SEP-2CYGKEFSDFORQH3J - [0:0]
:KUBE-SERVICES - [0:0]
:KUBE-SVC-ZSTEUXYJ236S7BT6 - [0:0]
-A PREROUTING -m comment --comment "kubernetes service portals" -j KUBE-SERVICES
-A PREROUTING -m addrtype --dst-type LOCAL -j DOCKER
-A KUBE-SEP-2CYGKEFSDFORQH3J -s 192.168.232.4/32 -m comment --comment "emc-local/nodeport-auto:http" -j KUBE-MARK-MASQ
-A KUBE-SEP-2CYGKEFSDFORQH3J -p tcp -m comment --comment "emc-local/nodeport-auto:http" -m tcp -j DNAT --to-destination 192.168.232.4:80
-A KUBE-SERVICES -d 192.168.1.128/32 -p tcp -m comment --comment "test-env-lolcats/my-nginx:omgwtfbbq cluster IP" -m tcp --dport 80 -j KUBE-SVC-ZSTEUXYJ236S7BT6
COMMIT
# Completed on Wed Mar 22 00:38:34 2017`)

func TestGetSaveLines(t *testing.T) {
	r, err := GetSaveLines("nat", testData)
	if err != nil {
		t.Fatal(err)
	}

	if len(r) != 6 {
		t.Fatalf("expected six chains in rules set. saw %d", len(r))
	}

	if len(r["PREROUTING"].Rules) != 2 {
		t.Fatalf("expected two rules in PREROUTING chain. saw %d", len(r["PREROUTING"].Rules))
	}

	sum := 0
	for _, chain := range r {
		sum += len(chain.Rules)
	}
	if sum != 5 {
		t.Fatalf("expected five rules total. saw %d", sum)
	}
}
