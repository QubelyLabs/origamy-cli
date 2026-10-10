package cmd

import (
	"fmt"

	"github.com/qubelylabs/origamy-cli/internal/ui"
)

// ── Config signing ───────────────────────────────────────────────────────────
//
// The control plane signs the config it serves; a data plane given the
// control plane's ed25519 PUBLIC key (CONFIG_SIGNING_KEY, chart
// controlPlane.publicKey / existingSecret) rejects config that does not
// verify. Enrollment hands the key out (internal/token: Enrollment.CSK).

// minSigningVersion is the first data-plane release whose images verify the
// signed config-sync poll and whose chart wires the key into every config
// consumer (QubelyLabs/origamy-data-plane#405). Releases before it check the
// signature over a field the control plane never fills, so given the key
// they reject ALL config: the CLI only hands the key to this release or
// newer. #405 is on main, not staging, and 0.1.18 is cut from staging, so
// 0.1.19 is the first release that can carry it. When that release is cut,
// set this to its version if it differs.
const minSigningVersion = "0.1.19"

// signingKeyCopyVar is the Docker .env entry holding the CLI's copy of the
// verify key. The services read CONFIG_SIGNING_KEY only, so the copy is inert
// on a release that cannot verify; `origamy upgrade` moves it into
// CONFIG_SIGNING_KEY once the target release can.
const signingKeyCopyVar = "ORIGAMY_CONFIG_SIGNING_KEY"

// configSigningSecret holds the verify key on Kubernetes (key public-key, the
// chart's controlPlane.existingSecretKeyKey default). It is stored on every
// chart but referenced (controlPlane.existingSecret) only on one that
// verifies; older charts never read it.
const configSigningSecret = "origamy-config-signing"

// signingSupported reports whether a data-plane release verifies signed
// config. Unlike featureGate, a non-semver release (edge "staging", "main")
// does NOT pass: a wrong guess rejects all config, so the key is only
// applied to a numbered release known to carry the fix.
func signingSupported(release string) bool {
	_, ok := parseVersion(release)
	return ok && !versionLess(release, minSigningVersion)
}

// requireSigningProblem explains why --require-signed-config cannot apply to
// release ("" when it can). haveKey says a verify key is known for the plane:
// strict mode without one makes every service refuse to start.
func requireSigningProblem(release string, haveKey bool) string {
	if !signingSupported(release) {
		return fmt.Sprintf("--require-signed-config needs data-plane release %s or newer (targeting %s).", minSigningVersion, release)
	}
	if !haveKey {
		return "--require-signed-config needs the control plane's config signing key, and none is known for this data plane."
	}
	return ""
}

// dockerSigningEnv returns the .env entries for a Docker plane on release:
// the CLI's copy of key always, CONFIG_SIGNING_KEY only when the release
// verifies (blank otherwise, so a downgrade drops it).
func dockerSigningEnv(key, release string) map[string]string {
	active := ""
	if signingSupported(release) {
		active = key
	}
	return map[string]string{signingKeyCopyVar: key, "CONFIG_SIGNING_KEY": active}
}

// signingSetArgs returns the helm --set arguments for config signing: wire
// points the chart at configSigningSecret, require turns on strict mode.
// Callers gate both on signingSupported.
func signingSetArgs(wire, require bool) []string {
	var args []string
	if wire {
		args = append(args, "--set", "controlPlane.existingSecret="+configSigningSecret)
	}
	if require {
		args = append(args, "--set", "controlPlane.signingRequired=true")
	}
	return args
}

// signingConfigured reports whether a release's values already wire config
// signing (a key, a key Secret, or strict mode), whether set by the CLI, the
// dashboard's Helm snippet or by hand.
func signingConfigured(vals map[string]any) bool {
	cp, _ := vals["controlPlane"].(map[string]any)
	if cp == nil {
		return false
	}
	key, _ := cp["publicKey"].(string)
	secret, _ := cp["existingSecret"].(string)
	required, _ := cp["signingRequired"].(bool)
	return key != "" || secret != "" || required
}

// signingSummary is the deploy summary's config-signing line ("" when the
// control plane handed out no key).
func signingSummary(haveKey, verifying, required bool) string {
	switch {
	case !haveKey:
		return ""
	case !verifying:
		return "signing key stored; verified from release " + minSigningVersion + " (origamy upgrade)"
	case required:
		return ui.Green("signature-verified") + " (required)"
	default:
		return ui.Green("signature-verified")
	}
}
