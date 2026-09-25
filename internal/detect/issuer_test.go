package detect

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/dns/dnsmessage"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// answer is a resolver's answer to one SOA query.
type answer struct {
	rcode dnsmessage.RCode
	zones []string
	err   error
}

// zones answers SOA queries from a table; a name not in it does not exist.
type zones map[string]answer

func (z zones) QuerySOA(_ context.Context, fqdn string) (dnsmessage.RCode, []string, error) {
	if a, ok := z[fqdn]; ok {
		return a.rcode, a.zones, a.err
	}
	return dnsmessage.RCodeNameError, nil, nil
}

func TestDNS01Zone(t *testing.T) {
	const fqdn = "_acme-challenge.models.wc1.acme.example.io"
	apex := answer{rcode: dnsmessage.RCodeSuccess, zones: []string{"acme.example.io."}}
	for _, tc := range []struct {
		name    string
		dns     zones
		zone    string
		wantErr string
	}{
		{name: "NXDOMAIN walks up to the zone's SOA", dns: zones{"acme.example.io.": apex}, zone: "acme.example.io."},
		{name: "NOERROR without SOA walks up too", dns: zones{fqdn + ".": {rcode: dnsmessage.RCodeSuccess}, "acme.example.io.": apex}, zone: "acme.example.io."},
		{name: "a self-CNAME wildcard answers SERVFAIL", dns: zones{fqdn + ".": {rcode: dnsmessage.RCodeServerFailure}, "acme.example.io.": apex}, wantErr: "SOA lookup of " + fqdn + ". answers SERVFAIL"},
		{name: "a parent that refuses stops the walk", dns: zones{"wc1.acme.example.io.": {rcode: dnsmessage.RCodeRefused}, "acme.example.io.": apex}, wantErr: "SOA lookup of wc1.acme.example.io. answers REFUSED"},
		{name: "no nameserver answers", dns: zones{fqdn + ".": {err: errors.New("i/o timeout")}}, wantErr: "SOA lookup of " + fqdn + ".: i/o timeout"},
		{name: "no SOA anywhere", dns: zones{}, wantErr: "no SOA record found for " + fqdn + " or any parent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			zone, err := DNS01Zone(context.Background(), tc.dns, fqdn)
			if tc.wantErr != "" {
				require.EqualError(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.zone, zone)
		})
	}
}

func issuer(name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1", "kind": "ClusterIssuer",
		"metadata": map[string]any{"name": name},
		"spec":     spec,
	}}
}

func acme(solvers ...any) map[string]any {
	return map[string]any{"acme": map[string]any{"solvers": solvers}}
}

var (
	dns01  = map[string]any{"dns01": map[string]any{"route53": map[string]any{}}}
	http01 = map[string]any{"http01": map[string]any{"ingress": map[string]any{}}}
)

func selected(solver map[string]any, selector map[string]any) map[string]any {
	out := map[string]any{"selector": selector}
	for k, v := range solver {
		out[k] = v
	}
	return out
}

func TestJudgeIssuance(t *testing.T) {
	const host = "models.wc1.acme.example.io"
	servfail := zones{"_acme-challenge." + host + ".": {rcode: dnsmessage.RCodeServerFailure}}
	healthy := zones{"acme.example.io.": {rcode: dnsmessage.RCodeSuccess, zones: []string{"acme.example.io."}}}
	for _, tc := range []struct {
		name    string
		issuer  *unstructured.Unstructured
		dns     zones
		want    Issuance
		wantErr string
	}{
		{name: "DNS-01 with its zone discovered", issuer: issuer("le", acme(dns01)), dns: healthy, want: Issuance{Issuer: "le", Solver: SolverDNS01, Zone: "acme.example.io."}},
		{name: "DNS-01 whose challenge name fails", issuer: issuer("le", acme(dns01)), dns: servfail, wantErr: "ClusterIssuer le solves " + host + " by DNS-01, but the zone of _acme-challenge." + host + " cannot be discovered (SOA lookup of _acme-challenge." + host + ". answers SERVFAIL)"},
		{name: "HTTP-01 needs no zone", issuer: issuer("le", acme(http01)), dns: servfail, want: Issuance{Issuer: "le", Solver: SolverHTTP01}},
		{name: "dnsZones pick DNS-01 over the default HTTP-01", issuer: issuer("le", acme(http01, selected(dns01, map[string]any{"dnsZones": []any{"acme.example.io"}}))), dns: servfail, wantErr: "answers SERVFAIL"},
		{name: "dnsNames win over dnsZones", issuer: issuer("le", acme(selected(dns01, map[string]any{"dnsZones": []any{"example.io"}}), selected(http01, map[string]any{"dnsNames": []any{host}}))), dns: servfail, want: Issuance{Issuer: "le", Solver: SolverHTTP01}},
		{name: "the zone with the most labels wins", issuer: issuer("le", acme(selected(http01, map[string]any{"dnsZones": []any{"example.io"}}), selected(dns01, map[string]any{"dnsZones": []any{"acme.example.io"}}))), dns: healthy, want: Issuance{Issuer: "le", Solver: SolverDNS01, Zone: "acme.example.io."}},
		{name: "a zone the host is not in is not picked", issuer: issuer("le", acme(selected(dns01, map[string]any{"dnsZones": []any{"other.io"}}))), wantErr: "ClusterIssuer le has no ACME solver for " + host},
		{name: "matchLabels select no unlabelled Certificate", issuer: issuer("le", acme(selected(dns01, map[string]any{"matchLabels": map[string]any{"team": "x"}}), http01)), dns: servfail, want: Issuance{Issuer: "le", Solver: SolverHTTP01}},
		{name: "a CA issuer is not ACME", issuer: issuer("le", map[string]any{"ca": map[string]any{"secretName": "ca"}}), dns: servfail, want: Issuance{Issuer: "le"}},
		{name: "no such issuer", issuer: issuer("other", acme(dns01)), wantErr: "ClusterIssuer le does not exist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), tc.issuer)
			got, err := JudgeIssuance(context.Background(), reader, tc.dns, "le", host)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestResolverQuerySOA runs the wire query against a nameserver on
// loopback that answers SERVFAIL for one name and an SOA for the rest.
func TestResolverQuerySOA(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	go serveSOA(conn)

	r := &Resolver{Nameservers: []string{conn.LocalAddr().String()}, Timeout: 2 * time.Second}
	rcode, zones, err := r.QuerySOA(context.Background(), "acme.example.io.")
	require.NoError(t, err)
	assert.Equal(t, dnsmessage.RCodeSuccess, rcode)
	assert.Equal(t, []string{"acme.example.io."}, zones)

	rcode, zones, err = r.QuerySOA(context.Background(), "_acme-challenge.models.acme.example.io.")
	require.NoError(t, err)
	assert.Equal(t, dnsmessage.RCodeServerFailure, rcode)
	assert.Empty(t, zones)

	dead := &Resolver{Nameservers: []string{"127.0.0.1:1"}, Timeout: 200 * time.Millisecond}
	_, _, err = dead.QuerySOA(context.Background(), "acme.example.io.")
	assert.Error(t, err, "no nameserver answers")
}

func serveSOA(conn net.PacketConn) {
	buf := make([]byte, 512)
	for {
		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			return
		}
		var p dnsmessage.Parser
		h, err := p.Start(buf[:n])
		if err != nil {
			continue
		}
		q, err := p.Question()
		if err != nil {
			continue
		}
		msg := dnsmessage.Message{Header: dnsmessage.Header{ID: h.ID, Response: true, RecursionAvailable: true}, Questions: []dnsmessage.Question{q}}
		if q.Name.String() == "_acme-challenge.models.acme.example.io." {
			msg.RCode = dnsmessage.RCodeServerFailure
		} else {
			msg.Answers = []dnsmessage.Resource{{
				Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeSOA, Class: dnsmessage.ClassINET, TTL: 60},
				Body:   &dnsmessage.SOAResource{NS: dnsmessage.MustNewName("ns1.example.io."), MBox: dnsmessage.MustNewName("hostmaster.example.io."), Serial: 1, Refresh: 60, Retry: 60, Expire: 60, MinTTL: 60},
			}}
		}
		out, err := msg.Pack()
		if err != nil {
			continue
		}
		_, _ = conn.WriteTo(out, addr)
	}
}
