package testrunner

import "telara.dev/tap/internal/model"

// buildAPIOutput turns a fixture record for one api: step's tool into the
// step's resolved output, walking `pages:` (paginate: all) to exhaustion if
// present, else treating a flat `items:` record as a single page.
func buildAPIOutput(rec interface{}) (map[string]interface{}, int) {
	m, ok := model.AsMap(rec)
	if !ok {
		return nil, 0
	}
	if pages, ok := model.AsSlice(m["pages"]); ok {
		var items []interface{}
		var lastPagination interface{}
		for _, pv := range pages {
			pm, ok := model.AsMap(pv)
			if !ok {
				continue
			}
			if pi, ok := model.AsSlice(pm["items"]); ok {
				items = append(items, pi...)
			}
			lastPagination = pm["_pagination"]
		}
		return map[string]interface{}{"items": items, "_pagination": lastPagination}, len(pages)
	}
	return m, 1
}

// lookupBrowserFixture resolves the fixture record for a browser: step,
// honoring `fixture_variant:` (looked up as `variant_<name>`), then the
// step id, then the generic "browser_extract" convention used by the
// web-changelog-watch example, then -- as a last resort for
// single-browser-step packages -- whichever entry looks browser-shaped.
func lookupBrowserFixture(fixtures map[string]interface{}, stepID, variant string) (map[string]interface{}, bool) {
	if variant != "" {
		if v, ok := fixtures["variant_"+variant]; ok {
			m, ok := model.AsMap(v)
			return m, ok
		}
	}
	if v, ok := fixtures[stepID]; ok {
		if m, ok := model.AsMap(v); ok {
			return m, true
		}
	}
	if v, ok := fixtures["browser_extract"]; ok {
		if m, ok := model.AsMap(v); ok {
			return m, true
		}
	}
	for _, v := range fixtures {
		m, ok := model.AsMap(v)
		if !ok {
			continue
		}
		if _, has := m["entries_raw"]; has {
			return m, true
		}
		if _, has := m["drift"]; has {
			return m, true
		}
	}
	return nil, false
}
