package provision

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/gpuai-dev/gpuai-oss/internal/privacy"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// customer_message.go — the customer-facing rendering of a provisioning
// failure (2026-09-22 incident).
//
// The engine's error strings are written for operators: they carry the
// upstream supplier's name ("all providers failed (last: <supplier>)") and, often,
// the supplier's raw HTTP body ("HTTP 400: {\"error\":\"insufficient_credit\",
// \"msg\":\"Your account lacks credit…\"}"). Surfaced verbatim, the second half
// reads as if the CUSTOMER's balance is empty — customers with money in their
// account concluded exactly that — and the first half names a supplier we must
// never expose (INFPRIV-01..06, API-09).
//
// privacy.ScrubProviderNames alone is not enough: it removes the brand but
// keeps the misleading supplier body. So this file does not scrub the raw
// string — it REPLACES it with one of a fixed set of sentences chosen by
// failure class, and only ever passes through text that is our own (the
// engine's sentinel sentences), which is still scrubbed as a backstop.
//
// The raw error is never discarded: callers keep it in the log line and the
// internal DB column (operations.error_detail) and render this message only at
// the customer boundary. CustomerFailureMessageFromText exists precisely so the
// render can happen at READ time over already-stored rows, which also fixes
// every historical operation without a backfill.

// FailureClass is the coarse bucket a provisioning failure is shown to a
// customer as. Stable strings — tests and callers switch on them.
type FailureClass string

const (
	// FailureSupplierUnfunded — an upstream supplier refused the launch
	// because OUR account with it is out of funds/credit. Must never read as
	// the customer's balance.
	FailureSupplierUnfunded FailureClass = "supplier_unfunded"
	// FailureCapacity — no upstream capacity could be placed (drained,
	// ghosted, every candidate refused).
	FailureCapacity FailureClass = "capacity"
	// FailurePriceChanged — the live price moved above the approved ceiling.
	FailurePriceChanged FailureClass = "price_changed"
	// FailureOwnSentence — the engine refused with one of our own
	// customer-worded sentences (spend limits, SSH keys, disk, environment…);
	// the sentence is passed through.
	FailureOwnSentence FailureClass = "own_sentence"
	// FailureGeneric — anything else.
	FailureGeneric FailureClass = "generic"
)

// Customer-facing sentences. Deliberately contain no supplier name, no HTTP
// status, and no upstream body.
const (
	MsgSupplierUnfunded = "No capacity could be placed for this offering right now. This is on our side, not your balance — nothing is wrong with your account. Please try again shortly or pick another offering."
	MsgCapacity         = "No capacity could be placed for this offering right now. Please try again shortly or pick another offering."
	MsgPriceChanged     = "The price for this offering moved above the price this launch was approved at, so nothing was launched. Check the current price and launch again to accept it."
	MsgGeneric          = "The launch could not be completed on our side. Please try again shortly or pick another offering; if it keeps failing, contact support with the operation ID."
)

// supplierUnfundedMarkers are lower-cased substrings that identify an upstream
// supplier refusing a launch because OUR account with it lacks funds. Seen in
// prod: "insufficient_credit" / "Your account lacks credit" and "Your account
// balance is too low to rent a pod" from marketplace suppliers. "account unfunded" / "unfunded"
// covers a typed sentinel for this class. Kept specific on purpose: a
// customer-side "insufficient balance" message must not land here — hence no
// generic "insufficient balance" marker.
var supplierUnfundedMarkers = []string{
	"insufficient_credit",
	"insufficient credit",
	"lacks credit",
	"balance is too low",
	"balance too low",
	"insufficient funds",
	"insufficient_funds",
	"not enough credit",
	"out of credit",
	"unfunded",
	"payment required",
	"http 402",
}

// capacityMarkers identify an exhausted placement. The sentinel texts are
// matched as substrings because a stored error is a flattened %w chain.
var capacityMarkers = []string{
	strings.ToLower(provider.ErrNoCapacity.Error()),
	strings.ToLower(provider.ErrQuotaExceeded.Error()),
	strings.ToLower(ErrNoProvider.Error()),
	"all providers failed",
	"out of stock",
	"no capacity",
	"no gpu capacity",
	"no offers",
	"no matching offers",
}

// ownSentenceErrors are engine sentinels whose text is written for the
// customer (actionable, no upstream detail). When one appears in a failure
// chain its sentence is the most useful thing to show, so it wins over the
// capacity/generic buckets. ErrOfferingUnavailable is deliberately here, not in
// capacity: its sentence already tells the customer what to do.
var ownSentenceErrors = []struct {
	err error
	msg string // "" = the sentinel's own text, capitalised
}{
	{ErrOfferingUnavailable, ""},
	{ErrCertifiedUnavailable, ""},
	{ErrFrameworkUnavailable, ""},
	{ErrOSUnavailable, ""},
	{ErrDiskUnavailable, ""},
	{ErrDiskExceedsOffering, ""},
	{ErrImageEntrypointUnavailable, ""},
	{ErrCustomImageSpotUnsupported, ""},
	{ErrSSHKeysNotFound, "No SSH keys found. Add an SSH key before launching an instance."},
	{ErrSSHKeyOrgMismatch, "One or more SSH keys do not belong to your organization. Use keys created under your active organization."},
	{ErrSSHKeyForbidden, "One or more SSH keys belong to another member of your organization. Use keys you own."},
}

var (
	// PriceExceededError.Error(): "current price %.4f/hr exceeds maximum %.4f/hr".
	priceExceededRe = regexp.MustCompile(`current price ([0-9]+(?:\.[0-9]+)?)/hr exceeds maximum ([0-9]+(?:\.[0-9]+)?)/hr`)
	// TemplateVRAMError.Error() — "template X requires at least N GB of VRAM per GPU; T has M GB…".
	templateVRAMRe = regexp.MustCompile(`template \S+ requires at least [0-9]+ GB of VRAM per GPU[^\n]*`)
)

// CustomerFailureMessage renders a provisioning error for a customer, using
// the typed chain where available and falling back to the text classifier.
func CustomerFailureMessage(err error) (FailureClass, string) {
	if err == nil {
		return FailureGeneric, MsgGeneric
	}
	var pe *PriceExceededError
	if errors.As(err, &pe) {
		return FailurePriceChanged, priceChangedMessage(pe.CurrentBestPerHour, pe.MaxPricePerHour)
	}
	return CustomerFailureMessageFromText(err.Error())
}

// CustomerFailureMessageFromText classifies a flattened (stored) engine error
// string and returns the customer-safe sentence for it. The result never
// contains a provider brand (privacy.ScrubProviderNames runs over the
// passthrough branches as a backstop) and never contains upstream bodies.
func CustomerFailureMessageFromText(raw string) (FailureClass, string) {
	lower := strings.ToLower(raw)

	// Supplier-unfunded first: it is the class most likely to be misread, and
	// it arrives wrapped in "all providers failed", which would otherwise
	// match capacity.
	for _, m := range supplierUnfundedMarkers {
		if strings.Contains(lower, m) {
			return FailureSupplierUnfunded, MsgSupplierUnfunded
		}
	}

	if m := priceExceededRe.FindStringSubmatch(raw); m != nil {
		cur, err1 := strconv.ParseFloat(m[1], 64)
		max, err2 := strconv.ParseFloat(m[2], 64)
		if err1 == nil && err2 == nil {
			return FailurePriceChanged, priceChangedMessage(cur, max)
		}
		return FailurePriceChanged, MsgPriceChanged
	}
	if strings.Contains(lower, strings.ToLower(ErrPriceExceeded.Error())) {
		return FailurePriceChanged, MsgPriceChanged
	}

	for _, e := range ownSentenceErrors {
		if strings.Contains(raw, e.err.Error()) {
			if e.msg != "" {
				return FailureOwnSentence, e.msg
			}
			return FailureOwnSentence, sentence(e.err.Error())
		}
	}
	if s := templateVRAMRe.FindString(raw); s != "" {
		return FailureOwnSentence, sentence(s)
	}

	for _, m := range capacityMarkers {
		if strings.Contains(lower, m) {
			return FailureCapacity, MsgCapacity
		}
	}
	return FailureGeneric, MsgGeneric
}

func priceChangedMessage(current, approved float64) string {
	if current <= 0 || approved <= 0 {
		return MsgPriceChanged
	}
	return fmt.Sprintf("The price for this offering moved to $%.2f/hr, above the $%.2f/hr this launch was approved at, so nothing was launched. Check the current price and launch again to accept it.", current, approved)
}

// sentence scrubs, capitalises and full-stops one of our own engine sentences.
func sentence(s string) string {
	s = strings.TrimSpace(privacy.ScrubProviderNames(s))
	if s == "" {
		return MsgGeneric
	}
	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}
