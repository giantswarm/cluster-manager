package detect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// ClusterIssuerGVR is cert-manager's ClusterIssuer: the one the models
// listener's Certificate is issued by, judged before the slice composes it.
var ClusterIssuerGVR = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "clusterissuers"}

// Solvers of an ACME issuer.
const (
	SolverDNS01  = "dns01"
	SolverHTTP01 = "http01"
)

// acmeChallengeLabel is the label a DNS-01 challenge's TXT record is
// published under, in front of the name being validated.
const acmeChallengeLabel = "_acme-challenge."

// Issuance is how a host's certificate would be issued: the ClusterIssuer,
// the ACME solver cert-manager picks for the host (empty for an issuer that
// is not ACME), and for DNS-01 the zone its challenge record goes into.
type Issuance struct {
	Issuer string `json:"issuer"`
	Solver string `json:"solver,omitempty"`
	Zone   string `json:"zone,omitempty"`
}

// SOAQuerier asks a recursive resolver for a name's SOA record: the
// response code and the owner names of the SOA records in the answer.
type SOAQuerier interface {
	QuerySOA(ctx context.Context, fqdn string) (dnsmessage.RCode, []string, error)
}

// JudgeIssuance reads the ClusterIssuer on the cluster and judges whether it
// can issue a certificate for host: it must exist, and when the solver
// cert-manager picks for host is DNS-01, the zone discovery of the
// challenge's name must resolve (DNS01Zone). The error says why it cannot,
// in words a caller acts on.
func JudgeIssuance(ctx context.Context, reader dynamic.Interface, q SOAQuerier, issuer, host string) (Issuance, error) {
	out := Issuance{Issuer: issuer}
	ci, err := reader.Resource(ClusterIssuerGVR).Get(ctx, issuer, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return out, fmt.Errorf("ClusterIssuer %s does not exist: the models host's Certificate would never be issued", issuer)
	case err != nil:
		return out, fmt.Errorf("ClusterIssuer %s cannot be read: %w", issuer, err)
	}
	solvers, isACME, _ := unstructured.NestedSlice(ci.Object, "spec", "acme", "solvers")
	if !isACME {
		return out, nil
	}
	solver := solverFor(solvers, host)
	if solver == nil {
		return out, fmt.Errorf("ClusterIssuer %s has no ACME solver for %s: the models host's Certificate would never be issued", issuer, host)
	}
	if _, ok := solver[SolverHTTP01]; ok {
		out.Solver = SolverHTTP01
		return out, nil
	}
	if _, ok := solver[SolverDNS01]; !ok {
		return out, nil
	}
	out.Solver = SolverDNS01
	fqdn := acmeChallengeLabel + host
	if out.Zone, err = DNS01Zone(ctx, q, fqdn); err != nil {
		return out, fmt.Errorf("ClusterIssuer %s solves %s by DNS-01, but the zone of %s cannot be discovered (%w): cert-manager never publishes the challenge and the Certificate is never issued — fix the DNS records of the cluster's domain, or use an issuer that solves by HTTP-01", issuer, host, fqdn, err)
	}
	return out, nil
}

// solverFor is the solver cert-manager picks for a host among an ACME
// issuer's solvers: one naming the host in its dnsNames, else the one whose
// dnsZones match the host with the most labels, else one without a
// selector. A solver selecting by matchLabels is not picked: the chart's
// Certificate carries none.
func solverFor(solvers []any, host string) map[string]any {
	var picked map[string]any
	best := -1
	for _, raw := range solvers {
		s, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if score := solverScore(s, host); score > best {
			picked, best = s, score
		}
	}
	return picked
}

// solverScore ranks a solver for host the way cert-manager does; -1 when
// its selector does not select the host.
func solverScore(s map[string]any, host string) int {
	sel, _, _ := unstructured.NestedMap(s, "selector")
	if labels, _, _ := unstructured.NestedMap(sel, "matchLabels"); len(labels) > 0 {
		return -1
	}
	names, _, _ := unstructured.NestedStringSlice(sel, "dnsNames")
	zones, _, _ := unstructured.NestedStringSlice(sel, "dnsZones")
	if len(names) == 0 && len(zones) == 0 {
		return 0
	}
	for _, n := range names {
		if n == host {
			return 1 << 16
		}
	}
	score := -1
	for _, z := range zones {
		if (host == z || strings.HasSuffix(host, "."+z)) && strings.Count(z, ".")+1 > score {
			score = strings.Count(z, ".") + 1
		}
	}
	return score
}

// DNS01Zone discovers the zone a DNS-01 challenge record of fqdn goes into,
// the way cert-manager does: the SOA of fqdn and of each parent in turn,
// NXDOMAIN or an answer without SOA moving one label up, the first SOA the
// zone. Any other response code stops the discovery — a SERVFAIL of the
// challenge's name (a wildcard CNAME pointing at itself) fails every
// DNS-01 order of the host.
func DNS01Zone(ctx context.Context, q SOAQuerier, fqdn string) (string, error) {
	name := strings.TrimSuffix(fqdn, ".") + "."
	for {
		rcode, zones, err := q.QuerySOA(ctx, name)
		if err != nil {
			return "", fmt.Errorf("SOA lookup of %s: %w", name, err)
		}
		switch rcode {
		case dnsmessage.RCodeSuccess:
			if len(zones) > 0 {
				return zones[0], nil
			}
		case dnsmessage.RCodeNameError:
		default:
			return "", fmt.Errorf("SOA lookup of %s answers %s", name, rcodeName(rcode))
		}
		i := strings.Index(name, ".")
		if i == len(name)-1 {
			return "", fmt.Errorf("no SOA record found for %s or any parent", fqdn)
		}
		name = name[i+1:]
	}
}

// rcodeName is a response code as DNS tools print it (SERVFAIL, REFUSED).
func rcodeName(r dnsmessage.RCode) string {
	switch r {
	case dnsmessage.RCodeFormatError:
		return "FORMERR"
	case dnsmessage.RCodeServerFailure:
		return "SERVFAIL"
	case dnsmessage.RCodeNotImplemented:
		return "NOTIMP"
	case dnsmessage.RCodeRefused:
		return "REFUSED"
	}
	return fmt.Sprintf("RCODE%d", r)
}

// Resolver queries the recursive nameservers over UDP, the first that
// answers deciding, as cert-manager's DNS-01 zone discovery does with the
// pod's resolv.conf.
type Resolver struct {
	// Nameservers are host:port addresses.
	Nameservers []string
	Timeout     time.Duration
}

// SystemResolver is the Resolver over the nameservers of /etc/resolv.conf.
func SystemResolver() (*Resolver, error) {
	raw, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil, err
	}
	r := &Resolver{Timeout: 5 * time.Second}
	for _, line := range strings.Split(string(raw), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "nameserver" {
			r.Nameservers = append(r.Nameservers, net.JoinHostPort(f[1], "53"))
		}
	}
	if len(r.Nameservers) == 0 {
		return nil, errors.New("/etc/resolv.conf names no nameserver")
	}
	return r, nil
}

// QuerySOA implements SOAQuerier.
func (r *Resolver) QuerySOA(ctx context.Context, fqdn string) (dnsmessage.RCode, []string, error) {
	name, err := dnsmessage.NewName(fqdn)
	if err != nil {
		return 0, nil, err
	}
	var errs []error
	for _, ns := range r.Nameservers {
		rcode, zones, err := r.query(ctx, ns, name)
		if err == nil {
			return rcode, zones, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", ns, err))
	}
	return 0, nil, errors.Join(errs...)
}

func (r *Resolver) query(ctx context.Context, ns string, name dnsmessage.Name) (dnsmessage.RCode, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	id := uint16(time.Now().UnixNano())
	msg := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: id, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeSOA, Class: dnsmessage.ClassINET}},
	}
	query, err := msg.Pack()
	if err != nil {
		return 0, nil, err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", ns)
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close() //nolint:errcheck // a UDP socket's close reports nothing to act on
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return 0, nil, err
		}
	}
	if _, err := conn.Write(query); err != nil {
		return 0, nil, err
	}
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return 0, nil, err
		}
		var p dnsmessage.Parser
		h, err := p.Start(buf[:n])
		if err != nil || h.ID != id || !h.Response {
			continue // not the answer to this query
		}
		if err := p.SkipAllQuestions(); err != nil {
			return 0, nil, err
		}
		var zones []string
		for {
			ah, err := p.AnswerHeader()
			if errors.Is(err, dnsmessage.ErrSectionDone) {
				break
			}
			if err != nil {
				return 0, nil, err
			}
			if ah.Type == dnsmessage.TypeSOA {
				zones = append(zones, ah.Name.String())
			}
			if err := p.SkipAnswer(); err != nil {
				return 0, nil, err
			}
		}
		return h.RCode, zones, nil
	}
}
