package manager

import (
	"fmt"
	"strings"

	"github.com/knadh/listmonk/models"
)

// filterSubscribersByAttribs filters subscribers whose attribs match the
// campaign's subscriberFilter conditions.
func filterSubscribersByAttribs(subs []models.Subscriber, campAttribs models.JSON) []models.Subscriber {
	if campAttribs == nil {
		return subs
	}
	filterRaw, ok := campAttribs["subscriberFilter"]
	if !ok {
		return subs
	}
	filter, ok := filterRaw.(map[string]any)
	if !ok || len(filter) == 0 {
		return subs
	}

	filtered := make([]models.Subscriber, 0, len(subs))
	for _, s := range subs {
		if matchAttribs(s.Attribs, filter) {
			filtered = append(filtered, s)
		}
	}
	return filtered
}

// matchAttribs checks if subscriber attribs match all filter conditions.
//
// Supported filter syntax:
//
//	{"segment": "hot"}                → include only segment="hot"
//	{"segment": ["hot", "warm"]}      → include segment "hot" OR "warm"
//	{"!segment": ["cold", "cool"]}    → exclude segment "cold" or "cool"
//	{"!segment": "cold"}              → exclude segment "cold"
//
// All conditions are ANDed together. Subscribers missing a filtered key
// are excluded for include filters and included for exclude filters.
func matchAttribs(attribs models.JSON, filter map[string]any) bool {
	if attribs == nil {
		// No attribs: pass exclude-only filters, fail if any include filter exists.
		for k := range filter {
			if !strings.HasPrefix(k, "!") {
				return false
			}
		}
		return true
	}
	for k, v := range filter {
		negate := strings.HasPrefix(k, "!")
		key := k
		if negate {
			key = k[1:]
		}

		av, exists := attribs[key]
		avStr := fmt.Sprintf("%v", av)

		// Build list of values to match against.
		var vals []string
		switch vt := v.(type) {
		case []any:
			for _, item := range vt {
				vals = append(vals, fmt.Sprintf("%v", item))
			}
		default:
			vals = []string{fmt.Sprintf("%v", v)}
		}

		if negate {
			// Exclude: if attrib exists and matches any value, reject.
			if exists {
				for _, val := range vals {
					if avStr == val {
						return false
					}
				}
			}
		} else {
			// Include: attrib must exist and match one of the values.
			if !exists {
				return false
			}
			matched := false
			for _, val := range vals {
				if avStr == val {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		}
	}
	return true
}
