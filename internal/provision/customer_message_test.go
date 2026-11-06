package provision

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gpuai-dev/gpuai-oss/internal/privacy"
	"github.com/gpuai-dev/gpuai-oss/internal/provider"
)

// The two supplier-unfunded strings customers actually saw on 2026-09-22.
const (
	prodMarketUnfunded = `provision: all providers failed (last: marketco): create instance: HTTP 400: {"error":"insufficient_credit","msg":"Your account lacks credit; see the billing page."}`
	prodPodUnfunded    = `provision: all providers failed (last: podco): create pod: graphql error: Your account balance is too low to rent a pod`
)

func TestCustomerFailureMessageFromText_Classes(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantClass FailureClass
		wantMsg   string // "" = don't pin the exact sentence
	}{
		{"marketco insufficient_credit", prodMarketUnfunded, FailureSupplierUnfunded, MsgSupplierUnfunded},
		{"podco balance too low", prodPodUnfunded, FailureSupplierUnfunded, MsgSupplierUnfunded},
		{"typed unfunded sentinel text", "provision: all providers failed (last: vmco): supplier account unfunded", FailureSupplierUnfunded, MsgSupplierUnfunded},
		{"http 402", "create instance: HTTP 402: payment required", FailureSupplierUnfunded, MsgSupplierUnfunded},
		{"no capacity", "provision: all providers failed (last: marketco): " + provider.ErrNoCapacity.Error(), FailureCapacity, MsgCapacity},
		{"no provider", ErrNoProvider.Error(), FailureCapacity, MsgCapacity},
		{"upstream quota", "launch: " + provider.ErrQuotaExceeded.Error(), FailureCapacity, MsgCapacity},
		{"all providers failed, opaque", "provision: all providers failed (last: metalco): create: HTTP 500: {\"detail\":\"boom\"}", FailureCapacity, MsgCapacity},
		{"price exceeded", "provision: current price 2.5000/hr exceeds maximum 2.1000/hr", FailurePriceChanged,
			"The price for this offering moved to $2.50/hr, above the $2.10/hr this launch was approved at, so nothing was launched. Check the current price and launch again to accept it."},
		{"price sentinel only", ErrPriceExceeded.Error(), FailurePriceChanged, MsgPriceChanged},
		{"offering unavailable", "provision: " + ErrOfferingUnavailable.Error(), FailureOwnSentence, "Selected machine is no longer available — pick another offering."},
		{"ssh keys", "provision: " + ErrSSHKeysNotFound.Error(), FailureOwnSentence, "No SSH keys found. Add an SSH key before launching an instance."},
		{"customer's own empty balance is NOT supplier-unfunded", "provision: insufficient balance to launch this instance", FailureGeneric, MsgGeneric},
		{"opaque", "engine: synthetic failure", FailureGeneric, MsgGeneric},
		{"empty", "", FailureGeneric, MsgGeneric},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, msg := CustomerFailureMessageFromText(tc.raw)
			if class != tc.wantClass {
				t.Errorf("class = %q, want %q (msg %q)", class, tc.wantClass, msg)
			}
			if tc.wantMsg != "" && msg != tc.wantMsg {
				t.Errorf("msg = %q\nwant  %q", msg, tc.wantMsg)
			}
		})
	}
}

func TestCustomerFailureMessage_TypedPriceError(t *testing.T) {
	err := fmt.Errorf("provision: %w", &PriceExceededError{CurrentBestPerHour: 3, MaxPricePerHour: 2.5})
	class, msg := CustomerFailureMessage(err)
	if class != FailurePriceChanged || !strings.Contains(msg, "$3.00/hr") || !strings.Contains(msg, "$2.50/hr") {
		t.Fatalf("got %q %q", class, msg)
	}
	if class, _ := CustomerFailureMessage(nil); class != FailureGeneric {
		t.Fatalf("nil err: got %q", class)
	}
	if class, msg := CustomerFailureMessage(errors.New(prodMarketUnfunded)); class != FailureSupplierUnfunded || msg != MsgSupplierUnfunded {
		t.Fatalf("unfunded: got %q %q", class, msg)
	}
}

// The unfunded sentence must say it is NOT the customer's balance — the whole
// point of the 2026-09-22 fix.
func TestMsgSupplierUnfunded_SaysNotYourBalance(t *testing.T) {
	if !strings.Contains(MsgSupplierUnfunded, "not your balance") {
		t.Fatalf("unfunded message lost its reassurance: %q", MsgSupplierUnfunded)
	}
}

// registryProviderNames returns every provider name the registry can hold:
// each adapter directory under internal/provider/ (the registry Names() set —
// the same walk privacy.TestProviderBrandTokensCoverEveryAdapterDir does) plus
// the privacy brand list and the two internal registry names.
func registryProviderNames(t *testing.T) []string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	providerDir := filepath.Join(filepath.Dir(file), "..", "provider")
	entries, err := os.ReadDir(providerDir)
	if err != nil {
		t.Fatalf("read %s: %v", providerDir, err)
	}
	seen := map[string]bool{}
	var names []string
	add := func(n string) {
		n = strings.ToLower(n)
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	adapters := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(providerDir, e.Name(), "adapter.go")); err != nil {
			continue
		}
		adapters++
		add(e.Name())
	}
	if adapters == 0 {
		t.Fatalf("found no provider adapters under %s — the leak guard would be vacuous", providerDir)
	}
	for _, tok := range privacy.ProviderBrandTokens {
		add(tok)
	}
	add("generic")
	add("community")
	return names
}

// TestCustomerFailureMessage_NoProviderNameLeaks feeds every realistic engine
// failure shape, for every provider name in the registry, through the
// customer renderer and asserts no name, raw body, or HTTP status survives.
func TestCustomerFailureMessage_NoProviderNameLeaks(t *testing.T) {
	shapes := []string{
		"provision: all providers failed (last: %s): create instance: HTTP 400: {\"error\":\"insufficient_credit\",\"msg\":\"Your account lacks credit; see the billing page.\"}",
		"provision: all providers failed (last: %s): Your account balance is too low to rent a pod",
		"provision: all providers failed (last: %s): no GPU capacity available",
		"no matching offers: %s.ai: no GPU capacity available",
		"provision: all providers failed (last: %s): create: HTTP 503: <html>upstream %s gateway</html>",
		"%s: something nobody has classified yet",
		"provision: %s: " + ErrOfferingUnavailable.Error(),
		"provision: %s: " + ErrOSUnavailable.Error(),
		"provision: %s: current price 4.0000/hr exceeds maximum 3.0000/hr",
	}
	for _, name := range registryProviderNames(t) {
		for _, shape := range shapes {
			raw := strings.ReplaceAll(shape, "%s", name)
			for _, variant := range []string{raw, strings.ToUpper(raw[:1]) + raw[1:], strings.ReplaceAll(raw, name, strings.ToUpper(name))} {
				_, msg := CustomerFailureMessageFromText(variant)
				lower := strings.ToLower(msg)
				if strings.Contains(lower, name) {
					t.Errorf("provider %q leaked into customer message %q (raw %q)", name, msg, variant)
				}
				for _, bad := range []string{"http 4", "http 5", "{", "<html", "insufficient_credit", "balance is too low", "lacks credit", "all providers failed"} {
					if strings.Contains(lower, bad) {
						t.Errorf("raw upstream fragment %q leaked into customer message %q (raw %q)", bad, msg, variant)
					}
				}
			}
		}
	}
}
