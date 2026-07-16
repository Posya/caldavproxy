package caldav

import (
	"time"

	"github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"
)

func eventAt(uid, summary string, start time.Time, opts ...func(*ical.Event)) *ical.Component {
	ev := ical.NewEvent()
	ev.Props.SetText(ical.PropUID, uid)
	ev.Props.SetDateTime(ical.PropDateTimeStamp, time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC))
	ev.Props.SetDateTime(ical.PropDateTimeStart, start)
	ev.Props.SetDateTime(ical.PropDateTimeEnd, start.Add(time.Hour))
	ev.Props.SetText(ical.PropSummary, summary)
	for _, opt := range opts {
		opt(ev)
	}
	return ev.Component
}

func withRRULE(rule string) func(*ical.Event) {
	return func(ev *ical.Event) {
		opt, err := rrule.StrToROption(rule)
		if err != nil {
			panic(err)
		}
		ev.Props.SetRecurrenceRule(opt)
	}
}

func withRecurrenceID(t time.Time) func(*ical.Event) {
	return func(ev *ical.Event) {
		ev.Props.SetDateTime(ical.PropRecurrenceID, t)
	}
}
