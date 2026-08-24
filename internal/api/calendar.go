package api

import (
	"net/http"
	"time"

	"github.com/mevdschee/webmail-server/internal/calendar"
)

// handleCalendar implements /api/v1/calendar/{entity}/{id}.
func (s *Server) handleCalendar(w http.ResponseWriter, r *http.Request, req *request) (any, int) {
	if !req.Identity.Authenticated() {
		return nil, http.StatusUnauthorized
	}
	customerID := req.Identity.CustomerID
	ctx := r.Context()

	switch req.Entity {
	case "calendar":
		if req.Method != http.MethodGet {
			return nil, http.StatusMethodNotAllowed
		}
		id := parseID(req.ID)
		if id == 0 {
			return nil, http.StatusBadRequest
		}
		if id != customerID {
			return nil, http.StatusNotFound
		}
		out, err := s.calendar.ListCalendars(ctx, customerID)
		return s.result(w, r, out, err)

	case "meeting":
		switch req.Method {
		case http.MethodGet:
			calendarIDs, ok := parseIDs(req.ID)
			if !ok {
				return nil, http.StatusBadRequest
			}
			from := time.Now().UTC().Truncate(24 * time.Hour)
			if v := req.str("fromDate"); v != "" {
				parsed, err := parseDate(v)
				if err != nil {
					return nil, http.StatusBadRequest
				}
				from = parsed
			}
			until := from.AddDate(0, 1, 0)
			if v := req.str("untilDate"); v != "" {
				parsed, err := parseDate(v)
				if err != nil {
					return nil, http.StatusBadRequest
				}
				until = parsed
			}
			if !until.After(from) {
				return nil, http.StatusBadRequest
			}
			out, err := s.calendar.ListMeetings(ctx, customerID, calendarIDs, from, until)
			return s.result(w, r, out, err)

		case http.MethodPost:
			calendarIDs, ok := parseIDs(req.ID)
			if !ok {
				return nil, http.StatusBadRequest
			}
			title := req.str("title")
			if title == "" {
				return nil, http.StatusBadRequest
			}
			startRaw, endRaw := req.str("startTime"), req.str("endTime")
			if startRaw == "" || endRaw == "" {
				return nil, http.StatusBadRequest
			}
			start, err := parseDateTime(startRaw)
			if err != nil {
				return nil, http.StatusBadRequest
			}
			end, err := parseDateTime(endRaw)
			if err != nil {
				return nil, http.StatusBadRequest
			}
			if end.Before(start) {
				return nil, http.StatusBadRequest
			}
			attendees, ok := req.strings("attendees")
			if !ok {
				return nil, http.StatusBadRequest
			}
			timeZone := req.str("timeZone")
			if timeZone == "" {
				timeZone = "UTC"
			}
			if _, err := time.LoadLocation(timeZone); err != nil {
				return nil, http.StatusBadRequest
			}
			out, err := s.calendar.AddMeeting(ctx, customerID, calendarIDs[0], calendar.NewMeeting{
				Attendees:       attendees,
				Title:           title,
				StartTime:       start,
				EndTime:         end,
				TimeZone:        timeZone,
				RepeatRule:      req.str("repeatRule"),
				DescriptionHTML: req.str("descriptionHtml"),
				IsAllDay:        req.boolean("isAllDay"),
			})
			if err != nil {
				// A malformed recurrence rule is the caller's mistake.
				if isBadInput(err) {
					return nil, http.StatusBadRequest
				}
				s.fail(w, r, err)
				return nil, 0
			}
			return out, http.StatusOK

		case http.MethodPut:
			meetingID := parseID(req.ID)
			if meetingID == 0 {
				return nil, http.StatusBadRequest
			}
			willAttend := req.strPtr("willAttend")
			if willAttend == nil {
				return true, http.StatusOK
			}
			switch *willAttend {
			case "yes", "no", "maybe":
			default:
				return nil, http.StatusBadRequest
			}
			out, err := s.calendar.SetRSVP(ctx, customerID, meetingID, *willAttend)
			return s.result(w, r, out, err)

		case http.MethodDelete:
			meetingID := parseID(req.ID)
			if meetingID == 0 {
				return nil, http.StatusBadRequest
			}
			out, err := s.calendar.DeleteMeeting(ctx, customerID, meetingID)
			return s.result(w, r, out, err)
		}
		return nil, http.StatusMethodNotAllowed

	case "attendee":
		meetingID := parseID(req.ID)
		if meetingID == 0 {
			return nil, http.StatusBadRequest
		}
		if req.Method != http.MethodPost {
			return nil, http.StatusMethodNotAllowed
		}
		address := req.str("email")
		if address == "" {
			return nil, http.StatusBadRequest
		}
		out, err := s.calendar.AddAttendee(ctx, customerID, meetingID, address)
		return s.result(w, r, out, err)

	case "document":
		id := parseID(req.ID)
		if id == 0 {
			return nil, http.StatusBadRequest
		}
		switch req.Method {
		case http.MethodGet:
			out, err := s.calendar.GetDocument(ctx, customerID, id)
			if err != nil {
				s.fail(w, r, err)
				return nil, 0
			}
			if out == nil {
				return nil, http.StatusNotFound
			}
			return out, http.StatusOK
		case http.MethodPost:
			fileName, mimeType := req.str("fileName"), req.str("mimeType")
			if fileName == "" || mimeType == "" {
				return nil, http.StatusBadRequest
			}
			data, err := decodeBase64(req.str("fileData"))
			if err != nil || len(data) == 0 {
				return nil, http.StatusBadRequest
			}
			out, err := s.calendar.AddDocument(ctx, customerID, id, fileName, mimeType, data)
			return s.result(w, r, out, err)
		case http.MethodDelete:
			out, err := s.calendar.DeleteDocument(ctx, customerID, id)
			return s.result(w, r, out, err)
		}
		return nil, http.StatusMethodNotAllowed
	}
	return nil, http.StatusNotFound
}

// parseDate reads a calendar day boundary.
func parseDate(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), nil
	}
	return time.ParseInLocation("2006-01-02", v, time.UTC)
}

// parseDateTime reads an event time, accepting RFC 3339 or a plain SQL
// datetime, which is what the PHP passed through strtotime.
func parseDateTime(v string) (time.Time, error) {
	layouts := []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"}
	var err error
	for _, layout := range layouts {
		var t time.Time
		t, err = time.ParseInLocation(layout, v, time.UTC)
		if err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, err
}
