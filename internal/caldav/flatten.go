package caldav

import (
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"
)

// maxExpandedOccurrences caps how many instances one RRULE may produce inside
// the feed window (safety against pathological rules).
const maxExpandedOccurrences = 366

// agendaPropNames are copied onto flattened agenda events. Attendees, alarms,
// and recurrence fields are intentionally omitted — this feed is for viewing.
var agendaPropNames = []string{
	ical.PropSummary,
	ical.PropDescription,
	ical.PropLocation,
	ical.PropURL,
	ical.PropCategories,
	ical.PropClass,
	ical.PropTransparency,
}

// flattenCalendar turns the merged calendar into a flat agenda: one VEVENT per
// visible occurrence in [start, end), with no RRULE/RECURRENCE-ID and times in UTC.
func flattenCalendar(cal *ical.Calendar, start, end time.Time) (kept, source int) {
	if cal == nil {
		return 0, 0
	}

	var (
		byUID     = make(map[string][]*ical.Component)
		uidOrder  []string
		todos     []*ical.Component
		uidless   []*ical.Component
		rawEvents int
	)

	for _, child := range cal.Children {
		switch child.Name {
		case ical.CompTimezone:
			// Flattened events use UTC; timezones are not needed.
			continue
		case ical.CompToDo:
			rawEvents++
			if flat := flattenSingle(child, start, end); flat != nil {
				todos = append(todos, flat)
			}
		case ical.CompEvent:
			rawEvents++
			uid := propValue(child.Props.Get(ical.PropUID))
			if uid == "" {
				if flat := flattenSingle(child, start, end); flat != nil {
					uidless = append(uidless, flat)
				}
				continue
			}
			if _, ok := byUID[uid]; !ok {
				uidOrder = append(uidOrder, uid)
			}
			byUID[uid] = append(byUID[uid], child)
		}
	}

	var events []*ical.Component
	for _, uid := range uidOrder {
		events = append(events, expandUIDGroup(uid, byUID[uid], start, end)...)
	}

	sort.SliceStable(events, func(i, j int) bool {
		ti, _ := componentTime(events[i], ical.PropDateTimeStart)
		tj, _ := componentTime(events[j], ical.PropDateTimeStart)
		return ti.Before(tj)
	})

	cal.Children = cal.Children[:0]
	cal.Children = append(cal.Children, events...)
	cal.Children = append(cal.Children, uidless...)
	cal.Children = append(cal.Children, todos...)

	kept = len(cal.Children)
	slog.Debug("flattened calendar to agenda",
		"windowStart", start,
		"windowEnd", end,
		"kept", kept,
		"sourceComponents", rawEvents)

	return kept, rawEvents
}

func expandUIDGroup(uid string, group []*ical.Component, start, end time.Time) []*ical.Component {
	var (
		master     *ical.Component
		exceptions []*ical.Component
		singles    []*ical.Component
	)

	for _, c := range group {
		hasRID := propValue(c.Props.Get(ical.PropRecurrenceID)) != ""
		hasRRULE := propValue(c.Props.Get(ical.PropRecurrenceRule)) != ""
		switch {
		case hasRID:
			exceptions = append(exceptions, c)
		case hasRRULE:
			if master == nil {
				master = c
			}
		default:
			singles = append(singles, c)
		}
	}

	if master == nil {
		out := make([]*ical.Component, 0, len(singles)+len(exceptions))
		for _, c := range singles {
			if flat := flattenSingle(c, start, end); flat != nil {
				out = append(out, flat)
			}
		}
		// Orphan overrides without a master: treat as one-off agenda items.
		for _, c := range exceptions {
			if flat := flattenSingle(c, start, end); flat != nil {
				out = append(out, flat)
			}
		}
		return out
	}

	excByOcc := indexExceptions(exceptions)
	duration := eventDuration(master)

	set, err := buildRecurrenceSet(master)
	if err != nil || set == nil {
		slog.Debug("rrule expand failed, falling back to overrides/singles",
			"uid", uid, "error", err)
		out := make([]*ical.Component, 0, len(exceptions)+len(singles)+1)
		// Still surface the master once if its DTSTART is in-window (rare).
		if flat := flattenSingle(master, start, end); flat != nil {
			out = append(out, flat)
		}
		for _, c := range exceptions {
			if flat := flattenSingle(c, start, end); flat != nil {
				out = append(out, flat)
			}
		}
		for _, c := range singles {
			if flat := flattenSingle(c, start, end); flat != nil {
				out = append(out, flat)
			}
		}
		return out
	}

	occs := set.Between(start, end, false)
	if len(occs) > maxExpandedOccurrences {
		occs = occs[:maxExpandedOccurrences]
	}

	out := make([]*ical.Component, 0, len(occs)+len(singles))
	seenOcc := make(map[int64]bool, len(occs))

	for _, occ := range occs {
		seenOcc[occ.Unix()] = true
		base := master
		occStart := occ
		dur := duration

		if exc, ok := excByOcc[occ.Unix()]; ok {
			if isCancelled(exc) {
				continue
			}
			base = exc
			if st, ok := componentTime(exc, ical.PropDateTimeStart); ok {
				occStart = st
			}
			dur = eventDuration(exc)
		} else if isCancelled(master) {
			continue
		}

		out = append(out, materializeAgendaEvent(base, uid, occStart, dur))
	}

	// Overrides whose RECURRENCE-ID was EXDATE'd from the rule but still fall
	// in-window (moved instances): include them if not already emitted.
	for _, exc := range exceptions {
		rid, ok := componentTime(exc, ical.PropRecurrenceID)
		if !ok || seenOcc[rid.Unix()] {
			continue
		}
		if isCancelled(exc) || !overlapsWindow(exc, start, end) {
			continue
		}
		st, ok := componentTime(exc, ical.PropDateTimeStart)
		if !ok {
			st = rid
		}
		out = append(out, materializeAgendaEvent(exc, uid, st, eventDuration(exc)))
	}

	for _, c := range singles {
		if flat := flattenSingle(c, start, end); flat != nil {
			out = append(out, flat)
		}
	}

	return out
}

func indexExceptions(exceptions []*ical.Component) map[int64]*ical.Component {
	out := make(map[int64]*ical.Component, len(exceptions))
	for _, exc := range exceptions {
		if t, ok := componentTime(exc, ical.PropRecurrenceID); ok {
			out[t.Unix()] = exc
		}
	}
	return out
}

func flattenSingle(comp *ical.Component, start, end time.Time) *ical.Component {
	if isCancelled(comp) || !overlapsWindow(comp, start, end) {
		return nil
	}
	st, ok := componentTime(comp, ical.PropDateTimeStart)
	if !ok {
		return nil
	}
	uid := propValue(comp.Props.Get(ical.PropUID))
	if uid == "" {
		uid = fmt.Sprintf("anon-%s", st.UTC().Format("20060102T150405Z"))
	}
	return materializeAgendaEvent(comp, uid, st, eventDuration(comp))
}

func materializeAgendaEvent(base *ical.Component, baseUID string, start time.Time, dur time.Duration) *ical.Component {
	start = start.UTC()
	if dur <= 0 {
		dur = time.Hour
	}
	end := start.Add(dur)

	ev := ical.NewComponent(base.Name)
	if ev.Name == "" {
		ev.Name = ical.CompEvent
	}

	for _, name := range agendaPropNames {
		copyPropValue(ev.Props, base.Props, name)
	}

	instanceUID := fmt.Sprintf("%s#%s", baseUID, start.Format("20060102T150405Z"))
	ev.Props.SetText(ical.PropUID, instanceUID)
	ev.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
	ev.Props.SetDateTime(ical.PropDateTimeStart, start)
	ev.Props.SetDateTime(ical.PropDateTimeEnd, end)

	return ev
}

func copyPropValue(dst, src ical.Props, name string) {
	p := src.Get(name)
	if p == nil || p.Value == "" {
		return
	}
	cp := ical.NewProp(name)
	cp.Value = p.Value
	if cp.Params == nil {
		cp.Params = ical.Params{}
	}
	for k, vals := range p.Params {
		for _, v := range vals {
			cp.Params.Add(k, v)
		}
	}
	dst.Set(cp)
}

func eventDuration(comp *ical.Component) time.Duration {
	st, ok := componentTime(comp, ical.PropDateTimeStart)
	if !ok {
		return time.Hour
	}
	if en, ok := componentTime(comp, ical.PropDateTimeEnd); ok && en.After(st) {
		return en.Sub(st)
	}
	if dur := propValue(comp.Props.Get(ical.PropDuration)); dur != "" {
		if d, err := parseICSDuration(dur); err == nil && d > 0 {
			return d
		}
	}
	// All-day DATE values are typically one day.
	if p := comp.Props.Get(ical.PropDateTimeStart); p != nil && len(p.Value) == len("20060102") {
		return 24 * time.Hour
	}
	return time.Hour
}

func isCancelled(comp *ical.Component) bool {
	return propValue(comp.Props.Get(ical.PropStatus)) == "CANCELLED"
}

// buildRecurrenceSet builds an rrule Set from a master VEVENT, including EXDATE/RDATE.
// It first tries go-ical's typed RRULE parser, then falls back to parsing the raw
// RRULE text (some producers omit the VALUE=RECUR type).
func buildRecurrenceSet(master *ical.Component) (*rrule.Set, error) {
	if set, err := master.RecurrenceSet(time.UTC); err == nil && set != nil {
		return set, nil
	}

	raw := propValue(master.Props.Get(ical.PropRecurrenceRule))
	if raw == "" {
		return nil, fmt.Errorf("missing RRULE")
	}

	opt, err := rrule.StrToROption(raw)
	if err != nil {
		return nil, fmt.Errorf("parse RRULE %q: %w", raw, err)
	}
	rule, err := rrule.NewRRule(*opt)
	if err != nil {
		return nil, fmt.Errorf("build RRULE: %w", err)
	}

	dtStart, ok := componentTime(master, ical.PropDateTimeStart)
	if !ok {
		return nil, fmt.Errorf("missing DTSTART")
	}

	set := &rrule.Set{}
	set.DTStart(dtStart)
	set.RRule(rule)

	for _, p := range master.Props[ical.PropExceptionDates] {
		if t, err := p.DateTime(time.UTC); err == nil {
			set.ExDate(t)
		} else if t, ok := parseRawICSTime(p.Value); ok {
			set.ExDate(t)
		}
	}
	for _, p := range master.Props[ical.PropRecurrenceDates] {
		if t, err := p.DateTime(time.UTC); err == nil {
			set.RDate(t)
		} else if t, ok := parseRawICSTime(p.Value); ok {
			set.RDate(t)
		}
	}

	return set, nil
}
