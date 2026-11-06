// Package imageref parses and validates CUSTOMER-SUPPLIED container image
// references. It is the single trust gate between the untrusted `image` string
// on the public create-instance API and every downstream context that string
// reaches: provider create calls (e.g. the generic adapter's
// docker_image), the rendered bootstrap script (a shell context), and the
// registry pre-flight client (an outbound HTTP dial).
//
// The grammar is the docker/distribution reference grammar, implemented
// strictly and by hand (no new dependency): the accepted charset is so narrow
// that everything a shell or TOML/JSON renderer could care about (quotes,
// spaces, `$`, backticks, `;`, newlines) is structurally unrepresentable in a
// valid reference. Downstream escaping is therefore defense in depth, not the
// primary control — parity with how tunnel.ValidateBootstrapData treats
// server-controlled refs (V5: this package is what makes a CUSTOMER string
// safe enough to enter that pipeline at all).
package imageref

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// MaxRefLen bounds the whole reference. The distribution spec allows 255 for
// the repository alone; we bound the full ref (host + path + tag + digest)
// well under any provider's limit while leaving room for real-world refs.
const MaxRefLen = 384

// Ref is a validated, normalized image reference.
type Ref struct {
	// Registry is the registry host[:port], normalized: bare Docker Hub
	// shorthands ("ubuntu", "user/repo") resolve to "docker.io".
	Registry string
	// Repository is the path within the registry, normalized: Docker Hub
	// single-component names gain the "library/" prefix.
	Repository string
	// Tag is the explicit tag, or "latest" when neither tag nor digest was
	// given. Empty when (and only when) Digest is set and no tag was given.
	Tag string
	// Digest is the "sha256:<64 hex>" digest when the ref was digest-pinned.
	Digest string
}

var (
	// pathComponentRe: lowercase alphanumerics joined by ".", one or two "_",
	// or any run of "-" (docker/distribution path-component grammar).
	pathComponentRe = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*$`)
	tagRe           = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	digestRe        = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	// hostRe: DNS-ish registry host, optionally with a port. Deliberately
	// permits uppercase nowhere and IP literals only in dotted-quad form —
	// IPv6 bracket literals are rejected outright (no legitimate public
	// registry is addressed that way by customers, and it closes a whole
	// class of parser ambiguity).
	hostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*(:[0-9]{1,5})?$`)
)

// Parse validates raw as a customer image reference and returns its
// normalized form. Errors are customer-facing (safe to return in a 400).
func Parse(raw string) (Ref, error) {
	if raw == "" {
		return Ref{}, fmt.Errorf("image must not be empty")
	}
	if len(raw) > MaxRefLen {
		return Ref{}, fmt.Errorf("image must be at most %d characters", MaxRefLen)
	}
	// The full-charset pre-check makes every later error message safe to
	// echo and guarantees no whitespace/control/shell bytes survive.
	for _, r := range raw {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			continue
		}
		switch r {
		case '.', '-', '_', '/', ':', '@':
		default:
			return Ref{}, fmt.Errorf("image contains invalid character %q", r)
		}
	}

	rest := raw
	var digest string
	if at := strings.IndexByte(rest, '@'); at >= 0 {
		digest = rest[at+1:]
		rest = rest[:at]
		if !digestRe.MatchString(digest) {
			return Ref{}, fmt.Errorf("image digest must be sha256:<64 hex characters>")
		}
	}

	var tag string
	// A ':' after the last '/' is a tag separator; before it, a port.
	if colon := strings.LastIndexByte(rest, ':'); colon > strings.LastIndexByte(rest, '/') {
		tag = rest[colon+1:]
		rest = rest[:colon]
		if !tagRe.MatchString(tag) {
			return Ref{}, fmt.Errorf("image tag %q is invalid", tag)
		}
	}
	if rest == "" {
		return Ref{}, fmt.Errorf("image repository must not be empty")
	}

	// Split registry host from repository path: the first component is a
	// registry iff it contains '.' or ':' or is "localhost" (docker's rule).
	registry := "docker.io"
	repo := rest
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		// Hostnames are case-insensitive: normalize before matching, so
		// GHCR.io works. The lowercased form is also what the first-component
		// registry test and every downstream consumer see.
		first := strings.ToLower(rest[:slash])
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			registry = first
			repo = rest[slash+1:]
		}
	}
	if registry != "docker.io" {
		if !hostRe.MatchString(registry) {
			return Ref{}, fmt.Errorf("image registry host %q is invalid", registry)
		}
		if colon := strings.LastIndexByte(registry, ':'); colon >= 0 {
			if port, err := strconv.Atoi(registry[colon+1:]); err != nil || port < 1 || port > 65535 {
				return Ref{}, fmt.Errorf("image registry port in %q is invalid", registry)
			}
		}
	}
	if err := checkRegistryHost(registry); err != nil {
		return Ref{}, err
	}

	if repo == "" {
		return Ref{}, fmt.Errorf("image repository must not be empty")
	}
	if registry == "docker.io" && !strings.ContainsRune(repo, '/') {
		repo = "library/" + repo
	}
	for _, comp := range strings.Split(repo, "/") {
		if !pathComponentRe.MatchString(comp) {
			return Ref{}, fmt.Errorf("image repository component %q is invalid (lowercase letters, digits, and separators only)", comp)
		}
	}
	if tag == "" && digest == "" {
		tag = "latest"
	}
	return Ref{Registry: registry, Repository: repo, Tag: tag, Digest: digest}, nil
}

// checkRegistryHost rejects registry hosts that are structurally internal:
// localhost, IP literals in private/loopback/link-local ranges, and
// single-label hostnames (no dot ⇒ only resolvable on an internal search
// domain). This is the STATIC half of the SSRF guard — the registry pre-flight
// client re-checks the RESOLVED address at dial time, because a public DNS
// name can still point at a private IP.
func checkRegistryHost(host string) error {
	bare := host
	if colon := strings.LastIndexByte(bare, ':'); colon >= 0 {
		bare = bare[:colon]
	}
	if bare == "localhost" {
		return fmt.Errorf("image registry host %q is not allowed", host)
	}
	if ip := net.ParseIP(bare); ip != nil {
		if DisallowedRegistryIP(ip) {
			return fmt.Errorf("image registry host %q is not allowed", host)
		}
		return nil
	}
	if !strings.ContainsRune(bare, '.') {
		return fmt.Errorf("image registry host %q is not allowed (must be a fully qualified domain)", host)
	}
	return nil
}

// specialV4Ranges are IPv4 ranges that are not private/loopback/link-local
// (so net.IP's own predicates pass them) yet must never be dialed for a
// customer-controlled registry: CGNAT (100.64/10 — used as a secondary VPC
// CIDR by some clouds), the documentation/benchmark blocks, and class E.
var specialV4Ranges = func() []*net.IPNet {
	out := make([]*net.IPNet, 0, 6)
	for _, cidr := range []string{
		"100.64.0.0/10",   // CGNAT
		"192.0.2.0/24",    // TEST-NET-1
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"198.18.0.0/15",   // benchmark
		"240.0.0.0/4",     // class E
	} {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(err) // static table; unreachable
		}
		out = append(out, n)
	}
	return out
}()

// DisallowedRegistryIP reports whether ip may NOT be used as a registry
// address — the shared verdict behind both the static host check here and
// registrycheck's dial-time guard (which re-checks the RESOLVED address,
// because a public DNS name can still point at a private IP).
func DisallowedRegistryIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		for _, n := range specialV4Ranges {
			if n.Contains(v4) {
				return true
			}
		}
	}
	return false
}

// NormalizeRegistryHost validates a BARE registry host (as stored on a
// registry credential) under the same grammar and static SSRF rules a host
// inside an image reference obeys, and returns its lowercased form. "docker.io"
// is accepted as-is (the Docker Hub sentinel).
func NormalizeRegistryHost(host string) (string, error) {
	h := strings.ToLower(host)
	if h == "docker.io" {
		return h, nil
	}
	if !hostRe.MatchString(h) {
		return "", fmt.Errorf("registry host %q is invalid", host)
	}
	if colon := strings.LastIndexByte(h, ':'); colon >= 0 {
		if port, err := strconv.Atoi(h[colon+1:]); err != nil || port < 1 || port > 65535 {
			return "", fmt.Errorf("registry port in %q is invalid", host)
		}
	}
	if err := checkRegistryHost(h); err != nil {
		return "", err
	}
	return h, nil
}

// String renders the canonical pull reference: digest-pinned when a digest is
// known, tagged otherwise. Docker Hub refs render WITHOUT the docker.io/
// prefix (and without library/) so providers that special-case Hub shorthand
// (all of them) see the familiar form.
func (r Ref) String() string {
	var b strings.Builder
	if r.Registry != "docker.io" {
		b.WriteString(r.Registry)
		b.WriteByte('/')
		b.WriteString(r.Repository)
	} else {
		b.WriteString(strings.TrimPrefix(r.Repository, "library/"))
	}
	if r.Digest != "" {
		if r.Tag != "" && r.Tag != "latest" {
			b.WriteByte(':')
			b.WriteString(r.Tag)
		}
		b.WriteByte('@')
		b.WriteString(r.Digest)
		return b.String()
	}
	b.WriteByte(':')
	b.WriteString(r.Tag)
	return b.String()
}

// Pinned returns the canonical reference with the digest forced to d (the
// pre-flight-resolved manifest digest). Used to launch exactly the bytes the
// pre-flight validated regardless of tag movement in between (TOCTOU).
func (r Ref) Pinned(d string) string {
	p := r
	p.Digest = d
	return p.String()
}
