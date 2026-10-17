package cmd

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// ── Which moves can touch the events table ───────────────────────────────────
//
// The safety machinery of kube_reset.go (asking ClickHouse in its pod, the
// lease, the reset record, the check that asks ClickHouse again afterwards)
// runs only on a move that can touch the events table: one that takes the
// data-plane images across the storage reset (a pre-reset release to a new one
// or the other way round, or images that cannot be told), or while a reset is
// recorded as unfinished. Every other upgrade, rollback and deploy behaves as
// it did before the reset existed: it reads helm's own state, rolls the
// release, and looks only at the images the Kubernetes objects run.

// firstJSONChart is the first published chart whose images write the new
// (JSON) events table. Chart 0.1.18 was never published (helm cannot pull
// it), although its images exist; messages name this one.
const firstJSONChart = "0.1.19"

// ── helm --set ───────────────────────────────────────────────────────────────

// setPair is one key=value of a --set flag, as helm reads it.
type setPair struct {
	path  []string
	value string
}

// parseHelmSets reads --set flags the way helm does: one flag can hold
// several key=value pairs separated by commas (a comma escaped with a
// backslash, or inside braces, belongs to its value), and the dots of a key
// separate its path (a backslashed dot belongs to a name). A pair without "="
// is left for helm to refuse.
func parseHelmSets(sets []string) []setPair {
	var pairs []setPair
	for _, s := range sets {
		for _, item := range splitSetItems(s) {
			k, v, ok := strings.Cut(item, "=")
			if !ok {
				continue
			}
			pairs = append(pairs, setPair{path: splitSetKey(k), value: unescapeSet(v)})
		}
	}
	return pairs
}

// splitSetItems splits one --set value on its commas, keeping the escapes.
func splitSetItems(s string) []string {
	var items []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				items = append(items, s[start:i])
				start = i + 1
			}
		}
	}
	return append(items, s[start:])
}

// splitSetKey splits a key on its unescaped dots.
func splitSetKey(k string) []string {
	var path []string
	var cur strings.Builder
	for i := 0; i < len(k); i++ {
		switch {
		case k[i] == '\\' && i+1 < len(k):
			i++
			cur.WriteByte(k[i])
		case k[i] == '.':
			path = append(path, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(k[i])
		}
	}
	return append(path, cur.String())
}

// unescapeSet drops the backslashes that escape a character in a value.
func unescapeSet(v string) string {
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] == '\\' && i+1 < len(v) {
			i++
		}
		b.WriteByte(v[i])
	}
	return b.String()
}

// setNull marks a --set value of null, which removes the key.
type setNull struct{}

// typedSetValue is a --set value as helm types it: true and false (any case)
// are booleans, null removes the key, anything else is kept as a string.
func typedSetValue(v string) any {
	switch {
	case strings.EqualFold(v, "true"):
		return true
	case strings.EqualFold(v, "false"):
		return false
	case strings.EqualFold(v, "null"):
		return setNull{}
	}
	return v
}

// effectiveValues is vals with the --set flags applied over them (a copy),
// as helm would merge them: the values the release will have after the run.
func effectiveValues(vals map[string]any, sets []string) map[string]any {
	out := map[string]any{}
	if b, err := json.Marshal(vals); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	for _, p := range parseHelmSets(sets) {
		setValuePath(out, p.path, typedSetValue(p.value))
	}
	return out
}

func setValuePath(root map[string]any, path []string, v any) {
	for i, k := range path {
		if i == len(path)-1 {
			if _, null := v.(setNull); null {
				delete(root, k)
			} else {
				root[k] = v
			}
			return
		}
		next, ok := root[k].(map[string]any)
		if !ok {
			if _, null := v.(setNull); null {
				return
			}
			next = map[string]any{}
			root[k] = next
		}
		root = next
	}
}

// valueAt reads the value at path in vals ("" or nil when there is none).
func valueAt(vals map[string]any, path ...string) any {
	var cur any = vals
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

func stringAt(vals map[string]any, path ...string) string {
	s, _ := valueAt(vals, path...).(string)
	return s
}

// ofServices reports whether the values key key is one of services (the
// values keys of the data-plane images, serviceImageKeys); with none known
// (nil), every key is taken to be.
func ofServices(key string, services []string) bool {
	return services == nil || slices.Contains(services, key)
}

// digestPins are the values keys of the data-plane services (services) pinned
// by image.digest, which the chart renders as repository@digest and for which
// it ignores the tag: neither the chart version nor an image.tag decides what
// they run. A datastore's pin is not one.
func digestPins(vals map[string]any, services []string) []string {
	var keys []string
	for k := range vals {
		if ofServices(k, services) && stringAt(vals, k, "image", "digest") != "" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

// ── Images ───────────────────────────────────────────────────────────────────

// imageSet is what a set of data-plane image references write: the tags of
// those with a tag, and the references pinned by digest alone, which name no
// release.
type imageSet struct {
	tags    []string
	digests []string
}

// classifyImages sorts data-plane image references by their tag.
func classifyImages(images []string) imageSet {
	var s imageSet
	for _, img := range images {
		if tag := imageTag(img); tag != "" {
			if !slices.Contains(s.tags, tag) {
				s.tags = append(s.tags, tag)
			}
		} else if !slices.Contains(s.digests, img) {
			s.digests = append(s.digests, img)
		}
	}
	slices.Sort(s.tags)
	slices.Sort(s.digests)
	return s
}

// genOfTags is the events table generation the images tagged tags write:
// "string" or "json" when they all write one, "mixed" when they write both
// (numbered releases on either side of the storage reset), "unknown" when one
// has a tag that is not a numbered release (main, sha-…), "" with no tags.
func genOfTags(tags []string) string {
	gens := map[string]bool{}
	for _, t := range tags {
		gens[releaseSchemaGeneration(t)] = true
	}
	switch {
	case len(gens) == 0:
		return ""
	case gens["unknown"]:
		return "unknown"
	case len(gens) > 1:
		return "mixed"
	}
	for g := range gens {
		return g
	}
	return ""
}

// numberedGen is gen when it names an events table ("string", "json") and ""
// otherwise (unknown, mixed, none): what a check can hold the images to.
func numberedGen(gen string) string {
	if gen == "string" || gen == "json" {
		return gen
	}
	return ""
}

// gen is the generation the tagged images write (genOfTags).
func (s imageSet) gen() string { return genOfTags(s.tags) }

// manifestImageSet reads the data-plane images of a rendered release manifest
// (helm get manifest). none says it holds no data-plane image at all (the
// manifest could not be read or is not the chart's).
func manifestImageSet(manifest string) (set imageSet, none bool) {
	var images []string
	for _, m := range reManifestImage.FindAllStringSubmatch(manifest, -1) {
		if img := m[1]; isDataPlaneContainer(kubeContainer{Image: img}) {
			images = append(images, img)
		}
	}
	return classifyImages(images), len(images) == 0
}

// manifestGeneration says which events table the data-plane images of a
// rendered release manifest write: gen, or why that is not one generation
// (none, several, one not known, or only digests).
func manifestGeneration(manifest string) (gen, why string) {
	set, none := manifestImageSet(manifest)
	switch g := set.gen(); {
	case none:
		return "", "it runs no data-plane service"
	case g == "unknown":
		return "", "it runs images whose tag is not a numbered release (" + strings.Join(unnumbered(set.tags), ", ") + "), so which events table they write is not known"
	case g == "mixed":
		return "", "its images write different events tables (tags " + strings.Join(set.tags, ", ") + ")"
	case g == "":
		return "", "it runs its data-plane images by digest alone (" + strings.Join(set.digests, ", ") + "), which names no release"
	default:
		return g, ""
	}
}

// unnumbered are the tags that are not numbered releases or the edge
// channel's.
func unnumbered(tags []string) []string {
	return slices.DeleteFunc(slices.Clone(tags), func(t string) bool { return releaseSchemaGeneration(t) != "unknown" })
}

// ── The move ─────────────────────────────────────────────────────────────────

// targetTags are the tags a move gives the data-plane services (services are
// their values keys): the release's, and any per-service image.tag of the
// --set flags (which wins over it). Services of both sides of the storage
// reset ("mixed", see genOfTags) cannot all write the one events table.
func targetTags(imageTag string, sets, services []string) []string {
	tags := []string{imageTag}
	for _, p := range parseHelmSets(sets) {
		if n := len(p.path); n >= 3 && p.path[n-2] == "image" && p.path[n-1] == "tag" && ofServices(p.path[0], services) {
			if t, ok := typedSetValue(p.value).(string); ok && t != "" && !slices.Contains(tags, t) {
				tags = append(tags, t)
			}
		}
	}
	slices.Sort(tags)
	return tags
}

// movesAcrossReset says whether taking the release from installed images to
// images tagged target can touch the events table, and why. Digest-pinned
// services do not move (the digest wins over any tag), so an installed
// release that runs digests alone moves nothing unless a pin is cleared
// (clearedPins); one whose images could not be read (none) is not known to
// be safe.
func movesAcrossReset(installed imageSet, none bool, target []string, clearedPins bool) (bool, string) {
	switch {
	case none:
		return true, "the installed release's images could not be read"
	case clearedPins:
		return true, "a digest pin is cleared, so those services move to the tag of the target"
	case len(installed.tags) == 0:
		return false, ""
	case len(target) == 0:
		return true, "the target runs its images by digest alone, which names no release"
	case slices.Equal(installed.tags, slices.Sorted(slices.Values(target))):
		return false, ""
	}
	ig, tg := installed.gen(), genOfTags(target)
	if ig == tg && (ig == "string" || ig == "json") {
		return false, ""
	}
	switch {
	case ig == "unknown" || ig == "mixed":
		return true, "the installed release runs images that do not write one known events table (" + strings.Join(installed.tags, ", ") + ")"
	case tg == "unknown" || tg == "mixed":
		return true, "the target runs images that do not write one known events table (" + strings.Join(target, ", ") + ")"
	}
	return true, "the images move from the " + genWord(ig) + " events table to the " + genWord(tg) + " one"
}

func genWord(gen string) string {
	if gen == "string" {
		return "pre-reset (String)"
	}
	return "new (JSON)"
}

// ── ClickHouse settings of the run ───────────────────────────────────────────

// clickhouseImageBelow reports whether image names a ClickHouse server older
// than the new events table needs, as its tag says: ok is false for an image
// whose version the tag does not tell (a mirror, latest), which is asked of
// the server when it matters.
func clickhouseImageBelow(image string) (below bool, version string) {
	v, ok := clickHouseImageVersion(image)
	if !ok {
		return false, ""
	}
	return versionLess(v+".0", minClickHouseForJSON+".0"), v
}

// clearsDigestPins reports whether the --set flags clear a digest pin
// (<service>.image.digest= or =null), which the tag then replaces.
func clearsDigestPins(sets, services []string) bool {
	for _, p := range parseHelmSets(sets) {
		if n := len(p.path); n >= 3 && p.path[n-2] == "image" && p.path[n-1] == "digest" && ofServices(p.path[0], services) {
			if v, ok := typedSetValue(p.value).(string); !ok || v == "" {
				return true
			}
		}
	}
	return false
}

// digestPinProblem says why the digest pins left after the run (pins) stop
// a move that has to judge what the services write, "" without any. The tag
// of the release decides nothing for them, and a digest names no release the
// CLI can read.
func digestPinProblem(pins []string) (headline, hint string) {
	if len(pins) == 0 {
		return "", ""
	}
	clear := make([]string, len(pins))
	for i, k := range pins {
		clear[i] = "--set " + k + ".image.digest="
	}
	return fmt.Sprintf("%s pinned by digest (image.digest in the release values), which names no release the CLI can read, so it cannot tell which events table they write.", servicesWord(pins)),
		"The digest also wins over the image tag this upgrade sets. Clear it so the release's tag decides (" + strings.Join(clear, " ") + "), or pin the digests of the target release's images yourself once the storage reset is done."
}

func servicesWord(keys []string) string {
	if len(keys) == 1 {
		return "Service " + keys[0] + " is"
	}
	return "Services " + strings.Join(keys, ", ") + " are"
}
